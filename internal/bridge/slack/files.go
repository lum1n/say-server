package slack

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/media"
)

// maxUploaded bounds the files say uploaded that are remembered, to know
// the message that shares them as its own.
const maxUploaded = 200

// media splits a message's files into those say takes in and those named
// in its text: files kept outside Slack, hidden or deleted ones, and ones
// larger than say keeps.
func (c *conn) media(m message) ([]bridge.Media, []file) {
	var in []bridge.Media
	var named []file
	for _, f := range m.Files {
		u := f.URLPrivateDownload
		if u == "" || f.Mode == "external" || f.Mode == "tombstone" || f.Mode == "hidden_by_limit" || f.Size > media.DefaultMax {
			named = append(named, f)
			continue
		}
		in = append(in, bridge.Media{
			Mime: f.Mimetype, Name: cmp.Or(f.Name, f.Title, "file"), Size: f.Size, Width: f.OriginalW, Height: f.OriginalH,
			Open: func(ctx context.Context) (io.ReadCloser, error) { return c.download(ctx, u) },
		})
	}
	return in, named
}

// download reads a file of a message. Slack serves files to the workspace
// only, so the request carries the user token; it goes to Slack's file
// host alone.
func (c *conn) download(ctx context.Context, u string) (io.ReadCloser, error) {
	if !strings.HasPrefix(u, c.files) {
		return nil, fmt.Errorf("slack: a file at %s, not on Slack's file host", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.UserToken)
	resp, err := c.fileClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("slack: download: HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		// Slack's sign-in page, for a token without files:read.
		resp.Body.Close()
		return nil, errors.New("slack: download: the file was not served; the app needs the files:read scope")
	}
	return resp.Body, nil
}

// fileClient is the HTTP client for files: the API client without its
// timeout, which a large file outlasts; the send's context bounds it.
func (c *conn) fileClient() *http.Client {
	cl := *c.http
	cl.Timeout = 0
	return &cl
}

// sharedFile is a file as files.completeUploadExternal and files.info
// describe it.
type sharedFile struct {
	ID     string `json:"id"`
	Shares struct {
		Public  map[string][]struct{ Ts string } `json:"public"`
		Private map[string][]struct{ Ts string } `json:"private"`
	} `json:"shares"`
}

// ts returns the ts of the message that shares the file in chat, or "".
func (f sharedFile) ts(chat string) string {
	for _, s := range [][]struct{ Ts string }{f.Shares.Public[chat], f.Shares.Private[chat]} {
		if len(s) > 0 {
			return s[0].Ts
		}
	}
	return ""
}

// sendFiles uploads a message's files and shares them in one message,
// with the text as its comment, through Slack's external upload flow.
//
// Slack posts the message a moment after the upload completes, so its ts
// is looked up with files.info, and is "" if it does not show in time.
// The files are noted before they are shared: the message's echo, which
// may come before Send returns, is dropped by them.
func (c *conn) sendFiles(ctx context.Context, m bridge.Outgoing) (string, error) {
	type ref struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	var refs []ref
	for _, f := range m.Media {
		id, err := c.upload(ctx, f)
		if err != nil {
			// Nothing is shared before the upload completes.
			return "", bridge.Rejected(err)
		}
		refs = append(refs, ref{id, cmp.Or(f.Name, "file")})
		c.uploaded(id)
	}
	files, _ := json.Marshal(refs)
	p := url.Values{"files": {string(files)}, "channel_id": {m.Chat}}
	if m.Text != "" {
		p.Set("initial_comment", escape(m.Text))
	}
	if m.ReplyTo != "" {
		p.Set("thread_ts", m.ReplyTo)
	}
	var r struct {
		response
		Files []sharedFile `json:"files"`
	}
	err := c.call(ctx, "files.completeUploadExternal", c.cfg.UserToken, p, &r)
	if _, ok := errors.AsType[*apiError](err); ok {
		return "", bridge.Rejected(err)
	}
	if err != nil {
		return "", err
	}
	if len(r.Files) > 0 {
		if ts := r.Files[0].ts(m.Chat); ts != "" {
			return ts, nil
		}
	}
	return c.shared(ctx, refs[0].ID, m.Chat), nil
}

// upload sends a file's bytes to Slack and returns its id, not yet shared.
func (c *conn) upload(ctx context.Context, f bridge.Media) (string, error) {
	r, err := f.Open(ctx)
	if err != nil {
		return "", err
	}
	// Slack wants the length before the bytes.
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return "", err
	}
	var u struct {
		response
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	p := url.Values{"filename": {cmp.Or(f.Name, "file")}, "length": {strconv.Itoa(len(data))}}
	if err := c.call(ctx, "files.getUploadURLExternal", c.cfg.UserToken, p, &u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", cmp.Or(f.Mime, "application/octet-stream"))
	resp, err := c.fileClient().Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("slack: upload: HTTP %d", resp.StatusCode)
	}
	return u.FileID, nil
}

// shared asks for the ts of the message that shares file id in chat a few
// times, waiting longer each time; "" if Slack does not tell in time.
func (c *conn) shared(ctx context.Context, id, chat string) string {
	wait := c.sharePoll
	for range 5 {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(wait):
		}
		var r struct {
			response
			File sharedFile `json:"file"`
		}
		if err := c.call(ctx, "files.info", c.cfg.UserToken, url.Values{"file": {id}}, &r); err == nil {
			if ts := r.File.ts(chat); ts != "" {
				return ts
			}
		}
		wait *= 2
	}
	return ""
}

// uploaded notes a file say uploaded.
func (c *conn) uploaded(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ours = append(c.ours, id)
	if n := len(c.ours) - maxUploaded; n > 0 {
		c.ours = append([]string(nil), c.ours[n:]...)
	}
}

// fromSay reports whether a message shares files say uploaded: it is the
// echo of one say sent.
func (c *conn) fromSay(m message) bool {
	if len(m.Files) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range m.Files {
		if !slices.Contains(c.ours, f.ID) {
			return false
		}
	}
	return true
}
