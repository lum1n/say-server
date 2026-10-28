package say_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/api"
	"github.com/lum1n/say-server/internal/bridge"
	say "github.com/lum1n/say-server/internal/bridge/say"
	"github.com/lum1n/say-server/internal/bus"
	"github.com/lum1n/say-server/internal/client"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/password"
	"github.com/lum1n/say-server/internal/store"
)

// node is a say instance: home, or a device.
type node struct {
	t     *testing.T
	store store.Store
	srv   *api.Server
	http  *httptest.Server
	// down answers every request 503; lose makes home take the next
	// message it is sent and then answer 502, as if the answer was lost;
	// old has no media routes, as a home from before files.
	down, lose, old atomic.Bool
}

func newNode(t *testing.T) *node {
	t.Helper()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "say.db"))
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewInProc()
	n := &node{t: t, store: st}
	n.srv = api.New(api.Config{Version: "test", Store: st, Bus: b})
	h := n.srv.Handler()
	n.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if n.old.Load() && strings.Contains(r.URL.Path, "/media") {
			http.NotFound(w, r)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") && n.lose.CompareAndSwap(true, false) {
			h.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "lost", http.StatusBadGateway)
			return
		}
		h.ServeHTTP(w, r)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runner := bridge.New(bridge.Config{Store: st, Bus: b, Chat: n.srv, Holder: "n"})
	go func() {
		defer close(done)
		runner.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		n.http.CloseClientConnections()
		n.http.Close()
		b.Close()
		st.Close()
	})
	return n
}

func (n *node) person(name string, login bool) (core.Principal, string) {
	n.t.Helper()
	ctx := context.Background()
	p := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: name, Admin: true, CreatedAt: time.Now()}
	var err error
	if login {
		err = n.store.CreateUser(ctx, p, store.Login{Username: strings.ToLower(name), PasswordHash: password.Hash("correct horse")})
	} else {
		err = n.store.CreatePrincipal(ctx, p)
	}
	if err != nil {
		n.t.Fatal(err)
	}
	token, err := n.store.CreateToken(ctx, p.ID, "test", time.Time{})
	if err != nil {
		n.t.Fatal(err)
	}
	return p, token
}

