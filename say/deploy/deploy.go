// Package deploy puts a person's own say node on a host and claims it
// (docs/adr/0017-own-node.md). Only making and removing the machine
// differ between hosts, behind Host; the image, the setup code, waiting
// for the node and claiming it are the same everywhere. Package
// say/exe is the exe.dev host.
package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Host makes and removes machines that run the say-node image.
type Host interface {
	// Create makes a machine named spec.Name from spec.Image with
	// spec.Env, serves it on a public HTTPS address, and returns it
	// without waiting for say to start.
	Create(ctx context.Context, spec Spec) (Node, error)
	// Delete removes the machine and everything on it.
	Delete(ctx context.Context, name string) error
}

// Spec is a machine to make.
type Spec struct {
	Name  string
	Image string
	Env   map[string]string
}

// Node is a machine running say.
type Node struct {
	Name string `json:"name"`
	URL  string `json:"url"` // such as https://say-k3x9.exe.xyz
}

const (
	// Image is the say-node image, tagged by release version.
	Image = "ghcr.io/lum1n/say-node"
	// StableImage is used when the newest release can't be looked up.
	StableImage = Image + ":stable"
	// Manifest names the newest release.
	Manifest = "https://github.com/lum1n/say-v2/releases/latest/download/manifest.json"
	// SetupMinutes is how long a new node waits to be claimed before it
	// refuses setup.
	SetupMinutes = 30
	// Wait is how long Run waits for a new node's setup to answer.
	Wait = 5 * time.Minute
)

// Step is how far Run has come, for a progress display.
type Step string

const (
	StepCreating Step = "creating" // making the machine
	StepStarting Step = "starting" // waiting for say to answer
	StepReady    Step = "ready"    // open for setup
)

// Options tune Start and Run. The zero value deploys the newest release.
type Options struct {
	Name     string        // "" picks say-<random>
	Image    string        // "" looks up the newest release
	Manifest string        // "" is Manifest
	Wait     time.Duration // 0 is Wait
	HTTP     *http.Client  // nil is http.DefaultClient
	Progress func(Step)
}

// Deployment is a new node and the code that claims it.
type Deployment struct {
	Node
	SetupCode string `json:"setup_code"`
}

// Run makes a node on h with a fresh setup code (Start) and waits until
// it is open for setup (WaitOpen). If it never opens, the node is still
// returned, for the caller to Delete (they may want to look at it first).
func Run(ctx context.Context, h Host, o Options) (Deployment, error) {
	d, err := Start(ctx, h, o)
	if err != nil {
		return d, err
	}
	o.progress(StepStarting)
	wait := o.Wait
	if wait == 0 {
		wait = Wait
	}
	if err := WaitOpen(ctx, o.http(), d.URL, wait); err != nil {
		return d, err
	}
	o.progress(StepReady)
	return d, nil
}

// Start makes a node on h with a fresh setup code, without waiting for
// say to answer.
func Start(ctx context.Context, h Host, o Options) (Deployment, error) {
	name := o.Name
	if name == "" {
		name = NewName()
	}
	image := o.Image
	if image == "" {
		manifest := o.Manifest
		if manifest == "" {
			manifest = Manifest
		}
		image = LatestImage(ctx, o.http(), manifest)
	}
	code := NewSetupCode()
	o.progress(StepCreating)
	node, err := h.Create(ctx, Spec{Name: name, Image: image, Env: map[string]string{
		"SAY_SETUP_CODE":    code,
		"SAY_SETUP_MINUTES": strconv.Itoa(SetupMinutes),
	}})
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{Node: node, SetupCode: code}, nil
}

func (o Options) http() *http.Client {
	if o.HTTP == nil {
		return http.DefaultClient
	}
	return o.HTTP
}

func (o Options) progress(s Step) {
	if o.Progress != nil {
		o.Progress(s)
	}
}

// crockford is Crockford's base 32: no I, L, O or U to misread.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func randomCrockford(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = crockford[b[i]&31]
	}
	return string(b)
}

// NewSetupCode is a code like 7KQ2-MX4D-9TRB-H0WN (80 bits).
func NewSetupCode() string {
	c := randomCrockford(16)
	return c[0:4] + "-" + c[4:8] + "-" + c[8:12] + "-" + c[12:16]
}

// NewName is a machine name like say-k3x9q2.
func NewName() string {
	return "say-" + strings.ToLower(randomCrockford(6))
}

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// LatestImage is the newest release's own image, such as
// ghcr.io/lum1n/say-node:0.4.1, or StableImage when the manifest can't be
// read. Hosts may keep an image they pulled by its tag, so :stable could
// start a new node on an old release.
func LatestImage(ctx context.Context, hc *http.Client, manifest string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest, nil)
	if err != nil {
		return StableImage
	}
	res, err := hc.Do(req)
	if err != nil {
		return StableImage
	}
	defer res.Body.Close()
	var m struct {
		Version string `json:"version"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&m) != nil {
		return StableImage
	}
	v := strings.TrimPrefix(m.Version, "v")
	if !releaseVersion.MatchString(v) {
		return StableImage
	}
	return Image + ":" + v
}

var (
	// ErrSetupDone means the node already has an admin.
	ErrSetupDone = errors.New("this node is set up already")
	// ErrSetupClosed means nobody claimed the node in time; delete it and
	// deploy another.
	ErrSetupClosed = errors.New("this node's setup window has closed")
)

// WaitOpen polls the node's GET /api/v1/setup every 2 s until it is open
// for setup, for at most wait.
func WaitOpen(ctx context.Context, hc *http.Client, url string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		err := setupOpen(ctx, hc, url)
		if err == nil || errors.Is(err, ErrSetupDone) || errors.Is(err, ErrSetupClosed) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the node didn't start: %w", err)
		case <-tick.C:
		}
	}
}

func setupOpen(ctx context.Context, hc *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/api/v1/setup", nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("setup status: %s", res.Status)
	}
	var s struct {
		Needed bool  `json:"needed"`
		Open   *bool `json:"open"` // absent before setup windows
	}
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return fmt.Errorf("setup status: %w", err)
	}
	switch {
	case !s.Needed:
		return ErrSetupDone
	case s.Open != nil && !*s.Open:
		return ErrSetupClosed
	}
	return nil
}

// Claim creates the node's admin with code and returns a session token.
func Claim(ctx context.Context, hc *http.Client, url, code, username, password string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	body, _ := json.Marshal(map[string]string{"code": code, "username": username, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/api/v1/setup", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(res.Body).Decode(&e)
		switch e.Error.Code {
		case "setup_done":
			return "", ErrSetupDone
		case "setup_closed":
			return "", ErrSetupClosed
		}
		return "", fmt.Errorf("claiming the node: %s %s", res.Status, e.Error.Message)
	}
	for _, c := range res.Cookies() {
		if c.Name == "say_token" {
			return c.Value, nil
		}
	}
	return "", errors.New("claiming the node: no session in the answer")
}
