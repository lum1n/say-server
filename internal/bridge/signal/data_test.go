package signal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lum1n/say-v2/internal/bridge"
)

func (h *host) raw() json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestLinkingKeepsData(t *testing.T) {
	h := newHost(0)
	r := startOwn(t, h, t.TempDir(), false, nil)
	select {
	case <-h.prompts:
	case err := <-r.done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no QR prompt")
	}
	os.WriteFile(filepath.Join(r.dir, "scanned"), nil, 0o600)
	r.waitConnected()
	s := h.saved()
	if s.Account != fakeNumber || len(s.Data) == 0 || s.DataID == "" {
		t.Fatalf("state %+v", s)
	}
	if got := readFile(t, filepath.Join(r.dir, dataIDFile)); got != s.DataID {
		t.Errorf("data id %q, saved %q", got, s.DataID)
	}
	// The copy has the account, which was kept before receiving began.
	out := t.TempDir()
	if err := unpack(s.Data, out); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(out, "account")); got != fakeNumber {
		t.Errorf("account in the copy: %q", got)
	}
	if n := len(r.requests("startLink")); n != 1 {
		t.Errorf("linked %d times", n)
	}
}

// A bridge moving between nodes stays the same device, and a node's own
// data is kept when it is the latest.
func TestDataMovesWithBridge(t *testing.T) {
	homeA, homeB := t.TempDir(), t.TempDir()
	st, _ := json.Marshal(state{Account: fakeNumber})

	// Node A has the account; it stops, as when it loses the lease.
	h := newHost(0)
	a := startOwn(t, h, homeA, true, st)
	a.waitConnected()
	os.WriteFile(filepath.Join(a.dir, "data", "keys"), []byte("a"), 0o600)
	a.end()
	st = h.raw()

	// Node B takes over without linking again.
	h = newHost(0)
	b := startOwn(t, h, homeB, false, st)
	b.waitConnected()
	if n := len(b.requests("startLink")); n != 0 {
		t.Fatalf("node B linked again")
	}
	if got := readFile(t, filepath.Join(b.dir, "data", "keys")); got != "a" {
		t.Fatalf("node B's keys: %q", got)
	}
	os.WriteFile(filepath.Join(b.dir, "data", "keys"), []byte("b"), 0o600)
	b.end()
	st = h.raw()

	// Back on node A, B's newer data replaces A's.
	h = newHost(0)
	a = startOwn(t, h, homeA, false, st)
	a.waitConnected()
	if got := readFile(t, filepath.Join(a.dir, "data", "keys")); got != "b" {
		t.Fatalf("node A's keys: %q", got)
	}
	a.end()
	st = h.raw()

	// Data newer than the copy, as after a failed save, stays.
	os.WriteFile(filepath.Join(a.dir, "data", "keys"), []byte("c"), 0o600)
	h = newHost(0)
	a = startOwn(t, h, homeA, false, st)
	a.waitConnected()
	if got := readFile(t, filepath.Join(a.dir, "data", "keys")); got != "c" {
		t.Fatalf("node A's own keys: %q", got)
	}
	a.end()
	st = h.raw()

	// Logging out on a third node unregisters the device too.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	log := filepath.Join(t.TempDir(), "commands")
	t.Setenv(fakeLogEnv, log)
	cfg, _ := json.Marshal(config{Path: fakeExe(t)})
	if err := logOut(context.Background(), bridge.Info{ID: "b1", Config: cfg, State: st}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, log); !strings.Contains(got, `"command":"unregister"`) {
		t.Errorf("commands: %s", got)
	}
}

func TestSetDataDirStays(t *testing.T) {
	r := start(t, true, &state{Account: fakeNumber})
	r.waitConnected()
	r.end()
	if s := r.h.saved(); len(s.Data) != 0 || s.DataID != "" {
		t.Fatalf("carried a set data directory: %+v", s)
	}
}

func TestUnpackStaysInside(t *testing.T) {
	for _, name := range []string{"../escape", "/abs", "a/../../escape"} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: 1})
		tw.Write([]byte("x"))
		tw.Close()
		zw.Close()
		if err := unpack(buf.Bytes(), t.TempDir()); err == nil {
			t.Errorf("unpacked %q", name)
		}
	}
}
