package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sort"
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
		Name:         "slack",
		Title:        "Slack",
		Modes:        []string{store.ModeAccount},
		Fields:       fields,
		Capabilities: []connector.Capability{connector.CapEdits, connector.CapReplies},
		Open:         open,
	})
}

var fields = []bridge.Field{
	{
		Key: "user_token", Label: "User token", Kind: "secret", Required: true, Placeholder: "xoxp-…",
		Help: "Create a Slack app for your workspace at api.slack.com/apps. Under OAuth & Permissions add these User Token Scopes: " +
			"channels:history, groups:history, im:history, mpim:history, channels:read, groups:read, im:read, mpim:read, users:read, chat:write. " +
			"Install the app to the workspace and paste the User OAuth Token (xoxp-…). Messages you write in say are posted as you.",
	},
	{
		Key: "app_token", Label: "App-level token", Kind: "secret", Required: true, Placeholder: "xapp-…",
		Help: "In the app's settings turn Socket Mode on, so say needs no public URL. Under Event Subscriptions turn events on and " +
			"subscribe to these events on behalf of users: message.channels, message.groups, message.im, message.mpim. " +
			"Then under Basic Information make an App-Level Token with the connections:write scope and paste it (xapp-…). " +
			"Reinstall the app if Slack asks you to.",
	},
}

// config is a bridge's settings.
type config struct {
	UserToken string `json:"user_token"`
	AppToken  string `json:"app_token"`
}

// state is what a connection keeps between runs: the newest message
// seen in each chat, to fetch what was missed while it was away.
type state struct {
	Last map[string]string `json:"last,omitempty"`
}

// open makes a connection; it only checks the config.
func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, fmt.Errorf("slack: bad config: %w", err)
	}
	if strings.HasPrefix(cfg.UserToken, "xoxc-") || strings.HasPrefix(cfg.UserToken, "xoxd-") {
		// A token lifted from the web client would work, but it breaks
		// Slack's terms and can get the account banned (ADR 0004).
		return nil, errors.New("slack: web client tokens (xoxc-, xoxd-) are not supported; use a user token (xoxp-) from your own Slack app")
	}
	if !strings.HasPrefix(cfg.UserToken, "xoxp-") {
		return nil, errors.New("slack: the user token starts with xoxp-")
	}
	if !strings.HasPrefix(cfg.AppToken, "xapp-") {
		return nil, errors.New("slack: the app-level token starts with xapp-")
	}
	var st state
	if len(b.State) > 0 {
		// A state that does not parse only costs a fresh backfill.
		json.Unmarshal(b.State, &st)
	}
	if st.Last == nil {
		st.Last = map[string]string{}
	}
	return &conn{
		h:             h,
		cfg:           cfg,
		api:           "https://slack.com/api/",
		http:          &http.Client{Timeout: 30 * time.Second},
		backfillAge:   30 * 24 * time.Hour,
		backfillCount: 20,
		maxChats:      50,
		catchUpMax:    500,
		saveEvery:     30 * time.Second,
		users:         map[string]string{},
		chats:         map[string]bridge.RemoteChat{},
		last:          st.Last,
	}, nil
}

// conn is a running Slack account.
type conn struct {
	h    bridge.Host
	cfg  config
	api  string // base URL of the Web API, ending in /
	http *http.Client

	backfillAge   time.Duration // how far back a new chat is fetched
	backfillCount int           // messages fetched of a new chat
	maxChats      int           // new chats fetched on connect
	catchUpMax    int           // messages fetched of a known chat
	saveEvery     time.Duration

	mu    sync.Mutex
	self  string // the account's user id
	appID string // the Slack app's id, to skip its own bot messages
	users map[string]string
	chats map[string]bridge.RemoteChat
	last  map[string]string // chat id: newest ts seen
	// behind holds, for chats not caught up yet, the newest ts seen
	// before this run. Live messages move last past the gap that
	// catching up fills, so until it is filled, behind is what is saved.
	behind map[string]string
	dirty  bool
}

// message is a Slack message, in an event or in history.
type message struct {
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
	Channel    string `json:"channel"`
	User       string `json:"user"`
	Text       string `json:"text"`
	Ts         string `json:"ts"`
	ThreadTs   string `json:"thread_ts"`
	BotID      string `json:"bot_id"`
	AppID      string `json:"app_id"`
	Username   string `json:"username"`
	Hidden     bool   `json:"hidden"`
	Files      []file `json:"files"`
	BotProfile *struct {
		Name  string `json:"name"`
		AppID string `json:"app_id"`
	} `json:"bot_profile"`
	// Message and PreviousMessage are set on message_changed.
	Message         *message `json:"message"`
	PreviousMessage *message `json:"previous_message"`
}

