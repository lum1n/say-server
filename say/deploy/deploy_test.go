package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"
)

func TestNewSetupCodeAndName(t *testing.T) {
	code := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$`)
	name := regexp.MustCompile(`^say-[0-9a-hjkmnp-tv-z]{6}$`)
	seen := map[string]bool{}
	for range 100 {
		c, n := NewSetupCode(), NewName()
		if !code.MatchString(c) || !name.MatchString(n) || seen[c] {
			t.Fatalf("code %q, name %q", c, n)
		}
		seen[c] = true
	}
}

func TestLatestImage(t *testing.T) {
	ctx := context.Background()
	for body, want := range map[string]string{
		`{"version":"0.4.1"}`:               Image + ":0.4.1",
		`{"version":"v0.4.1"}`:              Image + ":0.4.1",
		`{"version":"0.4.1-rc.1"}`:          StableImage,
		`{"version":"0.4.1 --env X=1"}`:     StableImage,
		`not json`:                          StableImage,
		`{"name":"no version here at all"}`: StableImage,
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		if got := LatestImage(ctx, http.DefaultClient, ts.URL); got != want {
			t.Errorf("%s: LatestImage = %q, want %q", body, got, want)
		}
		ts.Close()
	}
	if got := LatestImage(ctx, http.DefaultClient, "http://127.0.0.1:1/manifest.json"); got != StableImage {
		t.Errorf("unreachable manifest: %q", got)
	}
}

func TestWaitOpen(t *testing.T) {
	ctx := context.Background()
	answer := func(body string) string {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/setup" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, body)
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	for body, want := range map[string]error{
		`{"needed":true,"open":true}`:   nil,
		`{"needed":true}`:               nil, // before setup windows
		`{"needed":true,"open":false}`:  ErrSetupClosed,
		`{"needed":false,"open":false}`: ErrSetupDone,
	} {
		if err := WaitOpen(ctx, http.DefaultClient, answer(body), time.Second); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", body, err, want)
		}
	}
	// A node that never answers runs out of time.
	if err := WaitOpen(ctx, http.DefaultClient, "http://127.0.0.1:1", 100*time.Millisecond); err == nil {
		t.Error("WaitOpen on nothing succeeded")
	}
}
