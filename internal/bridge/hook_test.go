package bridge_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lum1n/say-server/internal/api"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"

	_ "github.com/lum1n/say-server/internal/bridge/webhook"
)

func TestWebhookBridge(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// The program on the other side.
	var mu sync.Mutex
	var posted []map[string]any
	var sigOK bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("sign"))
		mac.Write(body)
		var m map[string]any
		json.Unmarshal(body, &m)
		mu.Lock()
		posted = append(posted, m)
		sigOK = r.Header.Get("X-Say-Signature") == "sha256="+hex.EncodeToString(mac.Sum(nil))
		mu.Unlock()
		w.Write([]byte(`{"id": 42}`))
	}))
	defer remote.Close()

	var b wireBridge
	body := map[string]any{"network": "webhook", "mode": "bot", "config": map[string]any{"token": "s3cret", "url": remote.URL, "signing_secret": "sign"}}
	if code := e.call("POST", "/api/v1/bridges", body, &b); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var room struct{ ID string }
	e.call("POST", "/api/v1/rooms", map[string]string{"kind": "group", "name": "Ops"}, &room)
	var link struct{ ID string }
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/links", map[string]string{"room_id": room.ID, "remote_id": "alerts"}, &link); code != http.StatusCreated {
		t.Fatalf("link: %d", code)
	}

	hook := func(auth, query, body string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", e.http.URL+"/api/v1/hooks/"+b.ID+query, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// Messages come in without the bridge running: any node takes them.
	if code := hook("wrong", "", `{"chat":"alerts","text":"x"}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", code)
	}
	if code := hook("s3cret", "", `{"chat":"nowhere","text":"x"}`); code != http.StatusNotFound {
		t.Fatalf("unlinked chat: %d", code)
	}
	if code := hook("s3cret", "", `{"chat":"alerts"}`); code != http.StatusBadRequest {
		t.Fatalf("no text: %d", code)
	}
	if code := hook("s3cret", "", `{"chat":"alerts","id":"a1","text":"disk full","username":"Grafana"}`); code != http.StatusNoContent {
		t.Fatalf("hook: %d", code)
	}
	if code := hook("", "?token=s3cret&chat=alerts", `{"id":"a1","text":"disk full"}`); code != http.StatusNoContent {
		t.Fatalf("repeat: %d", code)
	}
	hook("s3cret", "", `{"chat":"alerts","id":"a1","text":"disk 99% full","edit":true,"username":"Grafana"}`)
	log := e.log(room.ID)
	if len(log) != 2 || log[0].Prose() != "disk 99% full" || log[1].Type != core.EventEdit || log[1].Prose() != "disk 99% full" {
		t.Fatalf("log = %+v", log)
	}
	grafana, err := e.store.GetPrincipal(ctx, log[0].SenderID)
	if err != nil || grafana.DisplayName != "Grafana" || grafana.Kind != core.PrincipalGhost {
		t.Fatalf("sender = %+v, %v", grafana, err)
	}

	// The room's messages go out once the bridge runs; the ghost's don't.
	e.runner("n1")
	eventually(t, "the bridge is live", func() bool { return e.bridge(b.ID).State == "live" })
	msg, _, _ := e.srv.SendText(ctx, e.ada, room.ID, "on it", "", "", false)
	eventually(t, "Ada's message is posted", func() bool { return e.delivery(link.ID, msg.ID) == store.BridgeSent })
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0]["text"] != "on it" || posted[0]["sender"] != "Ada" || posted[0]["chat"] != "alerts" || !sigOK {
		t.Fatalf("posted = %+v, signature ok %v", posted, sigOK)
	}
	if m, _ := e.store.GetBridgeMessage(ctx, link.ID, msg.ID); m.RemoteID != "42" {
		t.Fatalf("remote id = %q", m.RemoteID)
	}
}

func TestWebhookLimitAndNewSecret(t *testing.T) {
	e := newEnv(t)
	// A node whose hooks take two messages a minute.
	limited := httptest.NewServer(api.New(api.Config{Version: "test", Store: e.store, Bus: e.bus, HookLimit: 2}).Handler())
	defer limited.Close()

	var b wireBridge
	body := map[string]any{"network": "webhook", "mode": "bot", "config": map[string]any{"token": "s3cret"}}
	if code := e.call("POST", "/api/v1/bridges", body, &b); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var room struct{ ID string }
	e.call("POST", "/api/v1/rooms", map[string]string{"kind": "group", "name": "Ops"}, &room)
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/links", map[string]string{"room_id": room.ID, "remote_id": "default"}, nil); code != http.StatusCreated {
		t.Fatalf("link: %d", code)
	}

	hook := func(secret string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", limited.URL+"/api/v1/hooks/"+b.ID, strings.NewReader(`{"text":"x"}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	// Wrong secrets don't use up the allowance.
	for range 3 {
		if res := hook("wrong"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong secret: %d", res.StatusCode)
		}
	}
	for i := range 2 {
		if res := hook("s3cret"); res.StatusCode != http.StatusNoContent {
			t.Fatalf("message %d: %d", i, res.StatusCode)
		}
	}
	res := hook("s3cret")
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d, Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if n := len(e.log(room.ID)); n != 2 {
		t.Fatalf("%d messages in the room", n)
	}

	// A new secret: the old one is refused, the new one works.
	var rotated struct {
		Secret string     `json:"secret"`
		Bridge wireBridge `json:"bridge"`
	}
	if code := e.call("POST", "/api/v1/bridges/"+b.ID+"/hook-secret", nil, &rotated); code != http.StatusOK || len(rotated.Secret) < 32 || rotated.Bridge.ID != b.ID {
		t.Fatalf("new secret: %d %+v", code, rotated)
	}
	if res := hook("s3cret"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old secret: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("POST", e.http.URL+"/api/v1/hooks/"+b.ID, strings.NewReader(`{"text":"y"}`))
	req.Header.Set("Authorization", "Bearer "+rotated.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("new secret: %d", res.StatusCode)
	}

	// Bridges without a hook secret have nothing to replace.
	var fake wireBridge
	e.call("POST", "/api/v1/bridges", map[string]any{"network": "fake", "mode": "bot", "config": map[string]any{"server": "fake.example"}}, &fake)
	if code := e.call("POST", "/api/v1/bridges/"+fake.ID+"/hook-secret", nil, nil); code != http.StatusConflict {
		t.Fatalf("fake: %d", code)
	}
}