// channel is a conversation as the Web API describes it.
type channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsIM     bool   `json:"is_im"`
	IsMPIM   bool   `json:"is_mpim"`
	User     string `json:"user"` // the other person of an im
	IsMember *bool  `json:"is_member"`
}

// Run logs in, opens Socket Mode, catches up and receives until ctx ends.
func (c *conn) Run(ctx context.Context) error {
	var auth struct {
		response
		UserID string `json:"user_id"`
	}
	if err := c.call(ctx, "auth.test", c.cfg.UserToken, nil, &auth); err != nil {
		return fmt.Errorf("check the user token: %w", err)
	}
	// Where to catch up from is taken before any live message arrives.
	c.mu.Lock()
	c.self = auth.UserID
	c.behind = maps.Clone(c.last)
	from := maps.Clone(c.last)
	c.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		sctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		c.saveState(sctx)
		done()
	}()

	ws, err := c.connect(ctx)
	if err != nil {
		return err
	}
	// The socket reads into a queue and Run hands what arrives to the
	// core, so a slow Receive never holds up Slack's acks for long, and
	// Sends (plain HTTP calls) never wait on the reader.
	events := make(chan message, 256)
	errc := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errc <- c.socket(ctx, ws, events)
	}()
	c.h.Connected(ctx)
	// Catching up runs beside live messages: history is rate limited and
	// may take minutes, which live messages should not wait for.
	go func() {
		defer wg.Done()
		if err := c.backfill(ctx, from); err != nil && ctx.Err() == nil {
			errc <- err
		}
	}()

	tick := time.NewTicker(c.saveEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case m := <-events:
			if err := c.receive(ctx, m); err != nil {
				return err
			}
		case <-tick.C:
			if err := c.saveState(ctx); err != nil {
				return err
			}
		}
	}
}

// Send posts as the account's user and returns the message's ts, which
// the echo event carries too.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	var r struct {
		response
		Ts string `json:"ts"`
	}
	p := url.Values{"channel": {m.Chat}, "text": {escape(m.Text)}}
	if m.ReplyTo != "" {
		// Slack has no quotes: a reply goes in the message's thread.
		p.Set("thread_ts", m.ReplyTo)
	}
	err := c.call(ctx, "chat.postMessage", c.cfg.UserToken, p, &r)
	if _, ok := errors.AsType[*apiError](err); ok {
		return "", bridge.Rejected(err)
	}
	if err != nil {
		return "", err
	}
	return r.Ts, nil
}

// Edit changes a message the account sent.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	var r response
	p := url.Values{"channel": {m.Chat}, "ts": {remoteID}, "text": {escape(m.Text)}}
	return c.call(ctx, "chat.update", c.cfg.UserToken, p, &r)
}

// receive hands a message to the core, if it is one to show.
func (c *conn) receive(ctx context.Context, m message) error {
	in, ok := c.incoming(ctx, m)
	if !ok {
		return nil
	}
	if err := c.h.Receive(ctx, in); err != nil {
		return err
	}
	if !in.Edit {
		c.seen(m.Channel, in.ID)
	}
	return nil
}

// incoming converts a Slack message; false means it is not shown.
func (c *conn) incoming(ctx context.Context, m message) (bridge.Incoming, bool) {
	if m.Channel == "" {
		return bridge.Incoming{}, false
	}
	switch m.Subtype {
	case "message_changed":
		n := m.Message
		if n == nil || n.Ts == "" || n.Subtype != "" && n.Subtype != "file_share" && n.Subtype != "thread_broadcast" {
			return bridge.Incoming{}, false
		}
		// Slack also reports link previews being added as a change.
		if p := m.PreviousMessage; p != nil && p.Text == n.Text && len(p.Files) == len(n.Files) {
			return bridge.Incoming{}, false
		}
		a, ok := c.author(ctx, *n)
		if !ok {
			return bridge.Incoming{}, false
		}
		return bridge.Incoming{Chat: c.chat(ctx, m.Channel), ID: n.Ts, Author: a, Text: c.text(ctx, *n), Edit: true}, true
	case "", "file_share", "thread_broadcast", "me_message":
	case "bot_message":
		c.mu.Lock()
		app := c.appID
		c.mu.Unlock()
		if app != "" && (m.AppID == app || m.BotProfile != nil && m.BotProfile.AppID == app) {
			return bridge.Incoming{}, false
		}
	default:
		// Joins, topic changes, deletions and the like.
		return bridge.Incoming{}, false
	}
	if m.Hidden || m.Ts == "" {
		return bridge.Incoming{}, false
	}
	a, ok := c.author(ctx, m)
	if !ok {
		return bridge.Incoming{}, false
	}
	in := bridge.Incoming{Chat: c.chat(ctx, m.Channel), ID: m.Ts, Author: a, Text: c.text(ctx, m), Time: tsTime(m.Ts)}
	if m.ThreadTs != "" && m.ThreadTs != m.Ts {
		in.ReplyTo = m.ThreadTs
	}
	return in, true
}

