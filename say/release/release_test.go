package release_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/lum1n/say-v2/say/release"
	"github.com/lum1n/say-v2/say/release/releasetest"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixtures come from minisign itself, with a throwaway key.
func TestVerifyMinisign(t *testing.T) {
	key, err := release.ParsePublicKey(read(t, "test.pub"))
	if err != nil {
		t.Fatal(err)
	}
	msg := read(t, "manifest.json")
	for _, sig := range []string{"manifest.json.minisig", "manifest.json.legacy.minisig"} {
		if err := key.Verify(msg, read(t, sig)); err != nil {
			t.Errorf("%s: %v", sig, err)
		}
	}
	sig := read(t, "manifest.json.minisig")
	if err := key.Verify(bytes.Replace(msg, []byte("0.4.1"), []byte("0.4.2"), 1), sig); err == nil {
		t.Error("a changed manifest verified")
	}
	if err := key.Verify(msg, bytes.Replace(sig, []byte("v0.4.1"), []byte("v9.9.9"), 1)); err == nil {
		t.Error("a changed trusted comment verified")
	}
	if err := release.Key.Verify(msg, sig); err == nil {
		t.Error("the release key verified the test key's signature")
	}
}

func TestPinnedKey(t *testing.T) {
	if release.Key.Key == nil {
		t.Fatal("no pinned key")
	}
}

func TestFetch(t *testing.T) {
	ctx := context.Background()
	s := releasetest.NewSigner()
	srv := releasetest.NewServer(t, s)
	m := release.Manifest{Version: "0.4.1", Say: map[string]release.Download{"linux-amd64": {URL: "u", SHA256: "h"}}}
	m.SignalCLI.Version = "0.14.9"
	m.SignalCLI.LinuxAMD64 = &release.Download{URL: "s", SHA256: "x"}
	srv.Publish(m)
	got, err := release.Fetch(ctx, http.DefaultClient, srv.ManifestURL(), s.Public)
	if err != nil || got.Version != "0.4.1" || got.Say["linux-amd64"].SHA256 != "h" || got.SignalCLI.LinuxAMD64.URL != "s" {
		t.Fatalf("Fetch = %+v, %v", got, err)
	}
	other := releasetest.NewSigner()
	if _, err := release.Fetch(ctx, http.DefaultClient, srv.ManifestURL(), other.Public); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("another key: %v", err)
	}
	m.Version = "0.4.2-rc.1"
	srv.Publish(m)
	if _, err := release.Fetch(ctx, http.DefaultClient, srv.ManifestURL(), s.Public); err == nil {
		t.Fatal("a prerelease manifest was taken")
	}
	if _, err := release.Fetch(ctx, http.DefaultClient, srv.URL+"/nothing.json", s.Public); err == nil {
		t.Fatal("no manifest, no error")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"0.4.1", "0.4.0", true},
		{"0.4.1", "0.4.1", false},
		{"0.4.1", "0.5.0", false},
		{"0.10.0", "0.9.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.4.1", "0.4.1-dev", true},
		{"0.4.1", "v0.4.0", true},
		{"0.4.1", "dev", true},
		{"0.4.2-rc.1", "0.4.1", false},
		{"nonsense", "0.4.1", false},
	} {
		if got := release.Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}
