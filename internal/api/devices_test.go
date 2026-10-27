package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lum1n/say-server/internal/core"
)

// adaWithDevice sets the instance up with ada and pairs a device for her
// with her password.
func adaWithDevice(t *testing.T, e *env, name string) (wirePrincipal, string) {
	t.Helper()
	e.srv.setupCode = "c"
	if res, data := e.raw("POST", "/setup", map[string]string{"code": "c", "username": "ada", "password": "correct horse"}, nil); res.StatusCode != 201 {
		t.Fatalf("setup = %d %s", res.StatusCode, data)
	}
	return pairWith(t, e, map[string]string{"username": "ada", "password": "correct horse", "name": name})
}

func pairWith(t *testing.T, e *env, body map[string]string) (wirePrincipal, string) {
	t.Helper()
	res, data := e.raw("POST", "/devices", body, nil)
	var d struct {
		Principal wirePrincipal
		Token     string
	}
	if json.Unmarshal(data, &d); res.StatusCode != 201 || d.Token == "" {
		t.Fatalf("pair %v = %d %s", body, res.StatusCode, data)
	}
	return d.Principal, d.Token
}

func TestPairingCode(t *testing.T) {
	e := newEnv(t)
	ada, laptop := adaWithDevice(t, e, "laptop")

	var p struct {
		Code      string
		ExpiresAt time.Time `json:"expires_at"`
	}
	if code := e.do("POST", "/pairings", laptop, nil, &p); code != 201 || len(p.Code) != 11 ||
		p.ExpiresAt.Before(time.Now().Add(9*time.Minute)) || p.ExpiresAt.After(time.Now().Add(11*time.Minute)) {
		t.Fatalf("POST /pairings = %d %+v", code, p)
	}
	if res, data := e.raw("POST", "/devices", map[string]string{"pairing_code": p.Code, "username": "ada", "password": "correct horse", "name": "phone"}, nil); res.StatusCode != 422 {
		t.Fatalf("a code and a password = %d %s", res.StatusCode, data)
	}
	who, phone := pairWith(t, e, map[string]string{"pairing_code": p.Code, "name": "phone"})
	if who.ID != ada.ID {
		t.Fatalf("paired as %+v", who)
	}
	var me wirePrincipal
	if code := e.do("GET", "/me", phone, nil, &me); code != 200 || me.ID != ada.ID {
		t.Fatalf("the paired phone: %d %+v", code, me)
	}
	if res, data := e.raw("POST", "/devices", map[string]string{"pairing_code": p.Code, "name": "tablet"}, nil); res.StatusCode != 401 || errCode(data) != "invalid_pairing_code" {
		t.Fatalf("the code again = %d %s", res.StatusCode, data)
	}

	_, bot := e.user("Bot", core.PrincipalAgent)
	if code := e.do("POST", "/pairings", bot, nil, nil); code != 403 {
		t.Fatalf("an agent's pairing = %d", code)
	}
	if code := e.do("POST", "/pairings", "", nil, nil); code != 401 {
		t.Fatalf("pairing signed out = %d", code)
	}
}

type tokenList struct {
	Tokens []struct {
		ID, Name  string
		UserAgent string `json:"user_agent"`
		Current   bool
		ExpiresAt *time.Time `json:"expires_at"`
	}
}

