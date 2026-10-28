package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
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
