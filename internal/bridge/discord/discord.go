package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/say-v2/internal/bridge"
	"github.com/lum1n/say-v2/internal/connector"
	"github.com/lum1n/say-v2/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:         "discord",
		Title:        "Discord",
		Modes:        []string{store.ModeBot},
		Fields:       fields,
		Capabilities: []connector.Capability{connector.CapEdits},
		Open:         open,
	})
}

var fields = []bridge.Field{
	{
		Key: "bot_token", Label: "Bot token", Kind: "secret", Required: true,
		Help: "Create an application at discord.com/developers/applications and add a bot to it. Under Bot turn on the " +
			"Message Content intent and copy the bot's token. Invite the bot to your server (OAuth2, scope bot) with the " +
			"View Channels, Send Messages, Read Message History and Manage Webhooks permissions; with Manage Webhooks " +
			"messages from say show under the writer's name. Link a room by the channel's id: turn on Developer Mode in " +
			"Discord's settings, then right-click the channel and Copy Channel ID.",
	},
}

// config is a bridge's settings.
type config struct {
	BotToken string `json:"bot_token"`
}

// state is what a connection keeps between runs: the newest message seen
// in each channel, to fetch what was missed while it was away, and the
// ids of the webhooks it posts through, to know their messages.
type state struct {
	Last  map[string]string `json:"last,omitempty"`
	Hooks map[string]string `json:"hooks,omitempty"` // channel: webhook id
}

// open makes a connection; it only checks the config.
func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, fmt.Errorf("discord: bad config: %w", err)
	}
	cfg.BotToken = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cfg.BotToken), "Bot "))
	if cfg.BotToken == "" {
		return nil, errors.New("discord: the bot token is required")
	}
	var st state
	if len(b.State) > 0 {
		// A state that does not parse only costs the catch-up.
		json.Unmarshal(b.State, &st)
	}
	if st.Last == nil {
		st.Last = map[string]string{}
	}
	if st.Hooks == nil {
		st.Hooks = map[string]string{}
	}
	return &conn{
		h:          h,
		cfg:        cfg,
		api:        "https://discord.com/api/v10",
		http:       &http.Client{Timeout: 30 * time.Second},
		catchUpMax: 500,
		saveEvery:  30 * time.Second,
		linkedTTL:  5 * time.Second,
		readyWait:  30 * time.Second,
		buckets:    map[string]time.Time{},
		names:      map[string]string{},
		last:       st.Last,
		hookIDs:    st.Hooks,
		hooks:      map[string]*webhook{},
		noHook:     map[string]time.Time{},
		sentBy:     map[string]string{},
	}, nil
}

// conn is a running Discord bot.
type conn struct {
	h    bridge.Host
	cfg  config
	api  string // REST base URL, without a trailing /
	http *http.Client
	// gateway, if set, is dialed instead of asking for the gateway's URL.
	gateway string

	catchUpMax int           // messages fetched of a channel on connect
	saveEvery  time.Duration // how often changed state is saved
	linkedTTL  time.Duration // how stale the linked channels may get
	readyWait  time.Duration // how long to wait for READY

	// hookMu serializes finding and making webhooks.
	hookMu sync.Mutex

	mu       sync.Mutex
	self     string // the bot's user id
	appID    string // the bot's application id
	global   time.Time
	buckets  map[string]time.Time // route: no calls until
	names    map[string]string    // channel id: name
	linked   map[string]bool
	linkedAt time.Time
	last     map[string]string // channel id: newest message id seen
	// behind holds, for channels not caught up yet, the newest id seen
	// before this run; until the gap is filled it is what is saved.
	behind  map[string]string
	hookIDs map[string]string    // channel: id of its say webhook
	hooks   map[string]*webhook  // channel: say webhook, with token
	noHook  map[string]time.Time // channel: no webhook allowed, as of
	sentBy  map[string]string    // message id: "hook" or "bot"
	dirty   bool
}

