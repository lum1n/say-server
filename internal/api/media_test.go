package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/media"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// upload posts data to the room's media and returns the status and the
// attachment.
func (e *env) upload(token, roomID, name, typ string, data []byte) (int, core.Attachment) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/rooms/"+roomID+"/media?name="+name, bytes.NewReader(data))
	req.Header.Set("Content-Type", typ)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var a core.Attachment
	json.NewDecoder(res.Body).Decode(&a)
	return res.StatusCode, a
}

func (e *env) fetch(path, token string, header ...string) (int, []byte, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body, res.Header
}

func attachmentMsg(text string, as ...core.Attachment) map[string]any {
	var parts []map[string]any
	if text != "" {
		parts = append(parts, map[string]any{"kind": "text", "data": map[string]string{"text": text}})
	}
	for _, a := range as {
		parts = append(parts, map[string]any{"kind": "attachment", "data": map[string]string{"media": a.Media}})
	}
	return map[string]any{"parts": parts}
}

func eventAttachments(t *testing.T, ev wireEvent) []core.Attachment {
	t.Helper()
	var out []core.Attachment
	for _, p := range ev.Parts {
		if p.Kind == core.PartAttachment {
			var a core.Attachment
			if err := json.Unmarshal(p.Data, &a); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
	}
	return out
}

func TestMedia(t *testing.T) {
	e := newEnv(t)
	ada, adaTok := e.user("Ada", core.PrincipalHuman)
	bob, bobTok := e.user("Bob", core.PrincipalHuman)
	_, eveTok := e.user("Eve", core.PrincipalHuman)
	room := e.room(adaTok, "pics")
	e.join(adaTok, room.ID, bob.ID, core.RoleMember)
	other := e.room(adaTok, "other")
	_ = ada

	img := pngBytes(t, 40, 30)
	code, a := e.upload(adaTok, room.ID, "dot.png", "image/png", img)
	if code != 201 || a.Media == "" || a.Mime != "image/png" || a.Name != "dot.png" || a.Size != int64(len(img)) || a.Width != 40 || a.Height != 30 || a.URL == "" {
		t.Fatalf("upload = %d %+v", code, a)
	}

	t.Run("type must match", func(t *testing.T) {
		if code, _ := e.upload(adaTok, room.ID, "x.jpg", "image/jpeg", img); code != 415 {
			t.Errorf("png as jpeg = %d, want 415", code)
		}
		if code, _ := e.upload(adaTok, room.ID, "x.html", "text/html", []byte("<script>")); code != 415 {
			t.Errorf("html = %d, want 415", code)
		}
		if code, f := e.upload(adaTok, room.ID, "notes.txt", "text/plain", []byte("hello")); code != 201 || f.Width != 0 {
			t.Errorf("text file = %d %+v", code, f)
		}
	})
	t.Run("outsiders can't upload", func(t *testing.T) {
		if code, _ := e.upload(eveTok, room.ID, "dot.png", "image/png", img); code != 404 && code != 403 {
			t.Errorf("non-member upload = %d", code)
		}
	})

	// A message of only a file; the data comes from the upload, not the
	// client.
	var ev wireEvent
	body := attachmentMsg("", a)
	body["parts"].([]map[string]any)[0]["data"] = map[string]any{"media": a.Media, "name": "lie.exe", "size": 1}
	if code := e.do("POST", "/rooms/"+room.ID+"/messages", adaTok, body, &ev); code != 201 {
		t.Fatalf("send = %d", code)
	}
	got := eventAttachments(t, ev)
	if len(got) != 1 || got[0].Name != "dot.png" || got[0].Size != int64(len(img)) || got[0].URL == "" {
		t.Fatalf("sent attachments = %+v", got)
	}

	t.Run("pages carry signed urls", func(t *testing.T) {
		var page struct{ Events []wireEvent }
		e.do("GET", "/rooms/"+room.ID+"/events", bobTok, nil, &page)
		as := eventAttachments(t, page.Events[len(page.Events)-1])
		if len(as) != 1 || !strings.HasPrefix(as[0].URL, "/api/v1/media/"+a.Media+"?") {
			t.Fatalf("page attachments = %+v", as)
		}
		code, data, h := e.fetch(as[0].URL, "")
		if code != 200 || !bytes.Equal(data, img) || h.Get("Content-Type") != "image/png" || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") {
			t.Fatalf("signed fetch = %d %d bytes %v", code, len(data), h)
		}
	})
	t.Run("members fetch with a token", func(t *testing.T) {
		code, data, _ := e.fetch("/api/v1/rooms/"+room.ID+"/media/"+a.Media, bobTok)
		if code != 200 || !bytes.Equal(data, img) {
			t.Fatalf("member fetch = %d", code)
		}
		code, data, _ = e.fetch("/api/v1/rooms/"+room.ID+"/media/"+a.Media, bobTok, "Range", "bytes=0-3")
		if code != 206 || !bytes.Equal(data, img[:4]) {
			t.Fatalf("range fetch = %d %q", code, data)
		}
		if code, _, _ := e.fetch("/api/v1/rooms/"+room.ID+"/media/"+a.Media, eveTok); code != 404 && code != 403 {
			t.Errorf("non-member fetch = %d", code)
		}
		if code, _, _ := e.fetch("/api/v1/rooms/"+other.ID+"/media/"+a.Media, adaTok); code != 404 {
			t.Errorf("fetch through another room = %d, want 404", code)
		}
	})
	t.Run("files belong to their room and uploader", func(t *testing.T) {
		if code := e.do("POST", "/rooms/"+other.ID+"/messages", adaTok, attachmentMsg("", a), nil); code != 422 {
			t.Errorf("file from another room = %d, want 422", code)
		}
		if code := e.do("POST", "/rooms/"+room.ID+"/messages", bobTok, attachmentMsg("mine now", a), nil); code != 422 {
			t.Errorf("someone else's file = %d, want 422", code)
		}
		if code := e.do("POST", "/rooms/"+room.ID+"/messages", adaTok, attachmentMsg("", core.Attachment{Media: "nope"}), nil); code != 422 {
			t.Errorf("unknown file = %d, want 422", code)
		}
		many := make([]core.Attachment, maxAttachments+1)
		for i := range many {
			many[i] = a
		}
		if code := e.do("POST", "/rooms/"+room.ID+"/messages", adaTok, attachmentMsg("", many...), nil); code != 422 {
			t.Errorf("%d files = %d, want 422", len(many), code)
		}
	})
	t.Run("signed urls expire", func(t *testing.T) {
		key, err := e.srv.mediaKey()
		if err != nil {
			t.Fatal(err)
		}
		past := "/api/v1/media/" + a.Media + "?" + media.Sign(key, a.Media, time.Now().Add(-time.Minute))
		if code, _, _ := e.fetch(past, ""); code != 403 {
			t.Errorf("expired url = %d, want 403", code)
		}
		forged := "/api/v1/media/" + a.Media + "?" + media.Sign([]byte("not the key"), a.Media, time.Now().Add(time.Hour))
		if code, _, _ := e.fetch(forged, ""); code != 403 {
			t.Errorf("forged url = %d, want 403", code)
		}
		otherID := strings.Replace(a.URL, a.Media, core.NewID(), 1)
		if code, _, _ := e.fetch(otherID, ""); code != 403 {
			t.Errorf("url for another id = %d, want 403", code)
		}
	})
}

func TestMediaTooLarge(t *testing.T) {
	e := newEnv(t)
	e.srv.mediaMax = 100
	_, tok := e.user("Ada", core.PrincipalHuman)
	room := e.room(tok, "big")
	if code, _ := e.upload(tok, room.ID, "big.txt", "text/plain", bytes.Repeat([]byte("a"), 101)); code != 413 {
		t.Errorf("101 bytes = %d, want 413", code)
	}
	if code, _ := e.upload(tok, room.ID, "ok.txt", "text/plain", bytes.Repeat([]byte("a"), 100)); code != 201 {
		t.Errorf("100 bytes = %d, want 201", code)
	}
}
