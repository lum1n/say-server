// Package signal bridges a person's Signal account to say
// (docs/adr/0014-bridges.md).
//
// say becomes a linked device of the account, like Signal Desktop, through
// signal-cli. signal-cli is GPL, so it runs as a subprocess speaking
// JSON-RPC on stdin and stdout, never inside say (ADR 0004). It never
// registers a primary device: the person links say from their phone, for
// personal use only.
//
// signal-cli keeps the account's keys in its data directory on the node
// that runs it. say's own directory is copied into the bridge's state
// whenever signal-cli stops, so another node can take the bridge over as
// the same device (data.go); a directory set in the config stays on its
// node.
//
// Signal has no history API, so nothing older than the link is
// backfilled.
//
// A linked device can't remove itself from the account: deleting the
// bridge unregisters it, so Signal stops queueing messages for it, and
// deletes its keys, but it stays under Linked devices on the phone until
// the person removes it there.
package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:  "signal",
		Title: "Signal",
		Modes: []string{store.ModeAccount},
		Fields: []bridge.Field{
			{Key: "signal_cli", Label: "signal-cli program", Kind: "text", Placeholder: "signal-cli",
				Help: "Path to signal-cli on the server."},
			{Key: "data_dir", Label: "Data directory", Kind: "text",
				Help: "Where signal-cli keeps this account's keys. Empty is a directory of say's own, whose keys move with the bridge between servers; a directory set here stays on one server."},
			{Key: "device_name", Label: "Device name", Kind: "text", Placeholder: "say",
				Help: "How say shows in Signal's linked devices."},
		},
		Capabilities: []connector.Capability{connector.CapEdits},
		Open:         open,
		LogOut:       logOut,
	})
}

type config struct {
	Path       string `json:"signal_cli"`
	DataDir    string `json:"data_dir"`
	DeviceName string `json:"device_name"`
}

// state is what a Run leaves for the next.
type state struct {
	Account string `json:"account"`        // the phone number
	UUID    string `json:"uuid,omitempty"` // the account's ACI
	// Data is a copy of signal-cli's data, as a gzipped tar, and DataID
	// names it (data.go).
	Data   []byte `json:"data,omitempty"`
	DataID string `json:"data_id,omitempty"`
}

const (
	callTimeout = 30 * time.Second
	// stopTimeout is how long signal-cli gets to shut down cleanly before
	// it is killed.
	stopTimeout = 10 * time.Second
	// refreshEvery limits asking signal-cli for contacts and groups again
	// when a message names one it does not know.
	refreshEvery = time.Minute
)

func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if len(b.Config) > 0 {
		if err := json.Unmarshal(b.Config, &cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Path == "" {
		cfg.Path = "signal-cli"
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = "say"
	}
	c := &conn{id: b.ID, cfg: cfg, h: h}
	if len(b.State) > 0 {
		if err := json.Unmarshal(b.State, &c.st); err != nil {
			return nil, err
		}
	}
	return c, nil
}

type conn struct {
	id  string
	cfg config
	h   bridge.Host

	mu  sync.Mutex
	rpc *rpc // while connected
	st  state

	// Names, used only by the goroutine of Run.
	contacts  map[string]string // uuid or number: name
	uuids     map[string]string // number: uuid
	groups    map[string]string // group id: name
	refreshed time.Time
}

// dataDir returns signal-cli's directory for the bridge.
func (c *conn) dataDir() (string, error) {
	if c.cfg.DataDir != "" {
		return c.cfg.DataDir, nil
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		var err error
		if base, err = os.UserConfigDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "say", "signal", c.id), nil
}

// logOut unregisters say's device and deletes its keys and data. The
// device itself stays listed on the phone: only the primary device can
// remove it.
func logOut(ctx context.Context, b bridge.Info) error {
	bc, err := open(b, nil)
	if err != nil {
		return nil // never ran, so never linked
	}
	c := bc.(*conn)
	dir, err := c.dataDir()
	if err != nil {
		return err
	}
	// The bridge may have last run on another node.
	if err := c.restore(dir); err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if c.st.Account != "" {
		// Unregistering only stops messages being queued for the device;
		// its keys are deleted either way, so a failure does not stop it.
		if err := c.cli(ctx, dir, "unregister"); err != nil {
			slog.Warn("signal: unregister failed", "bridge", b.ID, "err", err)
		}
	}
	if c.cfg.DataDir == "" {
		// say's own directory, for this bridge alone.
		return os.RemoveAll(dir)
	}
	if c.st.Account == "" {
		return nil
	}
	return c.cli(ctx, dir, "deleteLocalAccountData", "--ignore-registered")
}

// cli runs one signal-cli command for the account.
func (c *conn) cli(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, c.cfg.Path, append([]string{"--config", dir, "-a", c.st.Account}, args...)...)
	cmd.WaitDelay = stopTimeout
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		if msg != "" {
			return fmt.Errorf("signal-cli %s: %w: %s", args[0], err, msg)
		}
		return fmt.Errorf("signal-cli %s: %w", args[0], err)
	}
	return nil
}

