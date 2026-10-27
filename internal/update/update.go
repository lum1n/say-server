// Package update is a say node's self-update (docs/adr/0017-own-node.md).
// A VM's filesystem comes from the image once, when the VM is made, so a
// new say-node image doesn't reach existing nodes. Instead `say update`,
// run as root by say-update.timer and whenever say asks, reads the newest
// release's signed manifest and, when it is newer:
//
//  1. downloads say, and signal-cli when its version changed, and checks
//     them against the manifest's SHA-256s;
//  2. stops say and copies its data aside, the backup kept until the next
//     update;
//  3. swaps the binaries in and starts say;
//  4. puts the old binaries and the data back if the new say doesn't
//     answer its health check with the new version.
//
// It reports what it did in a status file that say serves on
// GET /api/v1/node.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lum1n/say-server/say/release"
)

// The files in say's update directory (Config.UpdateDir in internal/api):
// the updater writes StatusFile, and say creates RequestFile to ask for
// an update, which starts say-update.service through say-update.path.
const (
	StatusFile  = "update.json"
	RequestFile = "update-request"
)

// States in Status.
const (
	StateChecking   = "checking"
	StateUpdating   = "updating"
	StateOK         = "ok"
	StateFailed     = "failed"      // nothing was changed
	StateRolledBack = "rolled_back" // the new version failed and the old one is back
)

// Status is the last run's outcome.
type Status struct {
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	Latest    string    `json:"latest,omitempty"` // the newest release, when the check worked
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// UpdatedAt and UpdatedFrom describe the last update that worked.
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	UpdatedFrom string    `json:"updated_from,omitempty"`
}

