package matrix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/bridge"
)

// TestHomeserver runs the driver against a real homeserver that allows
// open registration, such as
//
//	docker run -d --name say-test-matrix-conduit -p 127.0.0.1:18961:6167 \
//	  -e CONDUIT_SERVER_NAME=localhost -e CONDUIT_DATABASE_BACKEND=rocksdb \
//	  -e CONDUIT_DATABASE_PATH=/var/lib/matrix-conduit/ -e CONDUIT_ALLOW_REGISTRATION=true \
//	  -e CONDUIT_ALLOW_FEDERATION=false -e CONDUIT_PORT=6167 -e CONDUIT_ADDRESS=0.0.0.0 \
//	  -e CONDUIT_CONFIG='' matrixconduit/matrix-conduit:latest
//	SAY_TEST_MATRIX=http://127.0.0.1:18961 go test ./internal/bridge/matrix/ -run Homeserver
func TestHomeserver(t *testing.T) {
	hs := os.Getenv("SAY_TEST_MATRIX")
	if hs == "" {
		t.Skip("SAY_TEST_MATRIX is not set")
	}
	suffix := randomHex()
	ada := register(t, hs, "ada"+suffix)
	bob := register(t, hs, "bob"+suffix)

	// Bob starts a direct chat with Ada and says something before the
	// bridge runs.
	var created struct {
		RoomID string `json:"room_id"`
	}
	bob.call(t, http.MethodPost, path("createRoom"), map[string]any{"invite": []string{ada.userID}, "is_direct": true, "preset": "trusted_private_chat"}, &created)
	roomID := created.RoomID
	ada.call(t, http.MethodPost, path("join", roomID), map[string]any{}, nil)
	before := bob.send(t, roomID, "b1", map[string]any{"msgtype": "m.text", "body": "before the bridge"})

	h := newFakeHost()
	c, done := start(t, map[string]any{"homeserver": hs, "user_id": ada.userID, "password": "password-" + suffix}, nil, h)
	connected(t, h, done)

	m := h.waitFor(t, "the backfill", func(m bridge.Incoming) bool { return m.ID == before })
	if m.Text != "before the bridge" || m.Author.ID != bob.userID || m.Author.Self || m.Chat.ID != roomID || !m.Chat.Direct {
		t.Errorf("backfill: %+v", m)
	}
	// Named for Bob, by his display name (Conduit's default adds an emoji).
	if !strings.HasPrefix(m.Chat.Name, localpart(bob.userID)) || m.Author.Name != m.Chat.Name {
		t.Errorf("chat name %q, author %q; want Bob's name", m.Chat.Name, m.Author.Name)
	}

	live := bob.send(t, roomID, "b2", map[string]any{"msgtype": "m.text", "body": "live"})
	h.waitFor(t, "a live message", func(m bridge.Incoming) bool { return m.ID == live && m.Text == "live" })

	bob.send(t, roomID, "b3", map[string]any{
		"msgtype": "m.text", "body": "* live, edited",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "live, edited"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": live},
	})
	h.waitFor(t, "Bob's edit", func(m bridge.Incoming) bool { return m.Edit && m.ID == live && m.Text == "live, edited" })

	// Ada writes in say.
	ctx := context.Background()
	id, err := c.Send(ctx, bridge.Outgoing{Chat: roomID, EventID: "say-" + suffix, Text: "from say"})
	if err != nil {
		t.Fatal(err)
	}
	echo := h.waitFor(t, "the echo", func(m bridge.Incoming) bool { return m.ID == id })
	if !echo.Author.Self || echo.Text != "from say" {
		t.Errorf("echo: %+v", echo)
	}
	if again, err := c.Send(ctx, bridge.Outgoing{Chat: roomID, EventID: "say-" + suffix, Text: "from say"}); err != nil || again != id {
		t.Errorf("the same send again: %q, %v; want %q", again, err, id)
	}
	if err := c.Edit(ctx, bridge.Outgoing{Chat: roomID, EventID: "say-" + suffix, Text: "from say, edited"}, id); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "the edit's echo", func(m bridge.Incoming) bool { return m.Edit && m.ID == id && m.Text == "from say, edited" })

	// Bob sees both.
	var msgs struct {
		Chunk []event `json:"chunk"`
	}
	bob.call(t, http.MethodGet, path("rooms", roomID, "messages")+"?dir=b&limit=20", nil, &msgs)
	var sawSend, sawEdit bool
	for _, e := range msgs.Chunk {
		var cn content
		json.Unmarshal(e.Content, &cn)
		if e.ID == id && cn.Body == "from say" && e.Sender == ada.userID {
			sawSend = true
		}
		if cn.RelatesTo != nil && cn.RelatesTo.RelType == "m.replace" && cn.RelatesTo.EventID == id && cn.NewContent != nil && cn.NewContent.Body == "from say, edited" {
			sawEdit = true
		}
	}
	if !sawSend || !sawEdit {
		t.Errorf("Bob saw the send %v and the edit %v", sawSend, sawEdit)
	}

	// A run resuming from the saved state looks up a room it did not
	// sync in full, and logs in no more.
	bob.call(t, http.MethodPost, path("createRoom"), map[string]any{"invite": []string{ada.userID}, "name": "Plans " + suffix}, &created)
	ada.call(t, http.MethodPost, path("join", created.RoomID), map[string]any{}, nil)
	h2 := newFakeHost()
	_, done2 := start(t, map[string]any{"homeserver": hs, "user_id": ada.userID, "password": "password-" + suffix}, h.stateJSON(), h2)
	connected(t, h2, done2)
	later := bob.send(t, created.RoomID, "b4", map[string]any{"msgtype": "m.text", "body": "after resuming"})
	m = h2.waitFor(t, "a message after resuming", func(m bridge.Incoming) bool { return m.ID == later })
	if m.Chat.Name != "Plans "+suffix || m.Author.ID != bob.userID {
		t.Errorf("after resuming: %+v", m)
	}
	for _, m := range h2.messages() {
		if m.ID == before {
			t.Errorf("the backfill came again after resuming")
		}
	}
	if st := h2.saved(); st.AccessToken != h.saved().AccessToken {
		t.Errorf("logged in again after resuming")
	}
}

type account struct {
	hs, userID, token string
}

func randomHex() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func register(t *testing.T, hs, name string) *account {
	t.Helper()
	a := &account{hs: hs}
	var resp struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
	}
	a.call(t, http.MethodPost, path("register"), map[string]any{
		"username": name, "password": "password-" + name[3:], "auth": map[string]string{"type": "m.login.dummy"},
	}, &resp)
	a.userID, a.token = resp.UserID, resp.AccessToken
	return a
}

func (a *account) send(t *testing.T, roomID, txn string, cn map[string]any) string {
	t.Helper()
	var resp struct {
		EventID string `json:"event_id"`
	}
	a.call(t, http.MethodPut, path("rooms", roomID, "send", "m.room.message", txn), cn, &resp)
	return resp.EventID
}

func (a *account) call(t *testing.T, method, p string, body, out any) {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	} else {
		r = bytes.NewReader(nil)
	}
	u, err := url.Parse(a.hs + p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, u.String(), r)
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e apiError
		json.NewDecoder(resp.Body).Decode(&e)
		t.Fatalf("%s %s: %d %s %s", method, p, resp.StatusCode, e.Code, e.Message)
	}
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
}