// errLinked ends a session that has just linked the account, so that its
// keys are kept before it goes on.
var errLinked = errors.New("linked")

// Run starts signal-cli, links the account if it is not yet, and passes
// on what arrives until ctx ends or signal-cli exits. Whenever signal-cli
// stops, its data is kept for the next node.
func (c *conn) Run(ctx context.Context) error {
	dir, err := c.dataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("signal data directory: %w", err)
	}
	if err := c.restore(dir); err != nil {
		return err
	}
	for {
		err := c.runCLI(ctx, dir)
		c.keepOrWarn(ctx, dir)
		if !errors.Is(err, errLinked) || ctx.Err() != nil {
			return err
		}
	}
}

// runCLI runs signal-cli once, until it exits.
func (c *conn) runCLI(ctx context.Context, dir string) error {
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(pctx, c.cfg.Path, "--config", dir, "-o", "json", "jsonRpc")
	// An interrupt lets signal-cli close its database; a kill follows if
	// it takes too long.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = stopTimeout
	stderr := &tail{max: 2048}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// A pipe of our own rather than StdoutPipe, whose reads race Wait.
	out, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stdout = w
	err = cmd.Start()
	w.Close()
	if err != nil {
		return fmt.Errorf("start signal-cli: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	r := newRPC(out, stdin)
	err = c.session(ctx, r)
	c.mu.Lock()
	c.rpc = nil
	c.mu.Unlock()
	stop()
	werr := <-exited
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errClosed) {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		if msg != "" {
			return fmt.Errorf("signal-cli exited (%v): %s", werr, msg)
		}
		return fmt.Errorf("signal-cli exited (%v)", werr)
	}
	return err
}

// session logs in on a running signal-cli and receives.
func (c *conn) session(ctx context.Context, r *rpc) error {
	account, linked, err := c.account(ctx, r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.st.Account != account {
		c.st = state{Account: account, Data: c.st.Data, DataID: c.st.DataID}
	}
	c.mu.Unlock()
	c.contacts, c.groups, c.uuids = map[string]string{}, map[string]string{}, map[string]string{}
	c.refresh(ctx, r)
	if err := c.save(ctx); err != nil {
		return err
	}
	if linked {
		return errLinked
	}

	c.mu.Lock()
	c.rpc = r
	c.mu.Unlock()
	c.h.Connected(ctx)
	for {
		ns, err := r.notifications(ctx)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if n.Method != "receive" {
				continue
			}
			var p receiveParams
			if err := json.Unmarshal(n.Params, &p); err != nil {
				continue
			}
			if p.Result != nil {
				p.Account, p.Envelope = p.Result.Account, p.Result.Envelope
			}
			if p.Envelope == nil || (p.Account != "" && p.Account != account) {
				continue
			}
			in, ok := c.incoming(ctx, p.Envelope)
			if !ok {
				continue
			}
			if err := c.h.Receive(ctx, in); err != nil {
				return err
			}
		}
	}
}

// account returns the linked account's number, linking say as a new
// device first if signal-cli has none; linked says it did.
func (c *conn) account(ctx context.Context, r *rpc) (number string, linked bool, err error) {
	accounts, err := listAccounts(ctx, r)
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	saved := c.st.Account
	c.mu.Unlock()
	for _, a := range accounts {
		if a == saved {
			return a, false, nil
		}
	}
	if len(accounts) > 0 {
		return accounts[0], false, nil
	}

	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	var link struct {
		URI string `json:"deviceLinkUri"`
	}
	err = r.call(cctx, "startLink", map[string]any{}, &link)
	cancel()
	if err != nil {
		return "", false, fmt.Errorf("start linking: %w", err)
	}
	// The QR code shows while signal-cli waits for the phone; it clears
	// when the wait is over, linked or not.
	askCtx, stopAsk := context.WithCancel(ctx)
	defer stopAsk()
	go c.h.Ask(askCtx, bridge.Prompt{Kind: "qr", Text: "Scan with Signal on your phone: Settings → Linked devices", Data: link.URI})
	var done struct {
		Number string `json:"number"`
	}
	err = r.call(ctx, "finishLink", map[string]any{"deviceLinkUri": link.URI, "deviceName": c.cfg.DeviceName}, &done)
	stopAsk()
	var re *rpcError
	if errors.As(err, &re) && strings.Contains(re.Message, "MissingCapabilities") {
		// Signal's servers turn away devices that lack features they now
		// require; only a newer signal-cli has them.
		return "", false, fmt.Errorf("link: signal-cli is too old for Signal's servers; update it and link again: %w", err)
	}
	if err != nil {
		return "", false, fmt.Errorf("link: %w", err)
	}
	if done.Number != "" {
		return done.Number, true, nil
	}
	if accounts, err = listAccounts(ctx, r); err != nil {
		return "", false, err
	}
	if len(accounts) == 0 {
		return "", false, errors.New("signal-cli linked no account")
	}
	return accounts[0], true, nil
}

func listAccounts(ctx context.Context, r *rpc) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var list []struct {
		Number string `json:"number"`
	}
	if err := r.call(ctx, "listAccounts", map[string]any{}, &list); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	var out []string
	for _, a := range list {
		if a.Number != "" {
			out = append(out, a.Number)
		}
	}
	return out, nil
}

