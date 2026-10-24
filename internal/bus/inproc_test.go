package bus

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestInProc(t *testing.T) {
	ctx := context.Background()
	b := NewInProc()

	var got []string
	unsubA, _ := b.Subscribe("room.1", func(_ string, d []byte) { got = append(got, "a:"+string(d)) })
	b.Subscribe("room.1", func(_ string, d []byte) { got = append(got, "b:"+string(d)) })
	b.Subscribe("room.2", func(_ string, d []byte) { got = append(got, "other") })

	if err := b.Publish(ctx, "room.1", []byte("x")); err != nil {
		t.Fatal(err)
	}
	unsubA()
	b.Publish(ctx, "room.1", []byte("y"))

	if want := []string{"a:x", "b:x", "b:y"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	b.Close()
	if err := b.Publish(ctx, "room.1", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("publish after close = %v", err)
	}
}
