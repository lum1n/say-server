package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/vegard/say-v2/internal/core"
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