func TestTokenManagement(t *testing.T) {
	e := newEnv(t)
	ada, laptop := adaWithDevice(t, e, "laptop")
	_, phone := pairWith(t, e, map[string]string{"username": "ada", "password": "correct horse", "name": "phone"})
	bob, bobTok := e.user("Bob", core.PrincipalHuman)

	var l tokenList
	if code := e.do("GET", "/principals/"+ada.ID+"/tokens", laptop, nil, &l, "User-Agent", "test-agent"); code != 200 {
		t.Fatalf("GET tokens = %d", code)
	}
	// The setup session, and the two devices.
	byName := map[string]int{}
	for i, tk := range l.Tokens {
		byName[tk.Name] = i
	}
	lap, ph, sess := l.Tokens[byName["device:laptop"]], l.Tokens[byName["device:phone"]], l.Tokens[byName["session"]]
	if len(l.Tokens) != 3 || !lap.Current || ph.Current || sess.ExpiresAt == nil || lap.ExpiresAt != nil || ph.UserAgent == "" {
		t.Fatalf("tokens = %+v", l.Tokens)
	}

	if code := e.do("GET", "/principals/"+ada.ID+"/tokens", bobTok, nil, nil); code != 403 {
		t.Fatalf("bob lists ada's tokens = %d", code)
	}
	if code := e.do("DELETE", "/principals/"+ada.ID+"/tokens/"+ph.ID, bobTok, nil, nil); code != 403 {
		t.Fatalf("bob revokes ada's phone = %d", code)
	}
	if code := e.do("DELETE", "/principals/"+bob.ID+"/tokens/"+ph.ID, bobTok, nil, nil); code != 404 {
		t.Fatalf("revoking ada's token as one of bob's = %d", code)
	}
	if code := e.do("DELETE", "/principals/"+ada.ID+"/tokens/"+ph.ID, laptop, nil, nil); code != 204 {
		t.Fatalf("ada revokes her phone = %d", code)
	}
	if code := e.do("GET", "/me", phone, nil, nil); code != 401 {
		t.Fatalf("the revoked phone = %d", code)
	}

	// Ada is the admin: she manages bob's tokens too.
	if code := e.do("GET", "/principals/"+bob.ID+"/tokens", laptop, nil, &l); code != 200 || len(l.Tokens) != 1 || l.Tokens[0].Current {
		t.Fatalf("ada lists bob's tokens = %d %+v", code, l.Tokens)
	}
	if code := e.do("DELETE", "/principals/"+bob.ID+"/tokens/"+l.Tokens[0].ID, laptop, nil, nil); code != 204 {
		t.Fatalf("ada revokes bob's token = %d", code)
	}
	if code := e.do("GET", "/me", bobTok, nil, nil); code != 401 {
		t.Fatalf("bob's revoked token = %d", code)
	}
	if code := e.do("GET", "/principals/nobody/tokens", laptop, nil, nil); code != 404 {
		t.Fatalf("an unknown principal's tokens = %d", code)
	}
}

// Revoking a device closes its WebSocket and its event streams.
func TestRevokeClosesConnections(t *testing.T) {
	e := newEnv(t)
	e.srv.heartbeat = 50 * time.Millisecond
	ada, laptop := adaWithDevice(t, e, "laptop")
	_, phone := pairWith(t, e, map[string]string{"username": "ada", "password": "correct horse", "name": "phone"})
	r := e.room(laptop, "general")

	ws := dialWS(t, e, phone)
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/rooms/"+r.ID+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+phone)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("stream = %v, %v", res, err)
	}
	defer res.Body.Close()
	streamDone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, res.Body)
		close(streamDone)
	}()

	var l tokenList
	e.do("GET", "/principals/"+ada.ID+"/tokens", laptop, nil, &l)
	for _, tk := range l.Tokens {
		if tk.Name == "device:phone" {
			e.do("DELETE", "/principals/"+ada.ID+"/tokens/"+tk.ID, laptop, nil, nil)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err = ws.Read(ctx); err != nil {
			break
		}
	}
	if websocket.CloseStatus(err) != StatusSignedOut {
		t.Fatalf("the phone's socket ended with %v", err)
	}
	select {
	case <-streamDone:
	case <-ctx.Done():
		t.Fatal("the phone's event stream is still open")
	}
	// The laptop stays connected.
	other := dialWS(t, e, laptop)
	wsSend(t, other, "1", "subscribe", map[string]any{"room_id": r.ID, "after": 0})
	if env := wsRead(t, other); env.ID != "1" {
		t.Fatalf("the laptop's socket: %+v", env)
	}
}
