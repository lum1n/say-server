// Package release reads say's signed release manifest
// (docs/adr/0017-own-node.md). Every release publishes manifest.json,
// naming its say-node image and the say and signal-cli downloads with
// their SHA-256s, and manifest.json.minisig, a minisign signature by the
// key pinned here. A node's self-update installs only what a manifest
// that verifies names.
package release

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LatestURL is the newest release's manifest.
const LatestURL = "https://github.com/lum1n/say-v2/releases/latest/download/manifest.json"

//go:embed minisign.pub
var pinned []byte

// Key is the release signing key. scripts/release.sh and release.yml sign
// with its secret half.
var Key = must(ParsePublicKey(pinned))

func must(k PublicKey, err error) PublicKey {
	if err != nil {
		panic("release: the pinned key: " + err.Error())
	}
	return k
}

// Manifest describes one release.
type Manifest struct {
	Version     string `json:"version"` // such as 0.4.1
	Image       string `json:"image"`
	ImageDigest string `json:"image_digest"`
	// Say is keyed by GOOS-GOARCH, such as linux-amd64.
	Say       map[string]Download `json:"say"`
	SignalCLI SignalCLI           `json:"signal_cli"`
}

// SignalCLI is the signal-cli release the say-node image pins. Its
// native build is for linux-amd64 only.
type SignalCLI struct {
	Version    string    `json:"version"`
	LinuxAMD64 *Download `json:"linux-amd64,omitempty"`
}

// Download is a file and its SHA-256, in hex.
type Download struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Fetch downloads the manifest at url and its signature at url+".minisig"
// and checks the signature with key. The version must be a release, such
// as 0.4.1.
func Fetch(ctx context.Context, hc *http.Client, url string, key PublicKey) (Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := get(ctx, hc, url)
	if err != nil {
		return Manifest{}, err
	}
	sig, err := get(ctx, hc, url+".minisig")
	if err != nil {
		return Manifest{}, err
	}
	if err := key.Verify(body, sig); err != nil {
		return Manifest{}, fmt.Errorf("release: the manifest's signature: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("release: the manifest: %w", err)
	}
	if !versionRE.MatchString(m.Version) {
		return Manifest{}, fmt.Errorf("release: the manifest's version %q is not a release", m.Version)
	}
	return m, nil
}

func get(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: GET %s: %s", url, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Newer reports whether the release version latest comes after current.
// A current version with a suffix, such as 0.4.1-dev, comes before its
// release, and one that isn't a version at all, such as dev, before every
// release.
func Newer(latest, current string) bool {
	l, ok := parse(latest)
	if !ok || l.pre != "" {
		return false
	}
	c, ok := parse(current)
	if !ok {
		return true
	}
	for i := range l.n {
		if l.n[i] != c.n[i] {
			return l.n[i] > c.n[i]
		}
	}
	return c.pre != ""
}

type version struct {
	n   [3]int
	pre string
}

func parse(s string) (version, bool) {
	s, pre, _ := strings.Cut(strings.TrimPrefix(s, "v"), "-")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	var v version
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, false
		}
		v.n[i] = n
	}
	v.pre = pre
	return v, true
}