// ReadStatus reads a status file. A missing one is the zero Status.
func ReadStatus(path string) (Status, error) {
	var st Status
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// Service is say.service.
type Service interface {
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// Updater updates one node. Every field is required unless it says
// otherwise; Node fills them for the say-node image.
type Updater struct {
	Manifest string // the newest release's manifest URL
	Key      release.PublicKey
	HTTP     *http.Client
	Version  string // the installed say's
	Platform string // such as linux-amd64

	Say string // the say binary
	// SignalCLI is the signal-cli binary, and SignalCLIVersion a file
	// holding its version. signal-cli is left alone when either is
	// missing.
	SignalCLI        string
	SignalCLIVersion string

	Data   string // say's data directory, which is backed up
	Work   string // the updater's own directory: downloads and the backup
	Status string // the status file
	// Request is the file say makes to ask for an update; it is removed
	// when a run starts. Optional.
	Request string

	Service Service
	// Health is say's GET /api/v1/health, polled for HealthWait (default
	// 2 minutes) after the swap.
	Health     string
	HealthWait time.Duration
	Log        *slog.Logger
}

// Node is the Updater for the say-node image, running as version.
func Node(version string) *Updater {
	return &Updater{
		Manifest:         release.LatestURL,
		Key:              release.Key,
		HTTP:             &http.Client{Timeout: 10 * time.Minute},
		Version:          version,
		Platform:         runtime.GOOS + "-" + runtime.GOARCH,
		Say:              "/usr/local/bin/say",
		SignalCLI:        "/usr/local/bin/signal-cli",
		SignalCLIVersion: "/usr/local/lib/say/signal-cli.version",
		Data:             "/var/lib/say",
		Work:             "/var/lib/say-update",
		Status:           "/var/lib/say/" + StatusFile,
		Request:          "/var/lib/say/" + RequestFile,
		Service:          Systemd("say.service"),
		Health:           "http://127.0.0.1/api/v1/health",
		Log:              slog.Default(),
	}
}

// Run checks for a newer release and, if install is set, installs it.
func (u *Updater) Run(ctx context.Context, install bool) error {
	if u.Request != "" {
		os.Remove(u.Request)
	}
	st, _ := ReadStatus(u.Status)
	st.State, st.Error = StateChecking, ""
	u.write(st)

	m, err := release.Fetch(ctx, u.HTTP, u.Manifest, u.Key)
	if err != nil {
		st.State, st.Error = StateFailed, err.Error()
		u.write(st)
		return err
	}
	st.Latest, st.CheckedAt = m.Version, time.Now().UTC()
	if !install || !release.Newer(m.Version, u.Version) {
		u.Log.Info("update: checked", "version", u.Version, "latest", m.Version)
		st.State = StateOK
		u.write(st)
		return nil
	}

	u.Log.Info("update: installing", "from", u.Version, "to", m.Version)
	st.State = StateUpdating
	u.write(st)
	err = u.install(ctx, m)
	var rb *rolledBack
	switch {
	case err == nil:
		u.Log.Info("update: done", "version", m.Version)
		st.State, st.UpdatedAt, st.UpdatedFrom = StateOK, time.Now().UTC(), u.Version
	case errors.As(err, &rb):
		u.Log.Error("update: rolled back", "to", u.Version, "err", rb.err)
		st.State, st.Error = StateRolledBack, rb.err.Error()
	default:
		u.Log.Error("update: failed", "err", err)
		st.State, st.Error = StateFailed, err.Error()
	}
	u.write(st)
	return err
}

// rolledBack is an update that failed after the swap and was undone.
type rolledBack struct{ err error }

func (e *rolledBack) Error() string {
	return "the new version failed and was rolled back: " + e.err.Error()
}

func (u *Updater) write(st Status) {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := u.Status + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err == nil {
		os.Rename(tmp, u.Status)
	}
}

// swap is one binary to replace: new is staged next to path, and the old
// one is kept as prev.
type swap struct{ path, new, prev string }

func staged(path string) swap {
	dir, name := filepath.Split(path)
	return swap{path, filepath.Join(dir, "."+name+".new"), filepath.Join(dir, "."+name+".prev")}
}

func (u *Updater) install(ctx context.Context, m release.Manifest) error {
	dl, ok := m.Say[u.Platform]
	if !ok {
		return fmt.Errorf("release %s has no say for %s", m.Version, u.Platform)
	}
	dir := filepath.Join(u.Work, "new")
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	sayNew := filepath.Join(dir, "say")
	if err := u.download(ctx, dl, sayNew); err != nil {
		return fmt.Errorf("say: %w", err)
	}
	os.Chmod(sayNew, 0o755)
	if out, err := exec.CommandContext(ctx, sayNew, "version").Output(); err != nil || strings.TrimSpace(string(out)) != m.Version {
		return fmt.Errorf("the new say doesn't run, or isn't %s: %q, %v", m.Version, out, err)
	}
	files := map[string]string{u.Say: sayNew}

	sigVersion := ""
	if sig := m.SignalCLI; u.signalCLI() && u.Platform == "linux-amd64" && sig.LinuxAMD64 != nil && sig.Version != u.signalCLIVersion() {
		tgz := filepath.Join(dir, "signal-cli.tgz")
		if err := u.download(ctx, *sig.LinuxAMD64, tgz); err != nil {
			return fmt.Errorf("signal-cli: %w", err)
		}
		bin := filepath.Join(dir, "signal-cli")
		if err := untarOne(tgz, "signal-cli", bin); err != nil {
			return fmt.Errorf("signal-cli: %w", err)
		}
		files[u.SignalCLI], sigVersion = bin, sig.Version
	}

	var swaps []swap
	for path, src := range files {
		s := staged(path)
		if err := copyFile(src, s.new); err != nil {
			return err
		}
		defer os.Remove(s.new)
		swaps = append(swaps, s)
	}

	if err := u.Service.Stop(ctx); err != nil {
		return fmt.Errorf("stop say: %w", err)
	}
	backup := filepath.Join(u.Work, "backup")
	os.RemoveAll(backup)
	if err := copyTree(u.Data, backup); err != nil {
		return errors.Join(fmt.Errorf("back up %s: %w", u.Data, err), u.Service.Start(ctx))
	}
	oldSigVersion := u.signalCLIVersion()
	for i, s := range swaps {
		if err := s.apply(); err != nil {
			return u.rollback(ctx, swaps[:i], backup, oldSigVersion, err)
		}
	}
	if sigVersion != "" {
		os.WriteFile(u.SignalCLIVersion, []byte(sigVersion+"\n"), 0o644)
	}
	if err := u.Service.Start(ctx); err != nil {
		return u.rollback(ctx, swaps, backup, oldSigVersion, fmt.Errorf("start say: %w", err))
	}
	if err := u.waitHealthy(ctx, m.Version); err != nil {
		return u.rollback(ctx, swaps, backup, oldSigVersion, err)
	}
	return nil
}

// apply keeps path as prev and moves new over it. path is never missing.
func (s swap) apply() error {
	os.Remove(s.prev)
	if err := os.Link(s.path, s.prev); err != nil {
		return err
	}
	return os.Rename(s.new, s.path)
}

func (u *Updater) rollback(ctx context.Context, swaps []swap, backup, sigVersion string, cause error) error {
	errs := []error{u.Service.Stop(ctx)}
	for _, s := range swaps {
		errs = append(errs, os.Rename(s.prev, s.path))
	}
	if sigVersion != "" {
		errs = append(errs, os.WriteFile(u.SignalCLIVersion, []byte(sigVersion+"\n"), 0o644))
	} else {
		os.Remove(u.SignalCLIVersion)
	}
	errs = append(errs, restore(backup, u.Data), u.Service.Start(ctx))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%w, and rolling back failed too: %w", cause, err)
	}
	return &rolledBack{cause}
}

// signalCLI reports whether the updater manages signal-cli: the image's
// native build, which comes with a version file. A JVM build has none and
// is left alone.
func (u *Updater) signalCLI() bool {
	_, err := os.Stat(u.SignalCLI)
	return u.SignalCLI != "" && err == nil && u.signalCLIVersion() != ""
}

func (u *Updater) signalCLIVersion() string {
	b, _ := os.ReadFile(u.SignalCLIVersion)
	return strings.TrimSpace(string(b))
}

func (u *Updater) waitHealthy(ctx context.Context, version string) error {
	wait := u.HealthWait
	if wait == 0 {
		wait = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	last := errors.New("no answer")
	for {
		if v, err := u.health(ctx); err != nil {
			last = err
		} else if v == version {
			return nil
		} else {
			last = fmt.Errorf("say says it is %q", v)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("say %s isn't healthy after %s: %w", version, wait, last)
		case <-time.After(time.Second):
		}
	}
}

func (u *Updater) health(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.Health, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var h struct{ Status, Version string }
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&h) != nil || h.Status != "ok" {
		return "", fmt.Errorf("health: %s", res.Status)
	}
	return h.Version, nil
}

func (u *Updater) download(ctx context.Context, d release.Download, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	res, err := u.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", d.URL, res.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, 1<<30))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, d.SHA256) {
		return fmt.Errorf("%s has SHA-256 %s, not %s", d.URL, got, d.SHA256)
	}
	return nil
}

