package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vegard/say-v2/internal/bridge"
)

func TestOutgoingFormats(t *testing.T) {
	var got map[string]any
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	m := bridge.Outgoing{Chat: "c", EventID: "e1", Text: "hi <@everyone>", Sender: "Ada"}
	send := func(format string) (string, error) {
		c, err := open(bridge.Info{Config: json.RawMessage(`{"token":"t","url":"` + srv.URL + `","format":"` + format + `"}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Send(context.Background(), m)
	}

	if id, err := send("slack"); err != nil || id != "e1" || got["text"] != "*Ada*: hi <@everyone>" {
		t.Fatalf("slack: %q %v %v", id, err, got)
	}
	if _, err := send("discord"); err != nil || got["content"] != "hi <@everyone>" || got["username"] != "Ada" || got["allowed_mentions"] == nil {
		t.Fatalf("discord: %v %v", err, got)
	}
	if _, err := send("say"); err != nil || got["type"] != "message" || got["event_id"] != "e1" {
		t.Fatalf("say: %v %v", err, got)
	}
	status = http.StatusNotFound
	if _, err := send("say"); !errors.Is(err, bridge.ErrRejected) {
		t.Fatalf("404 is a refusal: %v", err)
	}
	status = http.StatusBadGateway
	if _, err := send("say"); err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Fatalf("502 leaves it unknown: %v", err)
	}
	status = http.StatusTooManyRequests
	if _, err := send("say"); err == nil || errors.Is(err, bridge.ErrRejected) {
		t.Fatalf("429 leaves it unknown: %v", err)
	}
	if _, err := open(bridge.Info{Config: json.RawMessage(`{"token":"t","format":"xml"}`)}, nil); err == nil {
		t.Fatal("an unknown format is refused")
	}
}