// author returns who wrote a message.
func (c *conn) author(ctx context.Context, m message) (bridge.Author, bool) {
	c.mu.Lock()
	self := c.self
	c.mu.Unlock()
	switch {
	case m.User != "" && m.Subtype != "bot_message":
		return bridge.Author{ID: m.User, Name: c.userName(ctx, m.User), Self: m.User == self}, true
	case m.BotID != "":
		name := m.Username
		if name == "" && m.BotProfile != nil {
			name = m.BotProfile.Name
		}
		if name == "" {
			name = m.BotID
		}
		return bridge.Author{ID: m.BotID, Name: name}, true
	case m.User != "":
		return bridge.Author{ID: m.User, Name: c.userName(ctx, m.User), Self: m.User == self}, true
	}
	return bridge.Author{}, false
}

// text renders a message's text and files.
func (c *conn) text(ctx context.Context, m message) string {
	return plainText(m.Text, m.Files, names{
		user:    func(id string) string { return c.userName(ctx, id) },
		channel: func(id string) string { return c.chat(ctx, id).Name },
	})
}

// userName returns a user's display name, from cache or users.info.
func (c *conn) userName(ctx context.Context, id string) string {
	c.mu.Lock()
	n, ok := c.users[id]
	c.mu.Unlock()
	if ok {
		return n
	}
	var r struct {
		response
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", c.cfg.UserToken, url.Values{"user": {id}}, &r); err != nil {
		// Not cached: the next message tries again.
		return id
	}
	u := r.User
	n = cmp.Or(u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name, id)
	c.mu.Lock()
	c.users[id] = n
	c.mu.Unlock()
	return n
}

// chat returns a conversation, from cache or conversations.info.
func (c *conn) chat(ctx context.Context, id string) bridge.RemoteChat {
	c.mu.Lock()
	ch, ok := c.chats[id]
	c.mu.Unlock()
	if ok {
		return ch
	}
	var r struct {
		response
		Channel channel `json:"channel"`
	}
	if err := c.call(ctx, "conversations.info", c.cfg.UserToken, url.Values{"channel": {id}}, &r); err != nil {
		// Im ids start with D; the name is only a first guess for a
		// new room.
		return bridge.RemoteChat{ID: id, Name: id, Direct: strings.HasPrefix(id, "D")}
	}
	r.Channel.ID = id
	return c.remember(ctx, r.Channel)
}

// remember caches a conversation, naming it as people see it.
func (c *conn) remember(ctx context.Context, ch channel) bridge.RemoteChat {
	rc := bridge.RemoteChat{ID: ch.ID, Name: ch.Name}
	switch {
	case ch.IsIM:
		rc.Direct = true
		rc.Name = c.userName(ctx, ch.User)
	case ch.IsMPIM:
		// Group DMs are named mpdm-ada--bob--cy-1.
		n := strings.TrimPrefix(ch.Name, "mpdm-")
		if i := strings.LastIndexByte(n, '-'); i > 0 {
			n = n[:i]
		}
		rc.Name = strings.Join(strings.Split(n, "--"), ", ")
	}
	if rc.Name == "" {
		rc.Name = ch.ID
	}
	c.mu.Lock()
	c.chats[ch.ID] = rc
	c.mu.Unlock()
	return rc
}

