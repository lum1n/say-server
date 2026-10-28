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

	"github.com/lum1n/say-server/say/release"
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
	Manifest = release.LatestURL
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
	// A host may return a node it made before failing, such as one it
	// couldn't share: it goes back too, for the caller to Delete.
	return Deployment{Node: node, SetupCode: code}, err
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

// Claim creates the node's admin with code, then pairs the device it runs
// on as device and returns that device's token, which doesn't expire and
// can be revoked on its own (docs/adr/0018-devices.md).
func Claim(ctx context.Context, hc *http.Client, url, code, username, password, device string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	url = strings.TrimRight(url, "/")
	res, err := postJSON(ctx, hc, url+"/api/v1/setup", map[string]string{"code": code, "username": username, "password": password})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		switch e := apiError(res); e.Code {
		case "setup_done":
			return "", ErrSetupDone
		case "setup_closed":
			return "", ErrSetupClosed
		default:
			return "", fmt.Errorf("claiming the node: %s %s", res.Status, e.Message)
		}
	}
	// Setup also signed in a browser session, which a device doesn't need.
	for _, c := range res.Cookies() {
		if c.Name == "say_token" {
			if req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/logout", nil); err == nil {
				req.AddCookie(c)
				if res, err := hc.Do(req); err == nil {
					res.Body.Close()
				}
			}
		}
	}
	res, err = postJSON(ctx, hc, url+"/api/v1/devices", map[string]string{"username": username, "password": password, "name": device})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var d struct {
		Token string `json:"token"`
	}
	if res.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("pairing this device with the node: %s %s", res.Status, apiError(res).Message)
	}
	if err := json.NewDecoder(res.Body).Decode(&d); err != nil || d.Token == "" {
		return "", fmt.Errorf("pairing this device with the node: no token in the answer")
	}
	return d.Token, nil
}

func postJSON(ctx context.Context, hc *http.Client, url string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return hc.Do(req)
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func apiError(res *http.Response) errorBody {
	var e struct {
		Error errorBody `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&e)
	return e.Error
}
