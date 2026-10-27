package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lum1n/say-server/say/release"
	"github.com/lum1n/say-server/say/release/releasetest"
)

// fakeSay is say.service: Start runs whatever say binary is installed,
// which here is a script that prints its version, and serves its health.
// A script containing "unhealthy" starts but fails its health check. A
// newer version changes the data, as a migration would.
type fakeSay struct {
	u       *Updater
	mu      sync.Mutex
	running string
	healthy bool
	stops   int
}

func (f *fakeSay) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = ""
	f.stops++
	return nil
}

func (f *fakeSay) Start(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, f.u.Say, "version").Output()
	if err != nil {
		return err
	}
	script, _ := os.ReadFile(f.u.Say)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = strings.TrimSpace(string(out))
	f.healthy = !bytes.Contains(script, []byte("unhealthy"))
	if f.running != "0.4.0" {
		os.WriteFile(filepath.Join(f.u.Data, "say.db"), []byte("migrated by "+f.running), 0o600)
	}
	return nil
}

func (f *fakeSay) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == "" || !f.healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, `{"status":"ok","version":%q}`, f.running)
}

func script(version, extra string) []byte {
	return []byte("#!/bin/sh\n# " + extra + "\necho " + version + "\n")
}

type env struct {
	u    *Updater
	say  *fakeSay
	srv  *releasetest.Server
	root string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"bin", "lib", "data/signal", "work"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	write := func(name string, b []byte, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, name), b, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/say", script("0.4.0", "old"), 0o755)
	write("bin/signal-cli", []byte("old signal-cli"), 0o755)
	write("lib/signal-cli.version", []byte("0.14.8\n"), 0o644)
	write("data/say.db", []byte("0.4.0 data"), 0o600)
	write("data/signal/key", []byte("signal key"), 0o600)
	os.Symlink("say.db", filepath.Join(root, "data/link"))
	write("data/"+RequestFile, nil, 0o644)

	signer := releasetest.NewSigner()
	u := &Updater{
		Key:              signer.Public,
		HTTP:             http.DefaultClient,
		Version:          "0.4.0",
		Platform:         "linux-amd64",
		Say:              filepath.Join(root, "bin/say"),
		SignalCLI:        filepath.Join(root, "bin/signal-cli"),
		SignalCLIVersion: filepath.Join(root, "lib/signal-cli.version"),
		Data:             filepath.Join(root, "data"),
		Work:             filepath.Join(root, "work"),
		Status:           filepath.Join(root, "data", StatusFile),
		Request:          filepath.Join(root, "data", RequestFile),
		HealthWait:       2 * time.Second,
		Log:              slog.New(slog.DiscardHandler),
	}
	e := &env{u: u, say: &fakeSay{u: u}, srv: releasetest.NewServer(t, signer), root: root}
	u.Service = e.say
	u.Manifest = e.srv.ManifestURL()
	hs := httptest.NewServer(e.say)
	t.Cleanup(hs.Close)
	u.Health = hs.URL
	e.say.Start(context.Background())
	return e
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func tgz(name string, b []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "README", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg})
	tw.Write(b)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// publish serves a release of version whose say is sayBin, with signal-cli
// sigVersion.
func (e *env) publish(version string, sayBin []byte, sigVersion string) release.Manifest {
	m := release.Manifest{
		Version: version,
		Say: map[string]release.Download{
			"linux-amd64": {URL: e.srv.File("/say-"+version, sayBin), SHA256: sum(sayBin)},
		},
	}
	sig := tgz("signal-cli", []byte("signal-cli "+sigVersion))
	m.SignalCLI.Version = sigVersion
	m.SignalCLI.LinuxAMD64 = &release.Download{URL: e.srv.File("/signal-cli-"+sigVersion+".tgz", sig), SHA256: sum(sig)}
	e.srv.Publish(m)
	return m
}