// backfill fetches recent history: what was missed in chats seen before,
// then the last messages of other chats active in the last 30 days. last
// is the newest message per chat seen before this run.
func (c *conn) backfill(ctx context.Context, last map[string]string) error {
	chats, err := c.conversations(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(chats, func(i, j int) bool {
		_, a := last[chats[i].ID]
		_, b := last[chats[j].ID]
		return a && !b
	})
	since := formatTs(time.Now().Add(-c.backfillAge))
	fresh := 0
	for _, ch := range chats {
		from, known := last[ch.ID]
		if !known && fresh >= c.maxChats {
			continue
		}
		var msgs []message
		if known {
			msgs, err = c.history(ctx, ch.ID, maxTs(from, since), c.catchUpMax, 200)
		} else {
			msgs, err = c.history(ctx, ch.ID, since, c.backfillCount, c.backfillCount)
		}
		if _, ok := errors.AsType[*apiError](err); ok {
			// Left, archived or out of scope since it was listed.
			c.caughtUp(ch.ID)
			continue
		}
		if err != nil {
			return err
		}
		if len(msgs) > 0 && !known {
			fresh++
		}
		if len(msgs) > 0 {
			c.remember(ctx, ch)
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			m.Channel = ch.ID
			if err := c.receive(ctx, m); err != nil {
				return err
			}
		}
		c.caughtUp(ch.ID)
	}
	// Chats no longer listed have nothing to catch up.
	c.mu.Lock()
	clear(c.behind)
	c.dirty = true
	c.mu.Unlock()
	return c.saveState(ctx)
}

// conversations lists the chats the account is in.
func (c *conn) conversations(ctx context.Context) ([]channel, error) {
	var out []channel
	cursor := ""
	for page := 0; page < 20; page++ {
		var r struct {
			response
			Channels []channel `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		p := url.Values{
			"types":            {"im,mpim,private_channel,public_channel"},
			"exclude_archived": {"true"},
			"limit":            {"200"},
		}
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		// users.conversations lists only the user's own chats, where
		// conversations.list would page through every public channel.
		if err := c.call(ctx, "users.conversations", c.cfg.UserToken, p, &r); err != nil {
			return nil, err
		}
		for _, ch := range r.Channels {
			if ch.IsMember == nil || *ch.IsMember {
				out = append(out, ch)
			}
		}
		cursor = r.Meta.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// history returns up to max messages of a chat after oldest, newest
// first, perPage at a time.
func (c *conn) history(ctx context.Context, id, oldest string, max, perPage int) ([]message, error) {
	var out []message
	cursor := ""
	for len(out) < max {
		var r struct {
			response
			Messages []message `json:"messages"`
			HasMore  bool      `json:"has_more"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		p := url.Values{"channel": {id}, "oldest": {oldest}, "limit": {strconv.Itoa(min(perPage, max-len(out)))}}
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		if err := c.call(ctx, "conversations.history", c.cfg.UserToken, p, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Messages...)
		cursor = r.Meta.NextCursor
		if !r.HasMore || cursor == "" {
			break
		}
	}
	return out, nil
}

// seen notes the newest message of a chat, for catching up later.
func (c *conn) seen(chat, ts string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.last[chat]; !ok || tsLess(cur, ts) {
		c.last[chat] = ts
		c.dirty = true
	}
}

// caughtUp notes that a chat's missed messages are in, so its newest
// message may be saved.
func (c *conn) caughtUp(chat string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.behind[chat]; ok {
		delete(c.behind, chat)
		c.dirty = true
	}
}

// saveState saves the newest message per chat, if it changed; for a chat
// still catching up, the newest from before this run.
func (c *conn) saveState(ctx context.Context) error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	last := maps.Clone(c.last)
	maps.Copy(last, c.behind)
	data, err := json.Marshal(state{Last: last})
	c.dirty = false
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := c.h.SaveState(ctx, data); err != nil {
		c.mu.Lock()
		c.dirty = true
		c.mu.Unlock()
		return err
	}
	return nil
}

// A ts is seconds and microseconds since the epoch, "1700000000.000100".

func tsParts(ts string) (sec, usec int64) {
	s, u, _ := strings.Cut(ts, ".")
	sec, _ = strconv.ParseInt(s, 10, 64)
	usec, _ = strconv.ParseInt(u, 10, 64)
	return sec, usec
}

func tsTime(ts string) time.Time {
	sec, usec := tsParts(ts)
	return time.Unix(sec, usec*1000)
}

func tsLess(a, b string) bool {
	as, au := tsParts(a)
	bs, bu := tsParts(b)
	return as < bs || as == bs && au < bu
}

func maxTs(a, b string) string {
	if tsLess(a, b) {
		return b
	}
	return a
}

func formatTs(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}