// untarOne extracts the regular file name from a .tar.gz.
func untarOne(tgz, name, dst string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("no %s in the archive", name)
		}
		if err != nil {
			return err
		}
		if filepath.Clean(h.Name) != name || h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		return errors.Join(err, out.Close())
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err = errors.Join(err, out.Sync(), out.Close()); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// copyTree copies the directory src to dst, which must not exist, with
// modes and owners. It skips anything but files, directories and
// symlinks, such as sockets.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		to := filepath.Join(dst, rel)
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			err = os.Mkdir(to, fi.Mode().Perm())
			if err == nil {
				err = os.Chmod(to, fi.Mode().Perm())
			}
		case fi.Mode().IsRegular():
			err = copyFile(path, to)
		case fi.Mode()&fs.ModeSymlink != 0:
			var target string
			if target, err = os.Readlink(path); err == nil {
				err = os.Symlink(target, to)
			}
		default:
			return nil
		}
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if err := os.Lchown(to, int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, fs.ErrPermission) {
				return err
			}
		}
		return nil
	})
}

// restore empties dir, which stays, and copies backup's contents into it.
func restore(backup, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(backup)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(backup, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Systemd is a systemd unit, run with systemctl.
type Systemd string

func (s Systemd) Stop(ctx context.Context) error  { return s.systemctl(ctx, "stop") }
func (s Systemd) Start(ctx context.Context) error { return s.systemctl(ctx, "start") }

func (s Systemd) systemctl(ctx context.Context, verb string) error {
	out, err := exec.CommandContext(ctx, "systemctl", verb, string(s)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", verb, s, err, strings.TrimSpace(string(out)))
	}
	return nil
}