// Run connects to the Gateway, catches up and receives until ctx ends.
func (c *conn) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		sctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		c.saveState(sctx)
		done()
	}()

	ws, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	s := &session{c: c, ws: ws}
	interval, err := s.hello(ctx)
	if err != nil {
		return err
	}
	if err := s.identify(ctx); err != nil {
		return err
	}
	// The gateway reads into a queue and Run hands what arrives to the
	// core, so Sends (plain HTTP calls) never wait on the reader.
	events := make(chan message, 256)
	ready := make(chan struct{})
	errc := make(chan error, 3)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errc <- s.read(ctx, ready, events)
	}()
	go func() {
		defer wg.Done()
		errc <- s.heartbeat(ctx, interval)
	}()
	select {
	case <-ready:
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.readyWait):
		return errors.New("discord: the gateway did not get ready")
	}

	// Where to catch up from is taken before any live message is
	// handled; channels never seen start from now.
	chats, err := c.h.Chats(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	from := maps.Clone(c.last)
	c.behind = maps.Clone(c.last)
	c.mu.Unlock()
	c.setLinked(chats)
	c.h.Connected(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.catchUp(ctx, chats, from); err != nil && ctx.Err() == nil {
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

// isLinked reports whether a channel is linked to a room, asking the core
// again when the answer is no and a little old.
func (c *conn) isLinked(ctx context.Context, id string) bool {
	c.mu.Lock()
	ok, stale := c.linked[id], time.Since(c.linkedAt) > c.linkedTTL
	c.mu.Unlock()
	if ok || !stale {
		return ok
	}
	chats, err := c.h.Chats(ctx)
	if err != nil {
		return false
	}
	c.setLinked(chats)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.linked[id]
}

// setLinked notes the linked channels. One with nothing seen yet starts
// from now: a bot does not backfill, but what comes after is caught up.
func (c *conn) setLinked(chats []string) {
	now := snowflake(time.Now())
	c.mu.Lock()
	defer c.mu.Unlock()
	c.linked = map[string]bool{}
	for _, id := range chats {
		c.linked[id] = true
		if _, ok := c.last[id]; !ok {
			c.last[id] = now
			c.dirty = true
		}
	}
	c.linkedAt = time.Now()
}

// catchUp receives what linked channels got while the bot was away, from
// the newest message seen of each before this run.
func (c *conn) catchUp(ctx context.Context, chats []string, from map[string]string) error {
	for _, ch := range chats {
		after, ok := from[ch]
		if !ok {
			continue
		}
		msgs, err := c.history(ctx, ch, after)
		if _, ok := errors.AsType[*apiError](err); ok {
			// Gone, or the bot may not read it.
			c.caughtUp(ch)
			continue
		}
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if err := c.receive(ctx, m); err != nil {
				return err
			}
		}
		c.caughtUp(ch)
	}
	// Channels no longer linked need no catching up, nor a place kept.
	c.mu.Lock()
	clear(c.behind)
	for id := range c.last {
		if !c.linked[id] {
			delete(c.last, id)
		}
	}
	c.dirty = true
	c.mu.Unlock()
	return c.saveState(ctx)
}

// history returns up to catchUpMax messages of a channel after a message
// id, oldest first.
func (c *conn) history(ctx context.Context, ch, after string) ([]message, error) {
	var out []message
	for len(out) < c.catchUpMax {
		var page []message
		path := fmt.Sprintf("/channels/%s/messages?after=%s&limit=100", ch, after)
		if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		sortByID(page)
		for i := range page {
			page[i].ChannelID = ch
		}
		out = append(out, page...)
		if len(page) < 100 {
			break
		}
		after = page[len(page)-1].ID
	}
	if len(out) > c.catchUpMax {
		out = out[:c.catchUpMax]
	}
	return out, nil
}

// seen notes the newest message of a channel, for catching up later.
func (c *conn) seen(ch, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.last[ch]; !ok || snowLess(cur, id) {
		c.last[ch] = id
		c.dirty = true
	}
}

// caughtUp notes that a channel's missed messages are in, so its newest
// message may be saved.
func (c *conn) caughtUp(ch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.behind[ch]; ok {
		delete(c.behind, ch)
		c.dirty = true
	}
}

// saveState saves the newest message per channel and the webhook ids, if
// they changed.
func (c *conn) saveState(ctx context.Context) error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	last := maps.Clone(c.last)
	maps.Copy(last, c.behind)
	data, err := json.Marshal(state{Last: last, Hooks: maps.Clone(c.hookIDs)})
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