func (e *env) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) status(t *testing.T) Status {
	t.Helper()
	st, err := ReadStatus(e.u.Status)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// unchanged checks that the node is still 0.4.0 with its own data, and
// running.
func (e *env) unchanged(t *testing.T) {
	t.Helper()
	if got := e.read(t, "bin/say"); got != string(script("0.4.0", "old")) {
		t.Errorf("say = %q", got)
	}
	if got := e.read(t, "bin/signal-cli"); got != "old signal-cli" {
		t.Errorf("signal-cli = %q", got)
	}
	if got := e.read(t, "lib/signal-cli.version"); got != "0.14.8\n" {
		t.Errorf("signal-cli version = %q", got)
	}
	if got := e.read(t, "data/say.db"); got != "0.4.0 data" {
		t.Errorf("data = %q", got)
	}
	if got := e.read(t, "data/link"); got != "0.4.0 data" {
		t.Errorf("symlink = %q", got)
	}
	if e.say.running != "0.4.0" {
		t.Errorf("running %q", e.say.running)
	}
}

func TestUpToDate(t *testing.T) {
	e := newEnv(t)
	e.publish("0.4.0", script("0.4.0", "same"), "0.14.9")
	if err := e.u.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if st := e.status(t); st.State != StateOK || st.Latest != "0.4.0" || st.CheckedAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	if e.say.stops != 0 {
		t.Fatal("say was stopped")
	}
	if _, err := os.Stat(e.u.Request); err == nil {
		t.Fatal("the request is still there")
	}
	e.unchanged(t)
}

func TestCheckOnly(t *testing.T) {
	e := newEnv(t)
	e.publish("0.4.1", script("0.4.1", "new"), "0.14.9")
	if err := e.u.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if st := e.status(t); st.State != StateOK || st.Latest != "0.4.1" {
		t.Fatalf("status = %+v", st)
	}
	e.unchanged(t)
}

func TestUpdate(t *testing.T) {
	e := newEnv(t)
	e.publish("0.4.1", script("0.4.1", "new"), "0.14.9")
	if err := e.u.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	st := e.status(t)
	if st.State != StateOK || st.Latest != "0.4.1" || st.UpdatedFrom != "0.4.0" || st.UpdatedAt.IsZero() || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	if e.say.running != "0.4.1" {
		t.Fatalf("running %q", e.say.running)
	}
	for name, want := range map[string]string{
		"bin/say":                string(script("0.4.1", "new")),
		"bin/.say.prev":          string(script("0.4.0", "old")),
		"bin/signal-cli":         "signal-cli 0.14.9",
		"bin/.signal-cli.prev":   "old signal-cli",
		"lib/signal-cli.version": "0.14.9\n",
		"data/say.db":            "migrated by 0.4.1",
		"work/backup/say.db":     "0.4.0 data",
		"work/backup/signal/key": "signal key",
		"work/backup/link":       "0.4.0 data",
	} {
		if got := e.read(t, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(e.root, "work/backup/signal/key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("backup's mode: %v, %v", fi, err)
	}
	for _, name := range []string{"bin/.say.new", "work/new"} {
		if _, err := os.Stat(filepath.Join(e.root, name)); err == nil {
			t.Errorf("%s is left over", name)
		}
	}

	// The same signal-cli isn't downloaded again.
	e.u.Version = "0.4.1"
	e.publish("0.4.2", script("0.4.2", "newer"), "0.14.9")
	e.srv.File("/signal-cli-0.14.9.tgz", []byte("gone"))
	if err := e.u.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := e.read(t, "bin/signal-cli"); got != "signal-cli 0.14.9" {
		t.Errorf("signal-cli = %q", got)
	}
	if got := e.read(t, "work/backup/say.db"); got != "migrated by 0.4.1" {
		t.Errorf("the backup isn't the last version's: %q", got)
	}
}

// Nothing is touched, and say keeps running, when a download is wrong.
func TestBadDownload(t *testing.T) {
	for name, tamper := range map[string]func(e *env, m release.Manifest){
		"say checksum": func(e *env, m release.Manifest) { e.srv.File("/say-0.4.1", script("0.4.1", "evil")) },
		"signal-cli checksum": func(e *env, m release.Manifest) {
			e.srv.File("/signal-cli-0.14.9.tgz", tgz("signal-cli", []byte("evil")))
		},
		"say isn't the version": func(e *env, m release.Manifest) {
			b := script("0.4.0", "liar")
			m.Say["linux-amd64"] = release.Download{URL: e.srv.File("/say-0.4.1", b), SHA256: sum(b)}
			e.srv.Publish(m)
		},
		"signature": func(e *env, m release.Manifest) {
			e.srv.File("/manifest.json.minisig", releasetest.NewSigner().Sign([]byte("x"), "x"))
		},
		"no say for this platform": func(e *env, m release.Manifest) {
			delete(m.Say, "linux-amd64")
			e.srv.Publish(m)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			tamper(e, e.publish("0.4.1", script("0.4.1", "new"), "0.14.9"))
			err := e.u.Run(context.Background(), true)
			if err == nil {
				t.Fatal("no error")
			}
			if st := e.status(t); st.State != StateFailed || st.Error == "" {
				t.Fatalf("status = %+v", st)
			}
			if e.say.stops != 0 {
				t.Fatal("say was stopped")
			}
			e.unchanged(t)
		})
	}
}

func TestRollback(t *testing.T) {
	e := newEnv(t)
	e.publish("0.4.1", script("0.4.1", "unhealthy"), "0.14.9")
	err := e.u.Run(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Run = %v", err)
	}
	if st := e.status(t); st.State != StateRolledBack || !strings.Contains(st.Error, "healthy") || st.Latest != "0.4.1" {
		t.Fatalf("status = %+v", st)
	}
	e.unchanged(t)
	if _, err := os.Stat(filepath.Join(e.root, "data/signal/key")); err != nil {
		t.Fatal(err)
	}
}

// A signal-cli without a version file, such as the JVM build, isn't the
// updater's to replace.
func TestSignalCLIWithoutVersion(t *testing.T) {
	e := newEnv(t)
	os.Remove(e.u.SignalCLIVersion)
	e.publish("0.4.1", script("0.4.1", "new"), "0.14.9")
	if err := e.u.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := e.read(t, "bin/signal-cli"); got != "old signal-cli" {
		t.Errorf("signal-cli = %q", got)
	}
	if got := e.read(t, "bin/say"); got != string(script("0.4.1", "new")) {
		t.Errorf("say = %q", got)
	}
}

func TestReadStatusMissing(t *testing.T) {
	st, err := ReadStatus(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || st != (Status{}) {
		t.Fatalf("%+v, %v", st, err)
	}
}
