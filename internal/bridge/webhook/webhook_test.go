package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lum1n/say-server/internal/bridge"
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

func TestReactions(t *testing.T) {
	info := bridge.Info{Config: json.RawMessage(`{"token":"t"}`)}
	post := func(body string) ([]bridge.Incoming, error) {
		r := httptest.NewRequest(http.MethodPost, "/hook?token=t", strings.NewReader(body))
		return hook(info, r)
	}
	got, err := post(`{"type":"reaction","chat":"alerts","target":"m1","key":"👍","user_id":"u1","username":"Ops"}`)
	if err != nil || len(got) != 1 {
		t.Fatalf("reaction: %v %v", got, err)
	}
	if m := got[0]; m.Chat.ID != "alerts" || m.Author.ID != "u1" || m.Reaction == nil || *m.Reaction != (bridge.IncomingReaction{Target: "m1", Key: "👍"}) {
		t.Fatalf("reaction = %+v", m)
	}
	got, err = post(`{"type":"reaction","target":"m1","key":"👍","removed":true}`)
	if err != nil || got[0].Chat.ID != "default" || !got[0].Reaction.Removed || got[0].Author.Name != "Webhook" {
		t.Fatalf("removal: %+v %v", got, err)
	}
	for _, body := range []string{`{"type":"reaction","key":"👍"}`, `{"type":"reaction","target":"m1"}`, `{"type":"poke","text":"x"}`} {
		if _, err := post(body); err == nil {
			t.Errorf("%s was taken", body)
		}
	}

	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = nil
		json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	for _, format := range []string{"say", "slack"} {
		sent = nil
		c, err := open(bridge.Info{Config: json.RawMessage(`{"token":"t","url":"` + srv.URL + `","format":"` + format + `"}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.(bridge.Reactor).React(context.Background(), bridge.Reaction{Chat: "c", EventID: "e2", Target: "m1", Key: "🎉", Removed: true})
		if err != nil {
			t.Fatal(err)
		}
		switch format {
		case "say":
			if sent["type"] != "reaction" || sent["target"] != "m1" || sent["key"] != "🎉" || sent["removed"] != true || sent["event_id"] != "e2" {
				t.Errorf("say reaction: %v", sent)
			}
		default:
			if sent != nil {
				t.Errorf("%s got a reaction: %v", format, sent)
			}
		}
	}
}
