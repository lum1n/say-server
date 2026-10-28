package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/media"
	"github.com/lum1n/say-server/internal/store"
)

// maxAttachments bounds the files of one message.
const maxAttachments = 10

// urlTTL is how long a signed media URL works at least.
const urlTTL = 24 * time.Hour

func toAttachment(m store.Media) core.Attachment {
	return core.Attachment{Media: m.ID, Mime: m.Mime, Name: m.Name, Size: m.Size, Width: m.Width, Height: m.Height}
}

// AttachmentPart is the attachment part of an uploaded file.
func AttachmentPart(m store.Media) core.Part { return toAttachment(m).Part() }

// uploadMedia takes in a file to send to a room: POST
// /rooms/{id}/media?name=, with the file as the body and its type as the
// Content-Type.
func (s *Server) uploadMedia(w http.ResponseWriter, r *http.Request, me core.Principal) {
	roomID := r.PathValue("id")
	body := http.MaxBytesReader(w, r.Body, s.mediaMax+1)
	m, err := s.storeMedia(r.Context(), me, roomID, body, r.Header.Get("Content-Type"), r.URL.Query().Get("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	a := toAttachment(m)
	a.URL = s.mediaURL(m.ID)
	writeJSON(w, http.StatusCreated, a)
}

// storeMedia keeps a file from me for roomID. Members who can send can
// upload.
func (s *Server) storeMedia(ctx context.Context, me core.Principal, roomID string, r io.Reader, typ, name string) (store.Media, error) {
	m, err := s.member(ctx, me, roomID)
	if err != nil {
		return store.Media{}, err
	}
	if err := s.checkVisitor(ctx, me); err != nil {
		return store.Media{}, err
	}
	if m.Role == core.RoleObserver {
		return store.Media{}, errorf(http.StatusForbidden, "forbidden", "observers cannot send")
	}
	id := core.NewID()
	info, err := media.Ingest(ctx, s.media, id, r, typ, s.mediaMax)
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, media.ErrTooLarge) || errors.As(err, &mbe):
		return store.Media{}, errorf(http.StatusRequestEntityTooLarge, "too_large", "a file is at most %d MiB", s.mediaMax>>20)
	case errors.Is(err, media.ErrWrongType):
		return store.Media{}, errorf(http.StatusUnsupportedMediaType, "unsupported_media_type", "%v", strings.TrimPrefix(err.Error(), media.ErrWrongType.Error()+": "))
	case err != nil:
		return store.Media{}, err
	}
	if info.Size == 0 {
		s.media.Delete(context.WithoutCancel(ctx), id)
		return store.Media{}, errorf(http.StatusUnprocessableEntity, "invalid", "the file is empty")
	}
	rec := store.Media{
		ID: id, RoomID: roomID, UploaderID: me.ID, Mime: info.Mime, Name: fileName(name, info.Mime),
		Size: info.Size, Width: info.Width, Height: info.Height, SHA256: info.SHA256, CreatedAt: time.Now(),
	}
	if err := s.store.CreateMedia(ctx, rec); err != nil {
		s.media.Delete(context.WithoutCancel(ctx), id)
		return store.Media{}, err
	}
	return rec, nil
}

// fileName makes a name to show and to download as: the last element of
// name without control characters, or one made from the type.
func fileName(name, typ string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > 200 {
		name = strings.ToValidUTF8(name[:200], "")
	}
	if name == "" || name == "." || name == "/" {
		name = "file"
		if exts, _ := mime.ExtensionsByType(typ); len(exts) > 0 {
			name += exts[0]
		}
	}
	return name
}

// roomMedia serves a file of a room to its members.
func (s *Server) roomMedia(w http.ResponseWriter, r *http.Request, me core.Principal) {
	roomID := r.PathValue("id")
	if _, err := s.member(r.Context(), me, roomID); err != nil {
		writeError(w, err)
		return
	}
	m, err := s.store.GetMedia(r.Context(), r.PathValue("media_id"))
	if errors.Is(err, store.ErrNotFound) || err == nil && m.RoomID != roomID {
		writeError(w, errMediaNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.serveMedia(w, r, m, "private, max-age=31536000, immutable")
}

// signedMedia serves a file to whoever has its signed URL: browsers'
// <img>, and networks that fetch what a bridge links to.
func (s *Server) signedMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	key, err := s.mediaKey()
	if err != nil {
		writeError(w, err)
		return
	}
	if !media.Verify(key, id, q.Get("exp"), q.Get("sig")) {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "the link is not valid or has expired"))
		return
	}
	m, err := s.store.GetMedia(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errMediaNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.serveMedia(w, r, m, "private, max-age=3600")
}

