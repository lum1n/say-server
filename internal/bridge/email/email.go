// Package email bridges a person's own mailbox to say
// (docs/adr/0014-bridges.md): mail comes in over IMAP, what they write in
// say goes out over SMTP as them.
//
// Every set of correspondents is a chat: mail between the owner and one
// other address is a direct chat keyed by that address, mail with several
// people is a group keyed by their sorted addresses. Messages are keyed by
// their Message-ID, which say also sets on the mail it sends, so the copy
// in the sent folder is recognized as the echo of a send.
package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:  "email",
		Title: "Email",
		Modes: []string{store.ModeAccount},
		// A reply is threaded with In-Reply-To; files go as attachments.
		Capabilities: []connector.Capability{connector.CapReplies, connector.CapAttachments},
		Fields: []bridge.Field{
			{Key: "address", Label: "Email address", Kind: "text", Required: true, Placeholder: "you@example.com"},
			{Key: "name", Label: "Display name", Kind: "text", Help: "Shown as the sender of the mail you write in say."},
			{Key: "imap", Label: "IMAP server", Kind: "text", Required: true, Placeholder: "imap.example.com:993"},
			{Key: "smtp", Label: "SMTP server", Kind: "text", Required: true, Placeholder: "smtp.example.com:465"},
			{Key: "username", Label: "Username", Kind: "text", Help: "Defaults to the email address."},
			{Key: "password", Label: "Password", Kind: "secret", Required: true, Help: "Use an app password if your provider has them."},
			{Key: "security", Label: "Security", Kind: "text", Placeholder: "tls",
				Help: "tls (STARTTLS on ports 143, 587 and 25, else implicit TLS), starttls, or none for testing."},
			{Key: "folder", Label: "Folder", Kind: "text", Placeholder: "INBOX"},
			{Key: "sent_folder", Label: "Sent folder", Kind: "text", Placeholder: "Sent",
				Help: "Read for mail you send from other apps; say also files its own mail there."},
		},
		Open: open,
	})
}

const (
	// backfillAge and backfillMax bound what a first sync reads.
	backfillAge = 30 * 24 * time.Hour
	backfillMax = 200
	// batch is how many new messages one sync of a folder reads.
	batch = 200
	// maxBody is how much of a message is read with the others: enough
	// for its text and the names of its first attachments.
	maxBody = 512 << 10
	// maxMail is the biggest message read whole, on its own, for its
	// files: a few at say's limit, base64-encoded. Bigger ones only name
	// them.
	maxMail = 48 << 20
	// maxThreads bounds the threading state kept per chat.
	maxThreads = 300
)

// Timings, shortened by tests.
var (
	// pollEvery is how often a server without IDLE is polled, and how
	// often the sent folder is checked while idling.
	pollEvery = 30 * time.Second
	// idleFor is how long an IDLE lasts when there is no sent folder.
	idleFor     = 10 * time.Minute
	dialTimeout = 30 * time.Second
	// smtpTimeout bounds a send when its context has no deadline.
	smtpTimeout = 2 * time.Minute
	// appendTimeout bounds filing a copy in the sent folder.
	appendTimeout = time.Minute
)

// config is the bridge config.
type config struct {
	Address    string `json:"address"`
	Name       string `json:"name"`
	IMAP       string `json:"imap"`
	SMTP       string `json:"smtp"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Security   string `json:"security"`
	Folder     string `json:"folder"`
	SentFolder string `json:"sent_folder"`
}

// state is what survives restarts.
type state struct {
	// Folders are the sync positions by folder name.
	Folders map[string]position `json:"folders,omitempty"`
	// Threads are the last message of each chat, to answer it.
	Threads map[string]thread `json:"threads,omitempty"`
}

// position is how far a folder has been read. A new UIDVALIDITY means the
// server renumbered the folder, and it is read afresh.
type position struct {
	Validity uint32 `json:"validity"`
	UID      uint32 `json:"uid"`
}

// thread is the last message seen in a chat.
type thread struct {
	MessageID string    `json:"id"`
	Subject   string    `json:"subject,omitempty"`
	At        time.Time `json:"at"`
}

type conn struct {
	cfg  config
	h    bridge.Host
	self string // the owner's address, lowercased

	mu sync.Mutex
	st state
	// saveMu keeps saves in order.
	saveMu sync.Mutex
}

func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, err
	}
	cfg.Address = strings.TrimSpace(cfg.Address)
	if !strings.Contains(cfg.Address, "@") {
		return nil, errors.New("email: the address needs an @")
	}
	if cfg.Username == "" {
		cfg.Username = cfg.Address
	}
	if cfg.Folder == "" {
		cfg.Folder = "INBOX"
	}
	switch cfg.Security {
	case "":
		cfg.Security = "tls"
	case "tls", "starttls", "none":
	default:
		return nil, fmt.Errorf("email: security is tls, starttls or none, not %q", cfg.Security)
	}
	for _, hp := range []string{cfg.IMAP, cfg.SMTP} {
		if _, _, err := net.SplitHostPort(hp); err != nil {
			return nil, fmt.Errorf("email: %q is not host:port", hp)
		}
	}
	c := &conn{cfg: cfg, h: h, self: strings.ToLower(cfg.Address)}
	if len(b.State) > 0 {
		// A broken state only costs a fresh sync.
		_ = json.Unmarshal(b.State, &c.st)
	}
	return c, nil
}

// domain is the domain of the owner's address, for Message-IDs.
func (c *conn) domain() string {
	return c.cfg.Address[strings.LastIndex(c.cfg.Address, "@")+1:]
}

// startTLS reports whether a connection to hostport upgrades with
// STARTTLS rather than speaking TLS from the start.
func (c *conn) startTLS(hostport string) bool {
	if c.cfg.Security == "starttls" {
		return true
	}
	_, port, _ := net.SplitHostPort(hostport)
	return c.cfg.Security == "tls" && (port == "143" || port == "587" || port == "25")
}

// position returns how far a folder has been read.
func (c *conn) position(folder string) position {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Folders[folder]
}

// setPositions records how far folders have been read.
func (c *conn) setPositions(p map[string]position) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.Folders == nil {
		c.st.Folders = map[string]position{}
	}
	for f, pos := range p {
		c.st.Folders[f] = pos
	}
}

// thread returns the last message of a chat.
func (c *conn) thread(chat string) thread {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Threads[chat]
}

// noteThread records the latest message of a chat, keeping state for the
// most recent chats only.
func (c *conn) noteThread(chat string, t thread) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.st.Threads[chat]; ok && old.At.After(t.At) {
		return
	}
	if c.st.Threads == nil {
		c.st.Threads = map[string]thread{}
	}
	c.st.Threads[chat] = t
	for len(c.st.Threads) > maxThreads {
		oldest := ""
		for k, v := range c.st.Threads {
			if oldest == "" || v.At.Before(c.st.Threads[oldest].At) {
				oldest = k
			}
		}
		delete(c.st.Threads, oldest)
	}
}

// save keeps the state for the next Run.
func (c *conn) save(ctx context.Context) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	b, err := json.Marshal(c.st)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.h.SaveState(ctx, b)
}
