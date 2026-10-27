package say

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/lum1n/say-server/say/deploy"
	"github.com/lum1n/say-server/say/exe"
)

// Deploying the person's own node to exe.dev, from the app
// (docs/adr/0017-own-node.md). In Swift:
//
//	let key = try SayNewExeKey()            // keep in the Keychain
//	let token = try SayMintExeToken(key.privateKey)
//	let login = try SayStartExeLogin(key.privateKey, token, 60, 20, screen)
//	// screen.onSignedIn → login.close()
//	let exe = SayNewExe(token)
//	let node = try exe.create()             // off the main thread
//	try exe.waitOpen(node.url)
//	let token = try SayClaimNode(node.url, node.setupCode, "me", password, "Ada's iPhone")

// ExeKey is the app's SSH key for exe.dev, in OpenSSH's formats. Keep it
// in the Keychain or Keystore; it never leaves the device.
type ExeKey struct {
	PrivateKey string
	PublicKey  string
}

// NewExeKey makes an Ed25519 key.
func NewExeKey() (*ExeKey, error) {
	k, err := exe.NewKey()
	if err != nil {
		return nil, err
	}
	return &ExeKey{PrivateKey: k.PrivateKey, PublicKey: k.PublicKey}, nil
}

// MintExeToken makes the app's exe.dev token from its key, lasting 90
// days. It works once the key is on the person's exe.dev account.
func MintExeToken(privateKey string) (string, error) {
	return exe.Mint(privateKey)
}

// ExeTokenNeedsRemint reports whether the token runs out within 14 days
// (or can't be read); mint a new one from the same key then.
func ExeTokenNeedsRemint(token string) bool {
	return exe.NeedsRemint(token, time.Now())
}

// ExeAPIKeyCommand is what someone who'd rather paste a token runs on a
// computer that can already `ssh exe.dev`.
func ExeAPIKeyCommand() string { return exe.APIKeyCommand }

// ExeNameOf is the VM name in a node's exe.xyz address, or "".
func ExeNameOf(url string) string { return exe.NameOf(url) }

// ExeScreen gets a sign-in session's news, on a background thread.
type ExeScreen interface {
	// OnScreen is exe.dev's screen when it changed: its rows joined by
	// "\n", and the cursor. Draw it natively, with a text field for input.
	OnScreen(text string, cursorRow, cursorCol int, cursorHidden bool)
	// OnSignedIn says exe.dev took the token: the person opened the link
	// in exe.dev's mail. who is their exe.dev account.
	OnSignedIn(who string)
	// OnClose says exe.dev ended the session; reason is empty unless it
	// failed. Signing in may still finish after it (OnSignedIn).
	OnClose(reason string)
}

// ExeLogin is exe.dev's sign-in screen, and a watch for the token
// working, which goes on after the screen ends.
type ExeLogin struct {
	l      *exe.Login
	cancel context.CancelFunc
}

// StartExeLogin opens exe.dev's sign-in with privateKey in a terminal of
// cols × rows, and tries token every 3 s until exe.dev takes it.
func StartExeLogin(privateKey, token string, cols, rows int, s ExeScreen) (*ExeLogin, error) {
	return startExeLogin(exe.LoginConfig{PrivateKey: privateKey, Cols: cols, Rows: rows}, exe.New(token), 3*time.Second, s)
}

func startExeLogin(c exe.LoginConfig, client *exe.Client, every time.Duration, s ExeScreen) (*ExeLogin, error) {
	c.OnScreen = func(sc exe.Screen) { s.OnScreen(sc.Text(), sc.CursorRow, sc.CursorCol, sc.CursorHidden) }
	ctx, cancel := context.WithCancel(context.Background())
	l, err := exe.StartLogin(ctx, c)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		<-l.Done()
		reason := ""
		if err := l.Err(); err != nil {
			reason = err.Error()
		}
		s.OnClose(reason)
	}()
	go func() {
		if who, err := exe.WaitSignedIn(ctx, client, every); err == nil {
			s.OnSignedIn(who)
		}
	}()
	return &ExeLogin{l: l, cancel: cancel}, nil
}

// Type sends text as typed; end a line with "\r".
func (l *ExeLogin) Type(text string) error { return l.l.Type(text) }

// Resize changes the terminal's size.
func (l *ExeLogin) Resize(cols, rows int) error { return l.l.Resize(cols, rows) }

// Close ends the session and the watch.
func (l *ExeLogin) Close() {
	l.cancel()
	l.l.Close()
}

// Exe is the person's exe.dev account, through the app's token. Its
// calls block; make them off the main thread. Cancel stops them.
type Exe struct {
	c *exe.Client

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// NewExe uses token on exe.dev.
func NewExe(token string) *Exe { return newExe(exe.New(token)) }

func newExe(c *exe.Client) *Exe {
	e := &Exe{c: c}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	return e
}

func (e *Exe) context() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ctx
}

// Cancel stops the calls in flight, which then fail. Later calls work.
func (e *Exe) Cancel() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancel()
	e.ctx, e.cancel = context.WithCancel(context.Background())
}

// Whoami is the account's email; it fails if exe.dev refuses the token.
func (e *Exe) Whoami() (string, error) { return e.c.Whoami(e.context()) }

// Nodes is the account's VMs as JSON: [{"name":…,"url":…}].
func (e *Exe) Nodes() (string, error) {
	nodes, err := e.c.List(e.context())
	if err != nil {
		return "", err
	}
	if nodes == nil {
		nodes = []deploy.Node{}
	}
	b, err := json.Marshal(nodes)
	return string(b), err
}

// ExeDeployment is a new node and the code that claims it.
type ExeDeployment struct {
	Name      string
	URL       string
	SetupCode string
}

// Create makes a node running the newest release, without waiting for
// it: then WaitOpen, and claim it with ClaimNode. Keep the name, to
// offer Delete if the node never opens.
func (e *Exe) Create() (*ExeDeployment, error) {
	return e.create(deploy.Options{})
}

func (e *Exe) create(o deploy.Options) (*ExeDeployment, error) {
	d, err := deploy.Start(e.context(), e.c, o)
	if err != nil {
		return nil, err
	}
	return &ExeDeployment{Name: d.Name, URL: d.URL, SetupCode: d.SetupCode}, nil
}

// WaitOpen waits, up to five minutes, until the node at url is open for
// setup. It fails at once if the node is set up already or nobody
// claimed it in time.
func (e *Exe) WaitOpen(url string) error {
	return deploy.WaitOpen(e.context(), http.DefaultClient, url, deploy.Wait)
}

// Delete removes the VM and everything on it.
func (e *Exe) Delete(name string) error { return e.c.Delete(e.context(), name) }

// ClaimNode creates the node's admin with the deploy's setup code, pairs
// this device under deviceName and returns its token. The token doesn't
// expire; the person can revoke it from another device.
func ClaimNode(url, setupCode, username, password, deviceName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return deploy.Claim(ctx, http.DefaultClient, url, setupCode, username, password, deviceName)
}
