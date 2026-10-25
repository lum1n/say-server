package matrix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/vegard/say-v2/internal/bridge"
	"github.com/vegard/say-v2/internal/connector"
	"github.com/vegard/say-v2/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:  "matrix",
		Title: "Matrix",
		Modes: []string{store.ModeAccount},
		Fields: []bridge.Field{
			{Key: "homeserver", Label: "Homeserver", Kind: "text", Required: true, Placeholder: "https://matrix.example.org",
				Help: "The address of your homeserver's client API."},
			{Key: "user_id", Label: "User ID", Kind: "text", Required: true, Placeholder: "@ada:example.org"},
			{Key: "password", Label: "Password", Kind: "secret",
				Help: "Used to log in a session of say's own. Leave it and the token out to be asked for it."},
			{Key: "access_token", Label: "Access token", Kind: "secret",
				Help: "Instead of a password: the token of a session you made."},
			{Key: "device_name", Label: "Device name", Kind: "text", Placeholder: "say",
				Help: "How say's session shows in your list of devices."},
		},
		Capabilities: []connector.Capability{connector.CapEdits},
		Open:         open,
	})
}

// config is a bridge's settings.
type config struct {
	Homeserver  string `json:"homeserver"`
	UserID      string `json:"user_id"`
	Password    string `json:"password"`
	AccessToken string `json:"access_token"`
	DeviceName  string `json:"device_name"`
}

