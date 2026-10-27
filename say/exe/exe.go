// Package exe makes say nodes on exe.dev from the app, in the person's
// own exe.dev account (docs/adr/0017-own-node.md).
//
// Signing in needs no computer: the app makes its own SSH key (NewKey)
// and shows exe.dev's sign-in screen (StartLogin), where the person
// types their email and opens the link exe.dev sends, which adds the key
// to their account. Meanwhile the app mints an exe0 token from the same
// key (MintToken) and waits until exe.dev takes it (WaitSignedIn). After
// that, Client runs exe.dev's commands over its HTTPS API and is the
// exe.dev deploy.Host.
package exe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lum1n/say-server/say/deploy"
)

const (
	// BaseURL is exe.dev's HTTPS API.
	BaseURL = "https://exe.dev"
	// SSHAddr is exe.dev's SSH sign-in.
	SSHAddr = "exe.dev:22"
	// HostKey is exe.dev's SSH host key fingerprint, from
	// https://exe.dev/docs/faq/host-key.
	HostKey = "SHA256:JJOP/lwiBGOMilfONPWZCXUrfK154cnJFXcqlsi6lPo"

	// Port is where say listens inside the VM; exe.dev's proxy serves it.
	Port = 80
	// Memory and Disk size a node; it fits next to other VMs in a plan.
	Memory = "2GB"
	Disk   = "20GB"

	// TokenLifetime is how long a minted token lasts, and RemintBefore how
	// long before that the app mints a new one. Minting needs no one, as
	// the key stays on the device.
	TokenLifetime = 90 * 24 * time.Hour
	RemintBefore  = 14 * 24 * time.Hour
)

// Commands are all a minted token may run.
var Commands = []string{"whoami", "ls", "new", "rm", "restart", "share show", "share port", "share set-public"}

// APIKeyCommand is what someone who'd rather paste a token runs on a
// computer that can already `ssh exe.dev`.
var APIKeyCommand = "ssh exe.dev ssh-key generate-api-key --exp=90d --cmds=" + strings.Join(Commands, ",")

// Mint makes a token from privateKey for Commands, lasting TokenLifetime.
func Mint(privateKey string) (string, error) {
	return MintToken(privateKey, Commands, time.Now().Add(TokenLifetime))
}

// Error is a command exe.dev refused or failed.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	switch {
	case e.TokenRefused():
		return "exe.dev refused the token: " + e.Message
	case e.Status == http.StatusUnprocessableEntity:
		return "exe.dev: " + e.Message
	}
	return fmt.Sprintf("exe.dev answered %d: %s", e.Status, e.Message)
}

// TokenRefused reports whether exe.dev didn't take the token: wrong,
// expired, not allowed that command, or its key not on an account (yet).
func (e *Error) TokenRefused() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// TokenRefused reports whether err is exe.dev refusing the token.
func TokenRefused(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.TokenRefused()
}

// Client runs exe.dev commands with a person's token.
type Client struct {
	Base  string // BaseURL by default
	Token string
	HTTP  *http.Client
}

var _ deploy.Host = (*Client)(nil)

// New returns a client for exe.dev with token.
func New(token string) *Client {
	return &Client{Base: BaseURL, Token: strings.TrimSpace(token), HTTP: http.DefaultClient}
}

// Exec runs one command, such as "ls --json", and returns its output:
// JSON when the command gave JSON, else the text as a JSON string.
// exe.dev takes the raw command line, so callers check every value they
// put in it (see safe).
func (c *Client) Exec(ctx context.Context, command string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Base, "/")+"/exec", strings.NewReader(command))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "text/plain")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &Error{Status: res.StatusCode, Message: messageOf(body)}
	}
	if json.Valid(body) {
		return body, nil
	}
	return json.Marshal(string(body))
}

// Whoami is who the token belongs to, as exe.dev says it. It is also the
// check that the token works.
func (c *Client) Whoami(ctx context.Context) (string, error) {
	out, err := c.Exec(ctx, "whoami")
	if err != nil {
		return "", err
	}
	var v any
	json.Unmarshal(out, &v)
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v), nil
	case map[string]any:
		for _, k := range []string{"email", "user", "name"} {
			if s, ok := v[k].(string); ok {
				return s, nil
			}
		}
	}
	return string(out), nil
}

