package say

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openNode(t *testing.T) *Node {
	t.Helper()
	n, err := Open(context.Background(), Config{DB: filepath.Join(t.TempDir(), "say.db"), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func TestLocalUser(t *testing.T) {
	ctx := context.Background()
	n := openNode(t)
	if n.SetupCode() == "" {
		t.Fatal("a fresh node has no setup code")
	}
	ada, err := n.LocalUser(ctx, "Ada", "Ada L", "tui")
	if err != nil || !ada.Admin || ada.DisplayName != "Ada L" || ada.Token == "" {
		t.Fatalf("first local user = %+v, %v", ada, err)
	}
	if n.SetupCode() != "" {
		t.Fatal("setup code after the first local user")
	}
	// Same person and client: the old token stops working.
	again, err := n.LocalUser(ctx, "ada", "", "tui")
	if err != nil || again.ID != ada.ID || again.Token == ada.Token {
		t.Fatalf("again = %+v, %v", again, err)
	}
	var e *Error
	if _, err := n.Client(ada.Token).Me(ctx); !errors.As(err, &e) || e.Status != 401 {
		t.Fatalf("old token: %v", err)
	}
	// Another client keeps its own token.
	phone, _ := n.LocalUser(ctx, "ada", "", "phone")
	for _, tok := range []string{again.Token, phone.Token} {
		if me, err := n.Client(tok).Me(ctx); err != nil || me.ID != ada.ID {
			t.Fatalf("Me = %+v, %v", me, err)
		}
	}
	bob, err := n.LocalUser(ctx, "bob", "Bob", "tui")
	if err != nil || bob.Admin || bob.ID == ada.ID {
		t.Fatalf("second local user = %+v, %v", bob, err)
	}
	if _, err := n.LocalUser(ctx, "", "", "tui"); err == nil {
		t.Fatal("empty username accepted")
	}
}

func TestClientAndRealtime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := openNode(t)
	ada, _ := n.LocalUser(ctx, "ada", "Ada", "test")
	bob, _ := n.LocalUser(ctx, "bob", "Bob", "test")
	a, b := n.Client(ada.Token), n.Client(bob.Token)

	room, err := a.CreateRoom(ctx, "group", "Tea")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddMember(ctx, room.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	first, err := a.Send(ctx, room.ID, []Part{TextPart("one")}, "k1")
	if err != nil || first.Seq != 1 || first.Text() != "one" {
		t.Fatalf("send = %+v, %v", first, err)
	}
	if again, _ := a.Send(ctx, room.ID, []Part{TextPart("one")}, "k1"); again.ID != first.ID {
		t.Fatal("idempotency key not honoured")
	}

	rt, err := b.Realtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.Subscribe(room.ID, 0)
	next := func() Push {
		t.Helper()
		select {
		case p := <-rt.Pushes():
			return p
		case <-ctx.Done():
			t.Fatal("no push")
		}
		return Push{}
	}
	if p := next(); p.Type != "event" || p.Event.Text() != "one" {
		t.Fatalf("replayed = %+v", p)
	}
	a.Send(ctx, room.ID, []Part{TextPart("two")}, "")
	if p := next(); p.Event == nil || p.Event.Text() != "two" || p.Event.Seq != 2 {
		t.Fatalf("live = %+v", p)
	}
	evs, err := b.Events(ctx, room.ID, EventsQuery{After: 1})
	if err != nil || len(evs) != 1 || evs[0].Text() != "two" {
		t.Fatalf("events after 1 = %+v, %v", evs, err)
	}
	ms, err := b.Members(ctx, room.ID)
	if err != nil || len(ms) != 2 {
		t.Fatalf("members = %+v, %v", ms, err)
	}
}

func TestBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := openNode(t)
	ada, _ := n.LocalUser(ctx, "ada", "Ada", "test")

	res, err := n.Request(ctx, "POST", "/api/v1/rooms", ada.Token, `{"kind":"group","name":"Raw"}`)
	if err != nil || res.Status != 201 {
		t.Fatalf("create = %+v, %v", res, err)
	}
	var room Room
	json.Unmarshal([]byte(res.Body), &room)
	if res, _ := n.Request(ctx, "GET", "/api/v1/me", "", ""); res.Status != 401 {
		t.Fatalf("no token = %d", res.Status)
	}

	conn, err := n.Connect(ctx, ada.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Send(ctx, fmt.Sprintf(`{"id":"1","type":"subscribe","payload":{"room_id":%q,"after":0}}`, room.ID))
	conn.Send(ctx, fmt.Sprintf(`{"id":"2","type":"send","payload":{"room_id":%q,"parts":[{"kind":"text","data":{"text":"hi"}}]}}`, room.ID))
	var gotReply, gotEvent bool
	for !gotReply || !gotEvent {
		msg, err := conn.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var env struct{ ID, Type, Status string }
		json.Unmarshal([]byte(msg), &env)
		gotReply = gotReply || env.ID == "2" && env.Status == "ok"
		gotEvent = gotEvent || env.Type == "event" && strings.Contains(msg, `"hi"`)
	}
}

func TestListen(t *testing.T) {
	n := openNode(t)
	addr, err := n.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get("http://" + addr.String() + "/api/v1/health")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("health over TCP = %v, %v", res, err)
	}
	res.Body.Close()
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://" + addr.String() + "/api/v1/health"); err == nil {
		t.Fatal("still listening after Close")
	}
	if _, err := n.Request(context.Background(), "GET", "/api/v1/health", "", ""); err == nil {
		t.Fatal("in-memory endpoint works after Close")
	}
}