// state is what a connection keeps between runs. The session lives here
// rather than in the config, so a password is used once and a restart on
// another node goes on with the same device.
type state struct {
	// Account is the homeserver and user the rest is for; a change of
	// either starts afresh.
	Account     string `json:"account"`
	UserID      string `json:"user_id,omitempty"`
	AccessToken string `json:"access_token,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	// Login is a hash of the config the session was made from, so that a
	// new access token in the config replaces the session.
	Login string `json:"login,omitempty"`
	// FromConfig says the session is the config's access token, not one
	// say logged in for.
	FromConfig bool `json:"from_config,omitempty"`
	// NextBatch is where the next sync starts; empty means the first sync,
	// which backfills.
	NextBatch string `json:"next_batch,omitempty"`
}

// conn is a running Matrix account.
type conn struct {
	cfg  config
	h    bridge.Host
	http *http.Client

	mu    sync.Mutex
	st    state
	rooms map[string]*room
	// direct are the rooms the account's m.direct lists.
	direct map[string]bool
	// senders are the senders of recent messages by event id, to check
	// edits; order is their age, for dropping the oldest.
	senders map[string]string
	order   []string
}

func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, fmt.Errorf("matrix: bad config: %w", err)
	}
	cfg.Homeserver = strings.TrimRight(strings.TrimSpace(cfg.Homeserver), "/")
	if cfg.Homeserver == "" {
		return nil, errors.New("matrix: the homeserver is required")
	}
	if !strings.Contains(cfg.Homeserver, "://") {
		cfg.Homeserver = "https://" + cfg.Homeserver
	}
	cfg.UserID = strings.TrimSpace(cfg.UserID)
	if !strings.HasPrefix(cfg.UserID, "@") || !strings.Contains(cfg.UserID, ":") {
		return nil, errors.New("matrix: the user ID looks like @ada:example.org")
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = "say"
	}
	c := &conn{cfg: cfg, h: h, http: &http.Client{}}
	if len(b.State) > 0 {
		json.Unmarshal(b.State, &c.st)
	}
	if c.st.Account != c.account() {
		c.st = state{Account: c.account()}
	}
	return c, nil
}

// account names the homeserver and user of the config.
func (c *conn) account() string { return c.cfg.Homeserver + " " + c.cfg.UserID }

// loginHash is a hash of what a session depends on in the config.
func (c *conn) loginHash() string {
	sum := sha256.Sum256([]byte(c.account() + "\n" + c.cfg.AccessToken))
	return hex.EncodeToString(sum[:8])
}

// token returns the access token of the session.
func (c *conn) token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.AccessToken
}

// self returns the account's user id.
func (c *conn) self() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.UserID != "" {
		return c.st.UserID
	}
	return c.cfg.UserID
}

// save keeps the state for the next run.
func (c *conn) save(ctx context.Context) error {
	c.mu.Lock()
	data, err := json.Marshal(c.st)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.h.SaveState(ctx, data)
}

// Run logs in and syncs until ctx ends.
func (c *conn) Run(ctx context.Context) error {
	if err := c.logIn(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	c.rooms = map[string]*room{}
	c.mu.Unlock()
	relogged := false
	connected := false
	for {
		c.mu.Lock()
		since := c.st.NextBatch
		c.mu.Unlock()
		// The first sync of a run does not wait, so the bridge shows as
		// live at once.
		resp, err := c.sync(ctx, since, connected)
		if isCode(err, "M_UNKNOWN_TOKEN") && !relogged {
			// The session was logged out elsewhere: start a new one.
			relogged = true
			c.mu.Lock()
			fromConfig := c.st.FromConfig
			c.st.AccessToken, c.st.DeviceID, c.st.FromConfig = "", "", false
			c.mu.Unlock()
			if fromConfig && c.cfg.Password == "" {
				c.save(ctx)
				return errors.New("matrix: the homeserver no longer accepts the access token; set a new one or a password")
			}
			if err := c.logIn(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if !connected && since != "" {
			// A first sync brings m.direct; a resumed one only changes.
			if err := c.loadDirect(ctx); err != nil {
				return err
			}
		}
		if err := c.handle(ctx, resp, since == ""); err != nil {
			return err
		}
		c.mu.Lock()
		c.st.NextBatch = resp.NextBatch
		c.mu.Unlock()
		if err := c.save(ctx); err != nil {
			return err
		}
		if !connected {
			connected = true
			c.h.Connected(ctx)
		}
	}
}

// logIn makes sure there is a session: the saved one, the config's access
// token, or a new one logged in with the password.
func (c *conn) logIn(ctx context.Context) error {
	hash := c.loginHash()
	c.mu.Lock()
	saved := c.st.AccessToken != "" && c.st.Login == hash
	c.mu.Unlock()
	if saved {
		return nil
	}
	if c.cfg.AccessToken != "" {
		err := c.useToken(ctx, hash)
		if err == nil || !isCode(err, "M_UNKNOWN_TOKEN") || c.cfg.Password == "" {
			if isCode(err, "M_UNKNOWN_TOKEN") {
				return errors.New("matrix: the homeserver does not accept the access token")
			}
			return err
		}
		// A stale token with a password beside it: log in instead.
	}
	password := c.cfg.Password
	if password == "" {
		var err error
		password, err = c.h.Ask(ctx, bridge.Prompt{Kind: "password", Text: "Password for " + c.cfg.UserID})
		if err != nil {
			return err
		}
	}
	return c.logInPassword(ctx, hash, password)
}

// useToken takes the config's access token as the session.
func (c *conn) useToken(ctx context.Context, hash string) error {
	c.mu.Lock()
	c.st.AccessToken = c.cfg.AccessToken
	c.mu.Unlock()
	var who struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	if err := c.do(ctx, http.MethodGet, path("account", "whoami"), nil, nil, &who); err != nil {
		c.mu.Lock()
		c.st.AccessToken = ""
		c.mu.Unlock()
		return err
	}
	if who.UserID != c.cfg.UserID {
		c.mu.Lock()
		c.st.AccessToken = ""
		c.mu.Unlock()
		return fmt.Errorf("matrix: the access token is for %s, not %s", who.UserID, c.cfg.UserID)
	}
	c.mu.Lock()
	c.st.UserID, c.st.DeviceID, c.st.Login, c.st.FromConfig = who.UserID, who.DeviceID, hash, true
	c.mu.Unlock()
	return c.save(ctx)
}

// logInPassword logs in a new session of say's own.
func (c *conn) logInPassword(ctx context.Context, hash, password string) error {
	req := map[string]any{
		"type":                        "m.login.password",
		"identifier":                  map[string]string{"type": "m.id.user", "user": c.cfg.UserID},
		"password":                    password,
		"initial_device_display_name": c.cfg.DeviceName,
	}
	var resp struct {
		UserID      string `json:"user_id"`
		AccessToken string `json:"access_token"`
		DeviceID    string `json:"device_id"`
	}
	err := c.do(ctx, http.MethodPost, path("login"), nil, req, &resp)
	if isCode(err, "M_FORBIDDEN") {
		return errors.New("matrix: the homeserver did not accept the user ID and password")
	}
	if err != nil {
		return err
	}
	if resp.AccessToken == "" {
		return errors.New("matrix: the login gave no access token")
	}
	c.mu.Lock()
	c.st.UserID, c.st.AccessToken, c.st.DeviceID = resp.UserID, resp.AccessToken, resp.DeviceID
	c.st.Login, c.st.FromConfig = hash, false
	c.mu.Unlock()
	return c.save(ctx)
}
