package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:         "telegram",
		Title:        "Telegram",
		Modes:        []string{store.ModeAccount},
		Fields:       fields(hasDefaultApp()),
		Capabilities: []connector.Capability{connector.CapEdits},
		Open:         open,
		LogOut:       logOut,
	})
}

// appID and appHash are the Telegram app say connects as when a bridge
// names none of its own. Release builds set them with
// -ldflags "-X github.com/lum1n/say-server/internal/bridge/telegram.appID=…"
// (and appHash), so they are not in the source.
var appID, appHash string

// defaultApp returns the app a bridge without its own uses: the one in
// SAY_TELEGRAM_API_ID and SAY_TELEGRAM_API_HASH, else the one built in.
// The id is 0 when there is none.
func defaultApp() (int, string) {
	id, hash := appID, appHash
	if v := os.Getenv("SAY_TELEGRAM_API_ID"); v != "" {
		id, hash = v, os.Getenv("SAY_TELEGRAM_API_HASH")
	}
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 || hash == "" {
		return 0, ""
	}
	return n, hash
}

// hasDefaultApp reports whether bridges need no app of their own.
func hasDefaultApp() bool {
	id, _ := defaultApp()
	return id != 0
}

// fields are the settings. With a default app, only the phone number is
// asked for, and an app of one's own is advanced.
func fields(builtIn bool) []bridge.Field {
	idHelp := "Log in at my.telegram.org with your phone number, open API development tools and create an application. " +
		"Copy its App api_id here. The id is yours: say connects to Telegram as its own client under it."
	if builtIn {
		idHelp = "Leave empty to connect as say. To connect as an app of your own instead, " +
			"create one under API development tools at my.telegram.org and copy its App api_id here."
	}
	return []bridge.Field{
		{Key: "api_id", Label: "API ID", Kind: "number", Required: !builtIn, Advanced: builtIn, Help: idHelp},
		{
			Key: "api_hash", Label: "API hash", Kind: "secret", Required: !builtIn, Advanced: builtIn,
			Help: "The App api_hash from the same page at my.telegram.org.",
		},
		{
			Key: "phone", Label: "Phone number", Kind: "text", Required: true, Placeholder: "+4712345678",
			Help: "Your Telegram account's phone number in international format. Telegram sends a login code to your other " +
				"Telegram apps, and say asks you for it (and for your password, if two-step verification is on).",
		},
	}
}

// config is a bridge's settings.
type config struct {
	APIID   int    `json:"api_id"`
	APIHash string `json:"api_hash"`
	Phone   string `json:"phone"`
}

const (
	// maxFloodWait is the longest Telegram may ask a call to wait before
	// it is given up. Calling again before then gets accounts limited.
	maxFloodWait = 5 * time.Minute
	// dialogsEvery bounds how often an unknown chat makes the dialogs be
	// fetched again.
	dialogsEvery = time.Minute
)

// open makes a connection; it only checks the config.
func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	cfg, err := parseConfig(b.Config)
	if err != nil {
		return nil, err
	}
	return &conn{
		h:             h,
		cfg:           cfg,
		st:            newSaved(b.State, cfg.Phone, h.SaveState),
		saveEvery:     10 * time.Second,
		backfillAge:   30 * 24 * time.Hour,
		backfillChats: 50,
		backfillCount: 20,
		pause:         500 * time.Millisecond,
	}, nil
}

// parseConfig reads and checks a bridge's settings.
func parseConfig(raw json.RawMessage) (config, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, fmt.Errorf("telegram: bad config: %w", err)
	}
	if cfg.APIID == 0 && cfg.APIHash == "" {
		cfg.APIID, cfg.APIHash = defaultApp()
	}
	if cfg.APIID <= 0 || cfg.APIHash == "" {
		return config{}, errors.New("telegram: the API ID and hash from my.telegram.org are required")
	}
	phone := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(cfg.Phone)
	if len(phone) < 8 || phone[0] != '+' || strings.Trim(phone[1:], "0123456789") != "" {
		return config{}, errors.New("telegram: the phone number is in international format, like +4712345678")
	}
	cfg.Phone = phone
	return cfg, nil
}

// device is how say's session shows among the account's devices.
var device = telegram.DeviceConfig{DeviceModel: "say", SystemVersion: runtime.GOOS, AppVersion: "say bridge"}

