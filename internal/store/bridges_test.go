package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
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
	})
}
