package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/core"
)

func TestWidgetsAndVisitors(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		now := time.Now().Truncate(time.Millisecond)
		ada := core.Principal{ID: core.NewID(), Kind: core.PrincipalHuman, DisplayName: "Ada", CreatedAt: now}
		if err := s.CreatePrincipal(ctx, ada); err != nil {
			t.Fatal(err)
		}
		w := Widget{
			ID: core.NewID(), Name: "Support", PublicKey: "pk_test", Secret: "sk_test",
			Origins: []string{"https://shop.example"}, Team: []string{ada.ID},
			Greeting: "Hi!", Enabled: true, UpdatedAt: now, CreatedAt: now,
		}
		if err := s.CreateWidget(ctx, w); err != nil {
			t.Fatal(err)
		}
		got, err := s.WidgetByKey(ctx, "pk_test")
		if err != nil || got.ID != w.ID || !slices.Equal(got.Origins, w.Origins) || !slices.Equal(got.Team, w.Team) || got.Secret != "sk_test" {
			t.Fatalf("WidgetByKey = %+v, %v", got, err)
		}
		if _, err := s.WidgetByKey(ctx, "pk_nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown key: %v", err)
		}
		if !got.Enabled {
			t.Fatal("created widget is off")
		}
		w.Name, w.Origins, w.Team, w.Enabled = "Help", []string{"https://a.example", "https://b.example"}, nil, false
		if err := s.UpdateWidget(ctx, w); err != nil {
			t.Fatal(err)
		}
		if got, err = s.GetWidget(ctx, w.ID); err != nil || got.Name != "Help" || len(got.Origins) != 2 || len(got.Team) != 0 || got.PublicKey != "pk_test" || got.Enabled {
			t.Fatalf("GetWidget after update = %+v, %v", got, err)
		}
		if err := s.UpdateWidget(ctx, Widget{ID: "nope"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update unknown: %v", err)
		}

		visit := func(subject string) (Visitor, error) {
			guest := core.Principal{ID: core.NewID(), Kind: core.PrincipalGuest, DisplayName: "Visitor", CreatedAt: now}
			room := core.Room{ID: core.NewID(), Kind: core.RoomWidgetThread, Name: "Help · Visitor", CreatedAt: now}
			ms := []core.Membership{
				{RoomID: room.ID, PrincipalID: guest.ID, Role: core.RoleMember},
				{RoomID: room.ID, PrincipalID: ada.ID, Role: core.RoleAdmin},
			}
			v := Visitor{PrincipalID: guest.ID, WidgetID: w.ID, Subject: subject, RoomID: room.ID, CreatedAt: now}
			return v, s.CreateVisitor(ctx, guest, room, ms, v)
		}
		// Anonymous visitors never collide.
		for range 2 {
			if _, err := visit(""); err != nil {
				t.Fatal(err)
			}
		}
		v, err := visit("user-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := visit("user-1"); !errors.Is(err, ErrVisitorExists) {
			t.Fatalf("second visitor with the same subject: %v", err)
		}
		if got, err := s.VisitorBySubject(ctx, w.ID, "user-1"); err != nil || got != v {
			t.Fatalf("VisitorBySubject = %+v, %v; want %+v", got, err, v)
		}
		if got, err := s.VisitorByPrincipal(ctx, v.PrincipalID); err != nil || got != v {
			t.Fatalf("VisitorByPrincipal = %+v, %v", got, err)
		}
		if _, err := s.VisitorByPrincipal(ctx, ada.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("non-visitor: %v", err)
		}
		// The failed visit left nothing behind.
		rooms, err := s.RoomsFor(ctx, ada.ID)
		if err != nil || len(rooms) != 3 {
			t.Fatalf("Ada's rooms = %d, %v; want 3", len(rooms), err)
		}
		if ms, err := s.Members(ctx, v.RoomID); err != nil || len(ms) != 2 {
			t.Fatalf("thread members = %v, %v", ms, err)
		}

		// An anonymous visitor who signs in keeps its guest and thread.
		anon, err := visit("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.VerifyVisitor(ctx, anon.PrincipalID, "user-1", "", ""); !errors.Is(err, ErrVisitorExists) {
			t.Fatalf("verify as a subject that has a visitor: %v", err)
		}
		if _, err := s.VerifyVisitor(ctx, v.PrincipalID, "user-2", "", ""); !errors.Is(err, ErrVisitorExists) {
			t.Fatalf("verify a verified visitor: %v", err)
		}
		verified, err := s.VerifyVisitor(ctx, anon.PrincipalID, "user-2", "Grace", "Help · Grace")
		if want := (Visitor{PrincipalID: anon.PrincipalID, WidgetID: w.ID, Subject: "user-2", RoomID: anon.RoomID, CreatedAt: now}); err != nil || verified != want {
			t.Fatalf("VerifyVisitor = %+v, %v; want %+v", verified, err, want)
		}
		if got, err := s.VisitorBySubject(ctx, w.ID, "user-2"); err != nil || got.PrincipalID != anon.PrincipalID {
			t.Fatalf("VisitorBySubject after verify = %+v, %v", got, err)
		}
		if p, err := s.GetPrincipal(ctx, anon.PrincipalID); err != nil || p.DisplayName != "Grace" {
			t.Fatalf("guest after verify = %+v, %v", p, err)
		}
		if r, err := s.GetRoom(ctx, anon.RoomID); err != nil || r.Name != "Help · Grace" {
			t.Fatalf("thread after verify = %+v, %v", r, err)
		}

		// One who signs in as a subject that has a visitor is linked to it.
		other, err := visit("")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.LinkVisitor(ctx, other.PrincipalID, v.PrincipalID); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkVisitor(ctx, v.PrincipalID, anon.PrincipalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("link a verified visitor: %v", err)
		}
		other.SignedInAs = v.PrincipalID
		if got, err := s.VisitorByPrincipal(ctx, other.PrincipalID); err != nil || got != other {
			t.Fatalf("linked visitor = %+v, %v; want %+v", got, err, other)
		}
		threads, err := s.WidgetThreads(ctx, ada.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, th := range threads {
			want := ""
			if th.PrincipalID == other.PrincipalID {
				want = v.RoomID
			}
			if th.SignedInRoomID != want {
				t.Fatalf("thread %+v: SignedInRoomID = %q, want %q", th.Visitor, th.SignedInRoomID, want)
			}
		}

		// Deleting the widget signs its visitors out and keeps their threads.
		token, err := s.CreateToken(ctx, v.PrincipalID, "widget", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteWidget(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetWidget(ctx, w.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted widget: %v", err)
		}
		if _, err := s.VisitorByPrincipal(ctx, v.PrincipalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("visitor of a deleted widget: %v", err)
		}
		if _, err := s.PrincipalByToken(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("visitor token after delete: %v", err)
		}
		if _, err := s.GetRoom(ctx, v.RoomID); err != nil {
			t.Fatalf("thread after delete: %v", err)
		}
		if err := s.DeleteWidget(ctx, w.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete again: %v", err)
		}
	})
}

func TestDeleteVisitor(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		now := time.Now().Truncate(time.Millisecond)
		ada, other := seed(t, s)
		w := Widget{ID: core.NewID(), Name: "Support", PublicKey: "pk_test", Secret: "sk_test", Origins: []string{"https://shop.example"}, Team: []string{ada.ID}, UpdatedAt: now, CreatedAt: now}
		if err := s.CreateWidget(ctx, w); err != nil {
			t.Fatal(err)
		}
		visit := func() Visitor {
			guest := core.Principal{ID: core.NewID(), Kind: core.PrincipalGuest, DisplayName: "Visitor", CreatedAt: now}
			room := core.Room{ID: core.NewID(), Kind: core.RoomWidgetThread, Name: "Support · Visitor", CreatedAt: now}
			ms := []core.Membership{
				{RoomID: room.ID, PrincipalID: guest.ID, Role: core.RoleMember},
				{RoomID: room.ID, PrincipalID: ada.ID, Role: core.RoleAdmin},
			}
			v := Visitor{PrincipalID: guest.ID, WidgetID: w.ID, Subject: "user-" + guest.ID, RoomID: room.ID, CreatedAt: now}
			if err := s.CreateVisitor(ctx, guest, room, ms, v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		say := func(room, sender, text string) core.Event {
			e, err := s.AppendEvent(ctx, core.Event{RoomID: room, Type: core.EventMessage, SenderID: sender, Parts: []core.Part{core.TextPart(text)}}, AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		v, kept := visit(), visit()
		say(v.RoomID, v.PrincipalID, "hello")
		say(v.RoomID, ada.ID, "hi, Ada here")
		say(kept.RoomID, kept.PrincipalID, "I stay")
		if err := s.SetAgentCursor(ctx, ada.ID, v.RoomID, 2); err != nil {
			t.Fatal(err)
		}
		// The visitor was also added to another room, and wrote there.
		if err := s.AddMember(ctx, core.Membership{RoomID: other.ID, PrincipalID: v.PrincipalID, Role: core.RoleMember}); err != nil {
			t.Fatal(err)
		}
		say(other.ID, ada.ID, "before")
		elsewhere := say(other.ID, v.PrincipalID, "me too")
		token, err := s.CreateToken(ctx, v.PrincipalID, "widget", now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}

		if err := s.DeleteVisitor(ctx, v.PrincipalID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.VisitorByPrincipal(ctx, v.PrincipalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("visitor after delete: %v", err)
		}
		if _, err := s.GetPrincipal(ctx, v.PrincipalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("guest after delete: %v", err)
		}
		if _, err := s.GetRoom(ctx, v.RoomID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("thread after delete: %v", err)
		}
		if _, err := s.PrincipalByToken(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Fatalf("token after delete: %v", err)
		}
		if _, err := s.GetEvent(ctx, elsewhere.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("message in another room after delete: %v", err)
		}
		if es, err := s.EventsAfter(ctx, other.ID, 0, 10); err != nil || len(es) != 1 || es[0].SenderID != ada.ID {
			t.Fatalf("other room's events = %+v, %v", es, err)
		}
		if ms, err := s.Members(ctx, other.ID); err != nil || len(ms) != 1 {
			t.Fatalf("other room's members = %+v, %v", ms, err)
		}
		rooms, err := s.RoomsFor(ctx, ada.ID)
		if err != nil || slices.ContainsFunc(rooms, func(r core.Room) bool { return r.ID == v.RoomID }) {
			t.Fatalf("Ada's rooms = %+v, %v", rooms, err)
		}
		// The appending a late agent reply would do finds no room.
		if _, err := s.AppendEvent(ctx, core.Event{RoomID: v.RoomID, Type: core.EventMessage, SenderID: ada.ID, Parts: []core.Part{core.TextPart("late")}}, AppendOptions{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("append to the deleted thread: %v", err)
		}
		// Other visitors are untouched.
		if es, err := s.EventsAfter(ctx, kept.RoomID, 0, 10); err != nil || len(es) != 1 {
			t.Fatalf("other visitor's thread = %+v, %v", es, err)
		}
		if err := s.DeleteVisitor(ctx, v.PrincipalID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
		if err := s.DeleteVisitor(ctx, ada.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete a non-visitor: %v", err)
		}
	})
}

func TestWidgetThreads(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		now := time.Now().Truncate(time.Millisecond)
		person := func(kind core.PrincipalKind, name string) core.Principal {
			p := core.Principal{ID: core.NewID(), Kind: kind, DisplayName: name, CreatedAt: now}
			if err := s.CreatePrincipal(ctx, p); err != nil {
				t.Fatal(err)
			}
			return p
		}
		ada, bob, bot := person(core.PrincipalHuman, "Ada"), person(core.PrincipalHuman, "Bob"), person(core.PrincipalAgent, "Bot")
		w := Widget{
			ID: core.NewID(), Name: "Support", PublicKey: "pk_test", Secret: "sk_test",
			Origins: []string{"https://shop.example"}, AgentID: bot.ID, Team: []string{ada.ID},
			UpdatedAt: now, CreatedAt: now,
		}
		if err := s.CreateWidget(ctx, w); err != nil {
			t.Fatal(err)
		}
		visit := func(name, subject string) Visitor {
			guest := core.Principal{ID: core.NewID(), Kind: core.PrincipalGuest, DisplayName: name, CreatedAt: now}
			room := core.Room{ID: core.NewID(), Kind: core.RoomWidgetThread, Name: "Support · " + name, CreatedAt: now}
			ms := []core.Membership{
				{RoomID: room.ID, PrincipalID: guest.ID, Role: core.RoleMember},
				{RoomID: room.ID, PrincipalID: ada.ID, Role: core.RoleAdmin},
				{RoomID: room.ID, PrincipalID: bot.ID, Role: core.RoleMember, Scopes: []string{core.ScopeResponder}},
			}
			v := Visitor{PrincipalID: guest.ID, WidgetID: w.ID, Subject: subject, RoomID: room.ID, CreatedAt: now}
			if err := s.CreateVisitor(ctx, guest, room, ms, v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		say := func(room, sender string) core.Event {
			e, err := s.AppendEvent(ctx, core.Event{
				RoomID: room, Type: core.EventMessage, SenderID: sender, Parts: text("hi"), Stream: core.StreamFinal,
			}, AppendOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return e
		}
		quiet := visit("Quiet", "")
		cat := visit("Cat", "user-1")
		dan := visit("Dan", "")
		say(cat.RoomID, cat.PrincipalID)
		time.Sleep(2 * time.Millisecond)
		say(dan.RoomID, dan.PrincipalID)
		last := say(dan.RoomID, bot.ID)
		time.Sleep(2 * time.Millisecond)
		// Cat's thread is handed over and has the latest message, a visitor's.
		if err := s.SetMemberScopes(ctx, cat.RoomID, bot.ID, nil); err != nil {
			t.Fatal(err)
		}
		catLast := say(cat.RoomID, cat.PrincipalID)

		got, err := s.WidgetThreads(ctx, ada.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		var rooms []string
		for _, th := range got {
			rooms = append(rooms, th.RoomID)
		}
		if !slices.Equal(rooms, []string{cat.RoomID, dan.RoomID, quiet.RoomID}) {
			t.Fatalf("thread order = %v; want Cat, Dan, Quiet", rooms)
		}
		c, d, q := got[0], got[1], got[2]
		if c.Visitor != cat || c.WidgetName != "Support" || c.VisitorName != "Cat" || c.AgentAnswers ||
			c.LastSenderID != cat.PrincipalID || !c.LastMessageAt.Equal(catLast.CreatedAt) {
			t.Fatalf("Cat's thread = %+v", c)
		}
		if !d.AgentAnswers || d.LastSenderID != bot.ID || !d.LastMessageAt.Equal(last.CreatedAt) {
			t.Fatalf("Dan's thread = %+v", d)
		}
		if !q.AgentAnswers || q.LastSenderID != "" || !q.LastMessageAt.IsZero() {
			t.Fatalf("Quiet's thread = %+v", q)
		}
		if got, err := s.WidgetThreads(ctx, ada.ID, 1); err != nil || len(got) != 1 || got[0].RoomID != cat.RoomID {
			t.Fatalf("limit 1 = %+v, %v", got, err)
		}
		// Only members see a thread.
		if got, err := s.WidgetThreads(ctx, bob.ID, 10); err != nil || len(got) != 0 {
			t.Fatalf("Bob's threads = %+v, %v", got, err)
		}
	})
}