// List is the account's VMs.
func (c *Client) List(ctx context.Context) ([]deploy.Node, error) {
	out, err := c.Exec(ctx, "ls --json")
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	var wrapped struct {
		VMs []json.RawMessage `json:"vms"`
	}
	if json.Unmarshal(out, &wrapped) == nil && wrapped.VMs != nil {
		items = wrapped.VMs
	} else {
		json.Unmarshal(out, &items)
	}
	var nodes []deploy.Node
	for _, it := range items {
		if n, ok := nodeOf(it); ok {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// Create makes a VM from spec, points exe.dev's proxy at say's port and
// makes it public: say's own sign-in guards it, not an exe.dev login.
func (c *Client) Create(ctx context.Context, spec deploy.Spec) (deploy.Node, error) {
	if !vmName.MatchString(spec.Name) {
		return deploy.Node{}, fmt.Errorf("exe.dev: %q isn't a VM name this can pass on", spec.Name)
	}
	if !safe.MatchString(spec.Image) {
		return deploy.Node{}, fmt.Errorf("exe.dev: %q isn't an image this can pass on", spec.Image)
	}
	cmd := fmt.Sprintf("new --json --no-email --name %s --image %s --memory %s --disk %s", spec.Name, spec.Image, Memory, Disk)
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := spec.Env[k]
		if !envName.MatchString(k) || !safe.MatchString(v) {
			return deploy.Node{}, fmt.Errorf("exe.dev: %s isn't a setting this can pass on", k)
		}
		cmd += " --env " + k + "=" + v
	}
	out, err := c.Exec(ctx, cmd)
	if err != nil {
		return deploy.Node{}, err
	}
	n, ok := nodeOf(out)
	if !ok || !vmName.MatchString(n.Name) {
		n = deploy.Node{Name: spec.Name, URL: "https://" + spec.Name + ".exe.xyz"}
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("share port %s %d", n.Name, Port)); err != nil {
		return n, err
	}
	if _, err := c.Exec(ctx, "share set-public "+n.Name); err != nil {
		return n, err
	}
	return n, nil
}

// Delete removes the VM and everything on it.
func (c *Client) Delete(ctx context.Context, name string) error {
	if !vmName.MatchString(name) {
		return fmt.Errorf("exe.dev: %q isn't a VM name this can pass on", name)
	}
	_, err := c.Exec(ctx, "rm "+name)
	return err
}

// NameOf is the VM name in a node's address, such as say-k3x9q2 in
// https://say-k3x9q2.exe.xyz, or "" if it isn't an exe.xyz address.
func NameOf(url string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	name, ok := strings.CutSuffix(host, ".exe.xyz")
	if !ok || !vmName.MatchString(name) {
		return ""
	}
	return name
}

// WaitSignedIn tries the client's token every interval until exe.dev
// takes it, which happens once the person opened the link in exe.dev's
// mail, and returns who they are. Errors before then are expected and
// only end the wait with ctx.
func WaitSignedIn(ctx context.Context, c *Client, interval time.Duration) (string, error) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		who, err := c.Whoami(ctx)
		if err == nil {
			return who, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
}

var (
	// safe is what a value may hold, since /exec takes a raw command
	// line and nothing is quoted.
	safe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	vmName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// nodeOf reads a VM from what exe.dev said about it. The shapes of new
// and ls aren't documented, so it tries the likely keys and falls back to
// the VM's exe.xyz address.
func nodeOf(raw json.RawMessage) (deploy.Node, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return deploy.Node{}, false
	}
	if vm, ok := m["vm"].(map[string]any); ok {
		m = vm
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	name := str("vm_name", "name", "vmname", "vm")
	if name == "" {
		return deploy.Node{}, false
	}
	url := strings.TrimRight(str("https_url", "url", "proxy_url", "https"), "/")
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		url = "https://" + name + ".exe.xyz"
	}
	return deploy.Node{Name: name, URL: url}, true
}

func messageOf(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"error", "message", "output"} {
			if s, ok := m[k].(string); ok {
				return s
			}
		}
	}
	return strings.TrimSpace(string(body))
}