var errMediaNotFound = errorf(http.StatusNotFound, "not_found", "no such file")

func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, m store.Media, cache string) {
	f, err := s.media.Open(r.Context(), m.ID)
	if errors.Is(err, media.ErrNotFound) {
		writeError(w, errMediaNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", m.Mime)
	h.Set("X-Content-Type-Options", "nosniff")
	// Nothing say serves from here may run as a page on its origin.
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", cache)
	h.Set("ETag", `"`+m.SHA256+`"`)
	disposition := "attachment"
	if media.Images[m.Mime] {
		disposition = "inline"
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": m.Name}))
	http.ServeContent(w, r, "", m.CreatedAt, f)
}

// mediaKey is the key that signs media URLs, the same on every node.
func (s *Server) mediaKey() ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.signKey == nil {
		k, err := s.store.InstanceKey(context.Background(), "media")
		if err != nil {
			return nil, err
		}
		s.signKey = k
	}
	return s.signKey, nil
}

// mediaURL is a signed URL for media id, relative to the node.
func (s *Server) mediaURL(id string) string {
	key, err := s.mediaKey()
	if err != nil {
		slog.Warn("media key unavailable", "err", err)
		return ""
	}
	return "/api/v1/media/" + id + "?" + media.Sign(key, id, media.Expiry(time.Now(), urlTTL))
}

// withURLs gives each attachment part a signed URL.
func (s *Server) withURLs(parts []wirePart) []wirePart {
	for i, p := range parts {
		if p.Kind != core.PartAttachment {
			continue
		}
		var a core.Attachment
		if json.Unmarshal(p.Data, &a) != nil || a.Media == "" {
			continue
		}
		if a.URL = s.mediaURL(a.Media); a.URL == "" {
			continue
		}
		data, err := json.Marshal(a)
		if err != nil {
			continue
		}
		parts[i].Data = data
	}
	return parts
}

// attachments checks a message's attachment parts: each names a file its
// sender uploaded to the room, and its data is then what say knows of the
// file, whatever the client wrote.
func (s *Server) attachments(ctx context.Context, me core.Principal, roomID string, parts []core.Part) ([]core.Part, error) {
	n := 0
	for i, p := range parts {
		if p.Kind != core.PartAttachment {
			continue
		}
		if n++; n > maxAttachments {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "a message has at most %d attachments", maxAttachments)
		}
		var a core.Attachment
		if json.Unmarshal(p.Data, &a) != nil || a.Media == "" {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "part %d: an attachment part needs a \"media\" id", i)
		}
		m, err := s.store.GetMedia(ctx, a.Media)
		if errors.Is(err, store.ErrNotFound) || err == nil && (m.RoomID != roomID || m.UploaderID != me.ID) {
			return nil, errorf(http.StatusUnprocessableEntity, "invalid", "part %d: %s is not a file you uploaded to this room", i, a.Media)
		}
		if err != nil {
			return nil, err
		}
		parts[i] = AttachmentPart(m)
	}
	return parts, nil
}

// Upload keeps a file from as for roomID and returns its attachment part,
// for bridges taking in a network's media.
func (s *Server) Upload(ctx context.Context, as core.Principal, roomID string, r io.Reader, typ, name string) (core.Part, error) {
	m, err := s.storeMedia(ctx, as, roomID, r, typ, name)
	if err != nil {
		return core.Part{}, err
	}
	return AttachmentPart(m), nil
}

// OpenMedia opens a file to send it on, for bridges.
func (s *Server) OpenMedia(ctx context.Context, id string) (io.ReadCloser, error) {
	f, err := s.media.Open(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return nil, fmt.Errorf("media %s: %w", id, store.ErrNotFound)
	}
	return f, err
}