// save keeps the account for the next Run.
func (c *conn) save(ctx context.Context) error {
	c.mu.Lock()
	raw, err := json.Marshal(c.st)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.h.SaveState(ctx, raw)
}

// refresh reads the names of contacts and groups from signal-cli, which
// keeps them from the phone's sync. Names are a nicety: errors are
// ignored, and the envelope's names stand in.
func (c *conn) refresh(ctx context.Context, r *rpc) {
	c.refreshed = time.Now()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	account := c.selfKey()
	var contacts []struct {
		Number    string `json:"number"`
		UUID      string `json:"uuid"`
		Name      string `json:"name"`
		NickName  string `json:"nickName"`
		GivenName string `json:"givenName"`
		Family    string `json:"familyName"`
		Profile   *struct {
			GivenName string `json:"givenName"`
			Family    string `json:"familyName"`
		} `json:"profile"`
	}
	if r.call(ctx, "listContacts", map[string]any{"account": account, "allRecipients": true}, &contacts) == nil {
		for _, ct := range contacts {
			name := first(ct.NickName, ct.Name, strings.TrimSpace(ct.GivenName+" "+ct.Family))
			if name == "" && ct.Profile != nil {
				name = strings.TrimSpace(ct.Profile.GivenName + " " + ct.Profile.Family)
			}
			if ct.Number != "" && ct.UUID != "" {
				c.uuids[ct.Number] = ct.UUID
			}
			if ct.Number == account && ct.UUID != "" {
				// The account itself, whose ACI names it in quotes.
				c.mu.Lock()
				c.st.UUID = ct.UUID
				c.mu.Unlock()
			}
			if name == "" {
				continue
			}
			for _, id := range []string{ct.UUID, ct.Number} {
				if id != "" {
					c.contacts[id] = name
				}
			}
		}
	}
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if r.call(ctx, "listGroups", map[string]any{"account": account}, &groups) == nil {
		for _, g := range groups {
			if g.ID != "" && g.Name != "" {
				c.groups[g.ID] = g.Name
			}
		}
	}
}

// contactName returns a person's name, asking signal-cli again for one it
// has not seen.
func (c *conn) contactName(ctx context.Context, id, fallback string) string {
	if n, ok := c.contacts[id]; ok {
		return n
	}
	if c.maybeRefresh(ctx) {
		if n, ok := c.contacts[id]; ok {
			return n
		}
	}
	return first(fallback, id)
}

// groupName returns a group's name, from the message or signal-cli.
func (c *conn) groupName(ctx context.Context, id, fromMessage string) string {
	if fromMessage != "" {
		return fromMessage
	}
	if n, ok := c.groups[id]; ok {
		return n
	}
	if c.maybeRefresh(ctx) {
		if n, ok := c.groups[id]; ok {
			return n
		}
	}
	return "Signal group"
}

func (c *conn) maybeRefresh(ctx context.Context) bool {
	c.mu.Lock()
	r := c.rpc
	c.mu.Unlock()
	if r == nil || time.Since(c.refreshed) < refreshEvery {
		return false
	}
	c.refresh(ctx, r)
	return true
}

// selfKey is how the account names itself in message ids: its number.
func (c *conn) selfKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Account
}

// isSelf reports whether an address is the account's own.
func (c *conn) isSelf(uuid, number string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return (uuid != "" && uuid == c.st.UUID) || (number != "" && number == c.st.Account)
}

