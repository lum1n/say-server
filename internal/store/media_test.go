package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lum1n/say-server/internal/core"
)

func TestMedia(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		m := Media{ID: "m1", RoomID: r.ID, UploaderID: p.ID, Mime: "image/png", Name: "dot.png", Size: 70, Width: 4, Height: 3, SHA256: "ab"}
		if err := s.CreateMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetMedia(ctx, "m1")
		if err != nil || got.Name != "dot.png" || got.Width != 4 || got.Height != 3 || got.Size != 70 || got.CreatedAt.IsZero() {
			t.Fatalf("GetMedia = %+v, %v", got, err)
		}
		if _, err := s.GetMedia(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing media: %v", err)
		}

		// The key is made once and then always the same.
		k1, err := s.InstanceKey(ctx, "media")
		if err != nil || len(k1) != 32 {
			t.Fatalf("InstanceKey = %x, %v", k1, err)
		}
		k2, _ := s.InstanceKey(ctx, "media")
		other, _ := s.InstanceKey(ctx, "other")
		if !bytes.Equal(k1, k2) || bytes.Equal(k1, other) {
			t.Fatalf("keys %x %x %x", k1, k2, other)
		}
	})
}

func TestDeleteUnusedMedia(t *testing.T) {
	eachEngine(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		p, r := seed(t, s)
		for _, id := range []string{"used", "unused", "fresh"} {
			if err := s.CreateMedia(ctx, Media{ID: id, RoomID: r.ID, UploaderID: p.ID, Mime: "image/png", Name: id, Size: 1, SHA256: "x"}); err != nil {
				t.Fatal(err)
			}
		}
		e := core.Event{RoomID: r.ID, Type: core.EventMessage, SenderID: p.ID, Parts: []core.Part{core.Attachment{Media: "used", Mime: "image/png", Name: "used"}.Part()}}
		if _, err := s.AppendEvent(ctx, e, AppendOptions{}); err != nil {
			t.Fatal(err)
		}
		// "fresh" is too new to judge; "unused" has no message.
		if err := s.CreateMedia(ctx, Media{ID: "old", RoomID: r.ID, UploaderID: p.ID, Mime: "image/png", Name: "old", Size: 1, SHA256: "x", CreatedAt: time.Now().Add(-48 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		ids, err := s.DeleteUnusedMedia(ctx, time.Now().Add(-24*time.Hour))
		if err != nil || len(ids) != 1 || ids[0] != "old" {
			t.Fatalf("DeleteUnusedMedia = %v, %v", ids, err)
		}
		if _, err := s.GetMedia(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old still there: %v", err)
		}
		ids, err = s.DeleteUnusedMedia(ctx, time.Now().Add(time.Hour))
		if err != nil || len(ids) != 2 {
			t.Fatalf("later cutoff = %v, %v (want unused and fresh, not used)", ids, err)
		}
		if _, err := s.GetMedia(ctx, "used"); err != nil {
			t.Fatalf("used media gone: %v", err)
		}
	})
}
