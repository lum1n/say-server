package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A socket the server closes as signed out isn't reconnected.
func TestRealtimeSignedOut(t *testing.T) {
	dials := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials++
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.Close(statusSignedOut, "signed out")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, err := New(srv.URL, "tok").Realtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p := range rt.Pushes() {
		got = append(got, p.Type)
	}
	if len(got) != 1 || got[0] != "signed_out" || ctx.Err() != nil {
		t.Fatalf("pushes = %v, ctx %v", got, ctx.Err())
	}
	if dials != 1 {
		t.Fatalf("dialed %d times", dials)
	}
}