// authorKey is the id part naming a message's author: the account's
// number for its own, else the uuid or number.
func (c *conn) authorKey(ctx context.Context, uuid, number string) string {
	if c.isSelf(uuid, number) {
		return c.selfKey()
	}
	return c.person(ctx, uuid, number)
}

// learnSelf keeps the account's ACI, which a sync message comes from.
func (c *conn) learnSelf(ctx context.Context, uuid string) {
	c.mu.Lock()
	known := uuid == "" || c.st.UUID == uuid
	if !known {
		c.st.UUID = uuid
	}
	c.mu.Unlock()
	if !known {
		c.save(ctx)
	}
}

// Send posts a message as the account and returns "<number>:<timestamp>",
// the id its sync to the person's other devices carries too.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	params, err := c.params(m)
	if err != nil {
		return "", err
	}
	if author, ts, ok := splitID(m.ReplyTo); ok {
		params["quoteTimestamp"] = ts
		params["quoteAuthor"] = author
	}
	return c.send(ctx, params)
}

// Edit changes a message sent before; Signal edits name the original's
// timestamp.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	_, ts, ok := splitID(remoteID)
	if !ok {
		return bridge.Rejected(fmt.Errorf("not a Signal message id: %q", remoteID))
	}
	params, err := c.params(m)
	if err != nil {
		return err
	}
	params["editTimestamp"] = ts
	_, err = c.send(ctx, params)
	return err
}

// params addresses a send to a remote chat.
func (c *conn) params(m bridge.Outgoing) (map[string]any, error) {
	p := map[string]any{"account": c.selfKey(), "message": m.Text}
	kind, id, _ := strings.Cut(m.Chat, ":")
	switch {
	case id == "":
		return nil, bridge.Rejected(fmt.Errorf("not a Signal chat: %q", m.Chat))
	case kind == "user":
		p["recipient"] = []string{id}
	case kind == "group":
		p["groupId"] = id
	default:
		return nil, bridge.Rejected(fmt.Errorf("not a Signal chat: %q", m.Chat))
	}
	return p, nil
}

func (c *conn) send(ctx context.Context, params map[string]any) (string, error) {
	c.mu.Lock()
	r := c.rpc
	c.mu.Unlock()
	if r == nil {
		return "", errors.New("signal is not connected")
	}
	var res struct {
		Timestamp int64 `json:"timestamp"`
	}
	if err := r.call(ctx, "send", params, &res); err != nil {
		if res.Timestamp, err = sendError(err); err != nil {
			return "", err
		}
	}
	if res.Timestamp == 0 {
		return "", errors.New("signal-cli sent no timestamp")
	}
	return params["account"].(string) + ":" + strconv.FormatInt(res.Timestamp, 10), nil
}

// sendError sorts out a failed send. signal-cli's codes: -1 a user error,
// -3 an I/O error, -4 an untrusted identity, -5 a rate limit, -32602 bad
// params such as an unknown group. A send that reached the server lists a
// result per recipient in the error's data. If any recipient got the
// message it was sent, and sendError returns its timestamp: a group
// message some members could not get still arrived for the rest, and a
// refusal would show it as not sent. Otherwise the error is a refusal
// unless a recipient had a network failure, after which it may have
// arrived.
func sendError(err error) (int64, error) {
	var re *rpcError
	if !errors.As(err, &re) {
		return 0, err
	}
	var data struct {
		Response *struct {
			Timestamp int64 `json:"timestamp"`
			Results   []struct {
				Type string `json:"type"`
			} `json:"results"`
		} `json:"response"`
	}
	json.Unmarshal(re.Data, &data)
	if resp := data.Response; resp != nil && len(resp.Results) > 0 {
		unsure := false
		for _, r := range resp.Results {
			switch r.Type {
			case "SUCCESS":
				if resp.Timestamp != 0 {
					return resp.Timestamp, nil
				}
				unsure = true
			case "NETWORK_FAILURE":
				unsure = true
			}
		}
		if unsure {
			return 0, err
		}
		return 0, bridge.Rejected(err)
	}
	switch re.Code {
	case -1, -4, -5, -32602:
		// Refused before sending.
		return 0, bridge.Rejected(err)
	}
	return 0, err
}

// splitID splits a message id into its author and timestamp.
func splitID(id string) (string, int64, bool) {
	i := strings.LastIndexByte(id, ':')
	if i <= 0 {
		return "", 0, false
	}
	ts, err := strconv.ParseInt(id[i+1:], 10, 64)
	return id[:i], ts, err == nil && ts > 0
}

// tail keeps the end of what a process writes, for its exit error.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
