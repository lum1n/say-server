package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/core"
)

func TestBridges(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		now := time.Now().Truncate(time.Millisecond)
		b := Bridge{ID: core.NewID(), OwnerID: p.ID, Mode: ModeAccount, Network: "irc", Name: "Libera", Config: []byte(`{"server":"irc.example:6697"}`), Enabled: true, UpdatedAt: now, CreatedAt: now}
		if err := s.CreateBridge(ctx, b); err != nil {
			t.Fatal(err)
		}
		b.Name, b.Enabled, b.Config = "Libera.Chat", false, nil
		if err := s.UpdateBridge(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBridgeError(ctx, b.ID, "boom"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBridgeSeen(ctx, b.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBridgeState(ctx, b.ID, []byte(`{"self":"ada"}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBridgePrompt(ctx, b.ID, []byte(`{"kind":"code"}`)); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetBridge(ctx, b.ID)
		if err != nil || got.Name != "Libera.Chat" || got.Enabled || string(got.Config) != "{}" || got.LastError != "boom" || !got.SeenAt.Equal(now) ||
			got.OwnerID != p.ID || got.Mode != ModeAccount || string(got.State) != `{"self":"ada"}` || string(got.Prompt) != `{"kind":"code"}` {
			t.Fatalf("GetBridge = %+v, %v", got, err)
		}
		if err := s.SetBridgePrompt(ctx, b.ID, nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetBridge(ctx, b.ID); got.Prompt != nil {
			t.Fatalf("prompt not cleared: %s", got.Prompt)
		}
		if err := s.UpdateBridge(ctx, Bridge{ID: "nope"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update unknown: %v", err)
		}
		if all, err := s.Bridges(ctx); err != nil || len(all) != 1 {
			t.Fatalf("Bridges = %+v, %v", all, err)
		}

		l := BridgeLink{ID: core.NewID(), BridgeID: b.ID, RoomID: r.ID, RemoteID: "#say", Cursor: 0, CreatedAt: now}
		if err := s.CreateBridgeLink(ctx, l); err != nil {
			t.Fatal(err)
		}
		dup := l
		dup.ID = core.NewID()
		if err := s.CreateBridgeLink(ctx, dup); !errors.Is(err, ErrLinkExists) {
			t.Fatalf("duplicate link: %v", err)
		}
		if err := s.SetBridgeLinkCursor(ctx, l.ID, 5); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBridgeLinkCursor(ctx, l.ID, 3); err != nil {
			t.Fatal(err)
		}
		if got, err := s.BridgeLinkByRemote(ctx, b.ID, "#say"); err != nil || got.ID != l.ID || got.Cursor != 5 {
			t.Fatalf("BridgeLinkByRemote = %+v, %v; the cursor only moves forward", got, err)
		}
		if links, err := s.RoomBridgeLinks(ctx, r.ID); err != nil || len(links) != 1 {
			t.Fatalf("RoomBridgeLinks = %+v, %v", links, err)
		}

		portal := func(remote string) (core.Room, error) {
			room := core.Room{ID: core.NewID(), Kind: core.RoomDM, Name: "bob", CreatedAt: now}
			ms := []core.Membership{{RoomID: room.ID, PrincipalID: p.ID, Role: core.RoleOwner}}
			return room, s.CreatePortal(ctx, room, ms, BridgeLink{ID: core.NewID(), BridgeID: b.ID, RoomID: room.ID, RemoteID: remote, CreatedAt: now})
		}
		pr, err := portal("@bob")
		if err != nil {
			t.Fatal(err)
		}
		if m, err := s.GetMember(ctx, pr.ID, p.ID); err != nil || m.Role != core.RoleOwner {
			t.Fatalf("portal owner = %+v, %v", m, err)
		}
		lost, err := portal("@bob")
		if !errors.Is(err, ErrLinkExists) {
			t.Fatalf("second portal: %v", err)
		}
		if _, err := s.GetRoom(ctx, lost.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the losing portal's room was kept: %v", err)
		}

		g, err := s.EnsureGhost(ctx, b.ID, "u1", "alice")
		if err != nil || g.Kind != core.PrincipalGhost || g.DisplayName != "alice" {
			t.Fatalf("EnsureGhost = %+v, %v", g, err)
		}
		again, err := s.EnsureGhost(ctx, b.ID, "u1", "alice_")
		if err != nil || again.ID != g.ID || again.DisplayName != "alice_" {
			t.Fatalf("EnsureGhost again = %+v, %v", again, err)
		}
		if back, _ := s.GetPrincipal(ctx, g.ID); back.DisplayName != "alice_" {
			t.Fatalf("ghost not renamed: %+v", back)
		}
		if gh, err := s.BridgeGhostByPrincipal(ctx, g.ID); err != nil || gh.RemoteUserID != "u1" || gh.BridgeID != b.ID {
			t.Fatalf("BridgeGhostByPrincipal = %+v, %v", gh, err)
		}
		if _, err := s.BridgeGhostByPrincipal(ctx, p.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("not a ghost: %v", err)
		}

		var evs []core.Event
		for range 3 {
			e, err := s.AppendEvent(ctx, core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID, Parts: text("hi")}, AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			evs = append(evs, e)
		}
		out := BridgeMessage{LinkID: l.ID, EventID: evs[0].ID, Direction: BridgeOut, State: BridgeSending, CreatedAt: now}
		if ok, err := s.StartBridgeMessage(ctx, out); err != nil || !ok {
			t.Fatalf("StartBridgeMessage = %v, %v", ok, err)
		}
		if ok, err := s.StartBridgeMessage(ctx, out); err != nil || ok {
			t.Fatalf("second StartBridgeMessage = %v, %v", ok, err)
		}
		in := BridgeMessage{LinkID: l.ID, EventID: evs[1].ID, RemoteID: "r2", Direction: BridgeIn, State: BridgeSent, CreatedAt: now}
		if ok, err := s.StartBridgeMessage(ctx, in); err != nil || !ok {
			t.Fatalf("inbound = %v, %v", ok, err)
		}
		// The same remote message for another event is a duplicate too.
		in.EventID = evs[2].ID
		if ok, err := s.StartBridgeMessage(ctx, in); err != nil || ok {
			t.Fatalf("duplicate remote id = %v, %v", ok, err)
		}
		out2 := BridgeMessage{LinkID: l.ID, EventID: evs[2].ID, Direction: BridgeOut, State: BridgeSending, CreatedAt: now}
		if _, err := s.StartBridgeMessage(ctx, out2); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBridgeMessage(ctx, l.ID, evs[0].ID, BridgeSent, "r1", ""); err != nil {
			t.Fatal(err)
		}
		if m, err := s.BridgeMessageByRemote(ctx, l.ID, "r1"); err != nil || m.EventID != evs[0].ID || m.State != BridgeSent {
			t.Fatalf("BridgeMessageByRemote = %+v, %v", m, err)
		}
		// A network that reuses an id: the send is kept without it.
		if err := s.FinishBridgeMessage(ctx, l.ID, evs[2].ID, BridgeSent, "r1", ""); err != nil {
			t.Fatalf("reused remote id: %v", err)
		}
		if m, _ := s.GetBridgeMessage(ctx, l.ID, evs[2].ID); m.State != BridgeSent || m.RemoteID != "" {
			t.Fatalf("reused remote id = %+v", m)
		}
		if m, _ := s.BridgeMessageByRemote(ctx, l.ID, "r1"); m.EventID != evs[0].ID {
			t.Fatalf("r1 is now %+v", m)
		}
		s.FinishBridgeMessage(ctx, l.ID, evs[2].ID, BridgeSending, "", "")
		if err := s.MarkBridgeSendsUnknown(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if m, err := s.GetBridgeMessage(ctx, l.ID, evs[2].ID); err != nil || m.State != BridgeUnknown {
			t.Fatalf("left sending = %+v, %v", m, err)
		}
		outs, err := s.OutgoingBridgeMessages(ctx, r.ID, evs[0].Seq, evs[2].Seq)
		if err != nil || len(outs) != 2 {
			t.Fatalf("OutgoingBridgeMessages = %+v, %v", outs, err)
		}
		if outs, _ := s.OutgoingBridgeMessages(ctx, r.ID, evs[1].Seq, evs[1].Seq); len(outs) != 0 {
			t.Fatalf("inbound rows listed: %+v", outs)
		}

		if err := s.DeleteBridgeLink(ctx, b.ID, l.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetBridgeLink(ctx, l.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted link: %v", err)
		}
		if err := s.DeleteBridgeLink(ctx, b.ID, l.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete twice: %v", err)
		}

		// Deleting the bridge keeps its portal rooms, their logs and its
		// ghosts, unlinked.
		if err := s.MarkBridgeDeleted(ctx, b.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkBridgeDeleted(ctx, b.ID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetBridge(ctx, b.ID); !got.DeletedAt.Equal(now) {
			t.Fatalf("DeletedAt = %v, want the first ask", got.DeletedAt)
		}
		if err := s.MarkBridgeDeleted(ctx, "nope", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("mark unknown: %v", err)
		}
		pl, _ := s.BridgeLinkByRemote(ctx, b.ID, "@bob")
		if _, err := s.StartBridgeMessage(ctx, BridgeMessage{LinkID: pl.ID, EventID: core.NewID(), Direction: BridgeOut, State: BridgeSent, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteBridge(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetBridge(ctx, b.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted bridge: %v", err)
		}
		if links, _ := s.RoomBridgeLinks(ctx, pr.ID); len(links) != 0 {
			t.Fatalf("portal still linked: %+v", links)
		}
		if _, err := s.GetRoom(ctx, pr.ID); err != nil {
			t.Fatalf("portal room gone: %v", err)
		}
		if _, err := s.GetPrincipal(ctx, g.ID); err != nil {
			t.Fatalf("ghost gone: %v", err)
		}
		if _, err := s.BridgeGhostByPrincipal(ctx, g.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ghost still mapped: %v", err)
		}
		if err := s.DeleteBridge(ctx, b.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete bridge twice: %v", err)
		}
	})
}

// A screen code is read any number of times until it runs out, is typed
// like a pairing code, points at its own bridge, and goes with the bridge.
func TestBridgeScreens(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, _ := seed(t, s)
		now := time.Now().Truncate(time.Millisecond)
		newBridge := func() Bridge {
			b := Bridge{ID: core.NewID(), OwnerID: p.ID, Mode: ModeAccount, Network: "signal", Name: "Signal", Enabled: true, UpdatedAt: now, CreatedAt: now}
			if err := s.CreateBridge(ctx, b); err != nil {
				t.Fatal(err)
			}
			return b
		}
		a, b := newBridge(), newBridge()
		code, err := s.CreateBridgeScreen(ctx, a.ID, time.Now().Add(time.Minute))
		if err != nil || len(code) != 11 || code[5] != '-' {
			t.Fatalf("CreateBridgeScreen = %q, %v", code, err)
		}
		other, err := s.CreateBridgeScreen(ctx, b.ID, time.Now().Add(time.Minute))
		if err != nil || other == code {
			t.Fatalf("a second code = %q, %v", other, err)
		}
		// Not used up by reading, and typed by hand like a pairing code.
		typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
		typed = strings.NewReplacer("0", "o", "1", "l").Replace(typed)
		for _, c := range []string{code, code, typed} {
			if id, err := s.BridgeScreen(ctx, c); err != nil || id != a.ID {
				t.Fatalf("BridgeScreen(%q) = %q, %v", c, id, err)
			}
		}
		if id, err := s.BridgeScreen(ctx, other); err != nil || id != b.ID {
			t.Fatalf("BridgeScreen(other) = %q, %v", id, err)
		}
		if _, err := s.BridgeScreen(ctx, "WRONG-CODE0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a wrong code: %v", err)
		}
		old, err := s.CreateBridgeScreen(ctx, a.ID, time.Now().Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.BridgeScreen(ctx, old); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an expired code: %v", err)
		}

		// Deleting a bridge takes its codes, and only its codes.
		if err := s.DeleteBridge(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BridgeScreen(ctx, code); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a deleted bridge's code: %v", err)
		}
		if id, err := s.BridgeScreen(ctx, other); err != nil || id != b.ID {
			t.Fatalf("another bridge's code after a delete = %q, %v", id, err)
		}
	})
}