// logOut ends say's session, which removes it from the account's active
// sessions.
func logOut(ctx context.Context, b bridge.Info) error {
	cfg, err := parseConfig(b.Config)
	if err != nil {
		return nil // never ran, so never logged in
	}
	st := newSaved(b.State, cfg.Phone, func(context.Context, json.RawMessage) error { return nil })
	if len(st.s.Session) == 0 {
		return nil
	}
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{SessionStorage: st, Device: device})
	err = client.Run(ctx, func(ctx context.Context) error {
		return endSession(ctx, client.Auth(), func(ctx context.Context) error {
			_, err := client.API().AuthLogOut(ctx)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("telegram: log out: %w", err)
	}
	return nil
}

// endSession logs the session out unless it is not signed in, as when the
// login never finished or the account ended it from another app.
func endSession(ctx context.Context, ac authClient, logOut func(context.Context) error) error {
	status, err := ac.Status(ctx)
	if err != nil {
		return fmt.Errorf("auth status: %w", err)
	}
	if !status.Authorized {
		return nil
	}
	return logOut(ctx)
}

// conn is a running Telegram account.
type conn struct {
	h   bridge.Host
	cfg config
	st  *saved

	saveEvery     time.Duration // how often update positions are saved
	backfillAge   time.Duration // chats quiet for longer are not fetched
	backfillChats int
	backfillCount int           // messages fetched per chat
	pause         time.Duration // between history calls, to go easy

	mu      sync.Mutex
	api     *tg.Client // while logged in
	dialogs time.Time  // when they were last fetched to find a chat
}

// Run connects, logs in, and passes on updates until ctx ends.
func (c *conn) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go c.st.saveLoop(ctx, c.saveEvery)
	defer c.st.flush(context.WithoutCancel(ctx))

	q := newQueue()
	go func() { cancel(c.receiveLoop(ctx, q)) }()

	mgr := updates.New(updates.Config{
		Handler:          &receiver{st: c.st, q: q},
		Storage:          c.st,
		AccessHasher:     c.st,
		UserAccessHasher: c.st,
	})
	client := telegram.NewClient(c.cfg.APIID, c.cfg.APIHash, telegram.Options{
		SessionStorage: c.st,
		UpdateHandler:  mgr,
		// The hook passes the updates in API results, such as a sent
		// message, to the manager, to keep its positions right.
		Middlewares: []telegram.Middleware{floodWait{max: maxFloodWait}, hook.UpdateHook(mgr.Handle)},
		Device:      device,
	})
	err := client.Run(ctx, func(ctx context.Context) error {
		if err := login(ctx, client.Auth(), c.cfg.Phone, c.h); err != nil {
			return err
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("telegram: get own account: %w", err)
		}
		c.st.loggedIn(self)
		if err := c.st.flush(ctx); err != nil {
			return err
		}
		api := client.API()
		c.setAPI(api)
		defer c.setAPI(nil)
		// The manager fetches what was missed since the saved positions,
		// then follows updates, recovering gaps after reconnects.
		return mgr.Run(ctx, api, self.ID, updates.AuthOptions{OnStart: func(ctx context.Context) {
			c.h.Connected(ctx)
			go c.backfill(ctx, api, q)
		}})
	})
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("telegram: %w", err)
	}
	return err
}

// receiveLoop passes queued messages to the host, apart from the
// goroutines that run sends.
func (c *conn) receiveLoop(ctx context.Context, q *queue) error {
	for {
		in, err := q.pop(ctx)
		if err != nil {
			return err
		}
		if err := c.h.Receive(ctx, in); err != nil {
			return fmt.Errorf("telegram: receive: %w", err)
		}
	}
}

func (c *conn) setAPI(api *tg.Client) {
	c.mu.Lock()
	c.api = api
	c.mu.Unlock()
}

func (c *conn) client() *tg.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.api
}

// Send posts a message as the account owner and returns its id.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	api := c.client()
	if api == nil {
		return "", bridge.Rejected(errors.New("telegram: not connected"))
	}
	peer, err := c.inputPeer(ctx, api, m.Chat)
	if err != nil {
		return "", bridge.Rejected(err)
	}
	var b [8]byte
	rand.Read(b[:])
	// Telegram drops a repeat of the same random id, so a retry after a
	// flood wait cannot post twice.
	randomID := int64(binary.LittleEndian.Uint64(b[:]))
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: m.Text, RandomID: randomID}
	if id, err := strconv.Atoi(m.ReplyTo); err == nil && id > 0 {
		req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: id}
	}
	res, err := api.MessagesSendMessage(ctx, req)
	if err != nil {
		return "", callErr(err)
	}
	id, ok := sentID(res, randomID)
	if !ok {
		return "", errors.New("telegram: the message was sent but its id is unknown")
	}
	return strconv.Itoa(id), nil
}

// Edit changes the text of a message the owner sent.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	api := c.client()
	if api == nil {
		return errors.New("telegram: not connected")
	}
	id, err := strconv.Atoi(remoteID)
	if err != nil {
		return fmt.Errorf("telegram: bad message id %q", remoteID)
	}
	peer, err := c.inputPeer(ctx, api, m.Chat)
	if err != nil {
		return err
	}
	_, err = api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: peer, ID: id, Message: m.Text})
	if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return nil
	}
	if err != nil {
		return callErr(err)
	}
	return nil
}