func (n *node) call(token, method, path string, body, out any) int {
	n.t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, n.http.URL+path, strings.NewReader(string(data)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func (n *node) log(roomID string) []core.Event {
	n.t.Helper()
	evs, err := n.store.EventsAfter(context.Background(), roomID, 0, 100)
	if err != nil {
		n.t.Fatal(err)
	}
	var out []core.Event
	for _, e := range evs {
		if e.Type == core.EventMessage {
			out = append(out, e)
		}
	}
	return out
}

func prose(es []core.Event) string {
	var s []string
	for _, e := range es {
		s = append(s, e.Prose())
	}
	return strings.Join(s, " | ")
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type wireBridge struct {
	ID     string         `json:"id"`
	State  string         `json:"state"`
	Error  string         `json:"error"`
	Prompt *bridge.Prompt `json:"prompt"`
}

func TestSync(t *testing.T) {
	say.StallFor = 100 * time.Millisecond
	ctx := context.Background()
	home, laptop := newNode(t), newNode(t)
	ada, adaWeb := home.person("Ada", true)
	bob, _ := home.person("Bob", false)
	room := core.Room{ID: core.NewID(), Kind: core.RoomGroup, Name: "Crew", CreatedAt: time.Now()}
	if err := home.store.CreateRoom(ctx, room, core.Membership{RoomID: room.ID, PrincipalID: ada.ID, Role: core.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := home.store.AddMember(ctx, core.Membership{RoomID: room.ID, PrincipalID: bob.ID, Role: core.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if _, err := home.srv.SendAs(ctx, bob, room.ID, core.NewID(), []core.Part{core.TextPart("long ago")}, "", "", time.Now().Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	hi, _, err := home.srv.SendText(ctx, bob, room.ID, "hi Ada", "", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Pairing: the laptop's Ada adds home on the Accounts page and types
	// her password into the prompt.
	me, token := laptop.person("Ada", false)
	var b wireBridge
	body := map[string]any{"network": "say", "mode": "account", "config": map[string]any{"url": home.http.URL, "username": "ada", "device": "laptop"}}
	if code := laptop.call(token, "POST", "/api/v1/bridges", body, &b); code != http.StatusCreated {
		t.Fatalf("create bridge: %d", code)
	}
	get := func() wireBridge {
		var got wireBridge
		laptop.call(token, "GET", "/api/v1/bridges/"+b.ID, nil, &got)
		return got
	}
	eventually(t, "it asks for the password", func() bool { return get().State == "login" })
	if p := get().Prompt; p.Kind != "password" {
		t.Fatalf("prompt = %+v", p)
	}
	laptop.call(token, "POST", "/api/v1/bridges/"+b.ID+"/answer", map[string]string{"value": "wrong password"}, nil)
	eventually(t, "it asks again", func() bool { g := get(); return g.State == "login" && g.Error != "" })
	laptop.call(token, "POST", "/api/v1/bridges/"+b.ID+"/answer", map[string]string{"value": "correct horse"}, nil)
	eventually(t, "the bridge is live", func() bool { return get().State == "live" })
	stored, _ := laptop.store.GetBridge(ctx, b.ID)
	if strings.Contains(string(stored.Config)+string(stored.State), "correct horse") {
		t.Fatal("the password is kept")
	}

	// The home room is a portal with the last 30 days in it, Bob a ghost.
	var l store.BridgeLink
	eventually(t, "the room is a portal", func() bool {
		l, err = laptop.store.BridgeLinkByRemote(ctx, b.ID, room.ID)
		return err == nil
	})
	eventually(t, "the backfill is in", func() bool { return prose(laptop.log(l.RoomID)) == "hi Ada" })
	portal, _ := laptop.store.GetRoom(ctx, l.RoomID)
	ghost, _ := laptop.store.GetPrincipal(ctx, laptop.log(l.RoomID)[0].SenderID)
	if portal.Name != "Crew" || ghost.Kind != core.PrincipalGhost || ghost.DisplayName != "Bob" {
		t.Fatalf("portal %+v, sender %+v", portal, ghost)
	}

	// The laptop's messages go to home as Ada, once, and their echo is
	// not taken in again.
	sent, _, err := laptop.srv.SendText(ctx, me, l.RoomID, "from the laptop", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "home has it", func() bool { return prose(home.log(room.ID)) == "long ago | hi Ada | from the laptop" })
	if got := home.log(room.ID)[2]; got.SenderID != ada.ID {
		t.Fatalf("sent as %s", got.SenderID)
	}
	// Ada on the web comes in as the laptop's Ada.
	msg := map[string]any{"parts": []map[string]any{{"kind": "text", "data": map[string]string{"text": "from the web"}}}}
	if code := home.call(adaWeb, "POST", "/api/v1/rooms/"+room.ID+"/messages", msg, nil); code != http.StatusCreated {
		t.Fatalf("send from the web: %d", code)
	}
	eventually(t, "the web message is in", func() bool { return len(laptop.log(l.RoomID)) == 3 })
	got := laptop.log(l.RoomID)
	if prose(got) != "hi Ada | from the laptop | from the web" || got[2].SenderID != me.ID {
		t.Fatalf("laptop log = %s, last from %s", prose(got), got[2].SenderID)
	}

	// Edits go both ways.
	if _, err := home.srv.EditAs(ctx, bob, room.ID, hi.ID, []core.Part{core.TextPart("hi Ada!")}); err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.srv.EditAs(ctx, me, l.RoomID, sent.ID, []core.Part{core.TextPart("from the laptop, edited")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the edits are in", func() bool {
		return prose(laptop.log(l.RoomID)) == "hi Ada! | from the laptop, edited | from the web" &&
			prose(home.log(room.ID)) == "long ago | hi Ada! | from the laptop, edited | from the web"
	})

	// Reactions go both ways: Bob's on home shows on the laptop, and Ada's
	// from the laptop, and taking it back, reach home.
	reactions := func(n *node, roomID string) string {
		var out []string
		// "hi Ada!" is third from the end on both.
		for _, r := range n.log(roomID)[len(n.log(roomID))-3].Reactions {
			out = append(out, fmt.Sprintf("%s×%d", r.Key, len(r.Senders)))
		}
		return strings.Join(out, " ")
	}
	if _, err := home.srv.ReactAs(ctx, bob, room.ID, "", hi.ID, "👍", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Bob's reaction is on the laptop", func() bool { return reactions(laptop, l.RoomID) == "👍×1" })
	laptopHi := laptop.log(l.RoomID)[0]
	if _, err := laptop.srv.ReactAs(ctx, me, l.RoomID, "", laptopHi.ID, "🎉", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ada's reaction is on home", func() bool { return reactions(home, room.ID) == "👍×1 🎉×1" })
	if _, err := laptop.srv.ReactAs(ctx, me, l.RoomID, "", laptopHi.ID, "🎉", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Ada's reaction is gone from home", func() bool { return reactions(home, room.ID) == "👍×1" })
	// Its echo from home changes nothing on the laptop.
	time.Sleep(100 * time.Millisecond)
	if got := reactions(laptop, l.RoomID); got != "👍×1" {
		t.Fatalf("laptop reactions = %s", got)
	}

	// Offline: what the laptop writes waits, however old, and goes out in
	// order when home is back.
	home.down.Store(true)
	a, _, _ := laptop.srv.SendText(ctx, me, l.RoomID, "offline one", "", "", false)
	old, err := laptop.srv.SendAs(ctx, me, l.RoomID, core.NewID(), []core.Part{core.TextPart("offline two")}, "", "", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the send failed", func() bool {
		m, err := laptop.store.GetBridgeMessage(ctx, l.ID, a.ID)
		return err == nil && m.State == store.BridgeSending && m.Error != ""
	})
	if _, err := laptop.store.GetBridgeMessage(ctx, l.ID, old.ID); err == nil {
		t.Fatal("a message went before the one ahead of it")
	}
	home.down.Store(false)
	eventually(t, "both are on home", func() bool {
		return strings.HasSuffix(prose(home.log(room.ID)), "from the web | offline one | offline two")
	})

	// A send whose answer is lost is sent again; home takes it once, and
	// the laptop takes its echo in once.
	home.lose.Store(true)
	lost, _, _ := laptop.srv.SendText(ctx, me, l.RoomID, "lost answer", "", "", false)
	eventually(t, "it is sent", func() bool {
		m, err := laptop.store.GetBridgeMessage(ctx, l.ID, lost.ID)
		return err == nil && m.State == store.BridgeSent
	})
	time.Sleep(300 * time.Millisecond)
	if p := prose(home.log(room.ID)); !strings.HasSuffix(p, "offline two | lost answer") {
		t.Fatalf("home log = %s", p)
	}
	if p := prose(laptop.log(l.RoomID)); !strings.HasSuffix(p, "from the web | offline one | offline two | lost answer") {
		t.Fatalf("laptop log = %s", p)
	}

	// Files go both ways, the same bytes on both sides.
	file := func(n *node, e core.Event) string {
		as := e.Attachments()
		if len(as) != 1 {
			return ""
		}
		r, err := n.srv.OpenMedia(ctx, as[0].Media)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return fmt.Sprintf("%s %s %dx%d %s", as[0].Name, as[0].Mime, as[0].Width, as[0].Height, b)
	}
	last := func(n *node, roomID string) core.Event { l := n.log(roomID); return l[len(l)-1] }
	var img bytes.Buffer
	png.Encode(&img, image.NewGray(image.Rect(0, 0, 3, 2)))
	part, err := laptop.srv.Upload(ctx, me, l.RoomID, bytes.NewReader(img.Bytes()), "image/png", "dot.png")
	if err != nil {
		t.Fatal(err)
	}
	withFile, err := laptop.srv.SendAs(ctx, me, l.RoomID, core.NewID(), []core.Part{core.TextPart("a dot"), part}, "", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := "dot.png image/png 3x2 " + img.String()
	eventually(t, "home has the file", func() bool { return file(home, last(home, room.ID)) == want })
	if p := last(home, room.ID).Prose(); p != "a dot" {
		t.Fatalf("home prose = %q", p)
	}
	homeFile := last(home, room.ID)
	// An edit of its text keeps home's file.
	if _, err := laptop.srv.EditAs(ctx, me, l.RoomID, withFile.ID, []core.Part{core.TextPart("a dot, edited"), part}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the edit is on home", func() bool {
		e, err := home.store.GetEvent(ctx, homeFile.ID)
		return err == nil && e.Prose() == "a dot, edited" && file(home, e) == want
	})
	bobPart, err := home.srv.Upload(ctx, bob, room.ID, strings.NewReader("a,b\n1,2\n"), "text/csv", "data.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := home.srv.SendAs(ctx, bob, room.ID, core.NewID(), []core.Part{bobPart}, "", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the laptop has Bob's file", func() bool {
		return file(laptop, last(laptop, l.RoomID)) == "data.csv text/csv 0x0 a,b\n1,2\n"
	})
	// A home from before files gets the file named instead.
	home.old.Store(true)
	part, err = laptop.srv.Upload(ctx, me, l.RoomID, bytes.NewReader(img.Bytes()), "image/png", "again.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := laptop.srv.SendAs(ctx, me, l.RoomID, core.NewID(), []core.Part{part}, "", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "home has the file named", func() bool { return last(home, room.ID).Prose() == "[image: again.png]" })
	home.old.Store(false)

	// Deleting the bridge unpairs the laptop.
	var st struct{ Token string }
	stored, _ = laptop.store.GetBridge(ctx, b.ID)
	json.Unmarshal(stored.State, &st)
	if _, err := client.New(home.http.URL, st.Token).Me(ctx); err != nil {
		t.Fatalf("device token: %v", err)
	}
	if code := laptop.call(token, "DELETE", "/api/v1/bridges/"+b.ID, nil, nil); code >= 300 {
		t.Fatalf("delete: %d", code)
	}
	eventually(t, "the token is revoked", func() bool {
		_, err := client.New(home.http.URL, st.Token).Me(ctx)
		return err != nil
	})
}