// callErr wraps an API error, as a refusal when Telegram answered with
// one: then the request surely did nothing. RANDOM_ID_DUPLICATE is the
// exception: gotd resends a request whose answer was lost, and Telegram
// says so when the first one was already posted.
func callErr(err error) error {
	if tgerr.Is(err, "RANDOM_ID_DUPLICATE") {
		return fmt.Errorf("telegram: may have been sent: %w", err)
	}
	if rpc, ok := tgerr.As(err); ok && rpc.Code >= 400 && rpc.Code < 500 {
		return bridge.Rejected(fmt.Errorf("telegram: %w", err))
	}
	return fmt.Errorf("telegram: %w", err)
}

// inputPeer addresses a chat. A user or channel needs its access hash;
// one not in the cache is looked for in the dialogs.
func (c *conn) inputPeer(ctx context.Context, api *tg.Client, chat string) (tg.InputPeerClass, error) {
	if p, ok := c.st.inputPeer(chat); ok {
		return p, nil
	}
	c.mu.Lock()
	recent := time.Since(c.dialogs) < dialogsEvery
	if !recent {
		c.dialogs = time.Now()
	}
	c.mu.Unlock()
	if !recent {
		if _, err := c.fetchDialogs(ctx, api, 100); err != nil {
			return nil, err
		}
		if p, ok := c.st.inputPeer(chat); ok {
			return p, nil
		}
	}
	return nil, fmt.Errorf("telegram: unknown chat %s", chat)
}

// inputPeer addresses a chat from the cache.
func (st *saved) inputPeer(chat string) (tg.InputPeerClass, bool) {
	kind, num, _ := strings.Cut(chat, ":")
	id, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return nil, false
	}
	switch kind {
	case "chat":
		return &tg.InputPeerChat{ChatID: id}, true
	case "user":
		if id == st.self() {
			return &tg.InputPeerSelf{}, true
		}
		if h, ok := st.hash(chat); ok {
			return &tg.InputPeerUser{UserID: id, AccessHash: h}, true
		}
	case "channel":
		if h, ok := st.hash(chat); ok {
			return &tg.InputPeerChannel{ChannelID: id, AccessHash: h}, true
		}
	}
	return nil, false
}

// fetchDialogs gets the most recent chats, caching their peers.
func (c *conn) fetchDialogs(ctx context.Context, api *tg.Client, limit int) (tg.ModifiedMessagesDialogs, error) {
	res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("telegram: get dialogs: %w", err)
	}
	d, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	c.st.remember(d.GetUsers(), d.GetChats())
	return d, nil
}

// backfill queues the latest messages of recently active chats, oldest
// first. It runs once per connection, beside the live updates; the host
// drops what it has seen.
func (c *conn) backfill(ctx context.Context, api *tg.Client, q *queue) {
	if err := c.fetchBackfill(ctx, api, q); err != nil && ctx.Err() == nil {
		slog.Warn("telegram: backfill failed", "err", err)
	}
}

func (c *conn) fetchBackfill(ctx context.Context, api *tg.Client, q *queue) error {
	d, err := c.fetchDialogs(ctx, api, c.backfillChats)
	if err != nil || d == nil {
		return err
	}
	// When each dialog's top message was sent.
	dates := map[string]int{}
	for _, mc := range d.GetMessages() {
		var peer tg.PeerClass
		var date int
		switch m := mc.(type) {
		case *tg.Message:
			peer, date = m.PeerID, m.Date
		case *tg.MessageService:
			peer, date = m.PeerID, m.Date
		default:
			continue
		}
		if id, ok := peerID(peer); ok {
			dates[id+"/"+strconv.Itoa(mc.GetID())] = date
		}
	}
	since := time.Now().Add(-c.backfillAge).Unix()
	for _, dc := range d.GetDialogs() {
		dlg, ok := dc.(*tg.Dialog)
		if !ok {
			continue
		}
		chat, ok := peerID(dlg.Peer)
		if !ok || int64(dates[chat+"/"+strconv.Itoa(dlg.TopMessage)]) < since {
			continue
		}
		peer, ok := c.st.inputPeer(chat)
		if !ok {
			continue
		}
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: c.backfillCount})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// A chat that cannot be read, such as a channel left since,
			// does not stop the others.
			slog.Warn("telegram: backfill chat failed", "chat", chat, "err", err)
			continue
		}
		if h, ok := res.AsModified(); ok {
			c.st.remember(h.GetUsers(), h.GetChats())
			msgs := h.GetMessages()
			// Newest first from the server.
			for i := len(msgs) - 1; i >= 0; i-- {
				if in, ok := incoming(c.st, msgs[i]); ok {
					q.push(in)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pause):
		}
	}
	return nil
}

// floodWait waits out a FLOOD_WAIT and calls again, a few times. A wait
// longer than max, or past the call's deadline, returns the error.
type floodWait struct{ max time.Duration }

func (f floodWait) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		for try := 0; ; try++ {
			err := next.Invoke(ctx, in, out)
			d, ok := tgerr.AsFloodWait(err)
			if !ok || try == 2 || d > f.max {
				return err
			}
			d += time.Second
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
				return err
			}
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
}
