package irc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:  "irc",
		Title: "IRC",
		Modes: []string{store.ModeAccount},
		// Reactions need a server with message-tags; elsewhere they are
		// dropped.
		Capabilities: []connector.Capability{connector.CapReactions},
		Fields: []bridge.Field{
			{Key: "server", Label: "Server", Kind: "text", Required: true, Placeholder: "irc.libera.chat:6697", Help: "Host and port."},
			{Key: "tls", Label: "Use TLS", Kind: "bool"},
			{Key: "nick", Label: "Nick", Kind: "text", Required: true},
			{Key: "username", Label: "Username", Kind: "text", Help: "Also the SASL account name. Defaults to the nick."},
			{Key: "realname", Label: "Real name", Kind: "text"},
			{Key: "sasl_password", Label: "SASL password", Kind: "secret", Help: "Your account's password, to log in with SASL."},
			{Key: "server_password", Label: "Server password", Kind: "secret", Help: "Sent with PASS, for servers and bouncers that want one."},
			{Key: "channels", Label: "Channels", Kind: "text", Placeholder: "#go-nuts, #say", Help: "Joined on every connect, comma-separated."},
		},
		Open: open,
	})
}

const (
	// maxLine is how many bytes of text go in one PRIVMSG, leaving room
	// for the prefix the server adds within IRC's 512-byte lines.
	maxLine = 400
	// backfillAge, backfillChats and backfillLines bound what is read
	// back on connect.
	backfillAge   = 30 * 24 * time.Hour
	backfillChats = 50
	backfillLines = 20
	// catchUpLines is how much a chat seen before reads after a
	// reconnect.
	catchUpLines = 100
	// requestTimeout bounds a JOIN or CHATHISTORY waiting for its reply.
	requestTimeout = 15 * time.Second
	// saveEvery is how often changed state is saved.
	saveEvery = 30 * time.Second
	// tsFormat is the timestamp format of IRCv3 server-time and
	// CHATHISTORY.
	tsFormat = "2006-01-02T15:04:05.000Z"
)

var (
	errNotConnected = errors.New("not connected to IRC")
	errNoReply      = errors.New("the server did not answer")
)

// config is a bridge's settings.
type config struct {
	Server         string `json:"server"`
	TLS            bool   `json:"tls"`
	Nick           string `json:"nick"`
	Username       string `json:"username"`
	RealName       string `json:"realname"`
	SASLPassword   string `json:"sasl_password"`
	ServerPassword string `json:"server_password"`
	Channels       string `json:"channels"`
}

// state is what the bridge saves between runs: when it last saw a line in
// each chat, to read what it missed after a reconnect, and the ids of the
// lines it sent lately, to know them in that history.
type state struct {
	Seen map[string]time.Time `json:"seen"`
	Sent map[string][]string  `json:"sent,omitempty"`
}

// maxSent is how many ids of sent lines are kept per chat.
const maxSent = 100

// conn is a bridge's connection. Each Run makes a new session.
type conn struct {
	cfg config
	h   bridge.Host

	mu   sync.Mutex
	sess *session
	// seen is when a line was last seen in each chat. held keeps the
	// time a chat had when the run began until its catch-up is read: a
	// live line moves seen to now, and saving that before the catch-up
	// would lose what was missed.
	seen  map[string]time.Time
	held  map[string]time.Time
	sent  map[string][]string
	dirty bool
}

func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, fmt.Errorf("irc: config: %w", err)
	}
	if cfg.Server == "" || cfg.Nick == "" {
		return nil, errors.New("irc: a server and a nick are required")
	}
	if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
		return nil, fmt.Errorf("irc: the server is host:port: %w", err)
	}
	c := &conn{cfg: cfg, h: h, seen: map[string]time.Time{}, held: map[string]time.Time{}, sent: map[string][]string{}}
	var st state
	if len(b.State) > 0 && json.Unmarshal(b.State, &st) == nil {
		if st.Seen != nil {
			c.seen = st.Seen
		}
		if st.Sent != nil {
			c.sent = st.Sent
		}
	}
	return c, nil
}

// session is one connection to the server.
type session struct {
	irc *ircevent.Connection
	q   queue

	// Set from what the server agreed to, before the session is used.
	labels    bool   // labeled-response and batch
	echo      bool   // echo-message, with labels: Send learns msgids
	history   bool   // CHATHISTORY, with labels
	chantypes string // prefixes of channel names
	statusmsg string // prefixes that address part of a channel
	accounts  bool   // account-tag: lines say who is logged in as whom
	tags      bool   // message-tags: client tags such as reactions pass

	// nick is the configured nick, the owner's even while the server
	// has them under another.
	nick string
	// account is the account the session is logged in to, from SASL.
	mu      sync.Mutex
	account string
}

func (s *session) setAccount(a string) {
	s.mu.Lock()
	s.account = a
	s.mu.Unlock()
}

func (s *session) loggedIn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account
}

// isMe reports whether a nick is the owner's: the one they have now or
// the one they configured, which an earlier connection may have had.
func (s *session) isMe(nick string) bool {
	n := fold(nick)
	return n == fold(s.irc.CurrentNick()) || n == fold(s.nick)
}

// isSelf reports whether the owner wrote a line. A line with an account
// tag is theirs if the account is; one without falls back to the nick.
func (s *session) isSelf(m ircmsg.Message) bool {
	if acct := s.loggedIn(); acct != "" && s.accounts {
		if ok, a := m.GetTag("account"); ok && a != "*" {
			return fold(a) == fold(acct)
		}
	}
	return s.isMe(m.Nick())
}

func (c *conn) newSession() *session {
	cfg := c.cfg
	cl := &ircevent.Connection{
		Server:   cfg.Server,
		UseTLS:   cfg.TLS,
		Nick:     cfg.Nick,
		User:     cfg.Username,
		RealName: cfg.RealName,
		Password: cfg.ServerPassword,
		// draft/event-playback makes history hold joins and parts as
		// themselves, which are skipped, rather than as notes from a
		// HistServ that would read as messages.
		RequestCaps: []string{"server-time", "message-tags", "batch", "labeled-response", "echo-message", "draft/chathistory", "chathistory", "draft/event-playback", "account-tag"},
		QuitMessage: "say",
		Timeout:     30 * time.Second,
		KeepAlive:   4 * time.Minute,
		Log:         log.New(io.Discard, "", 0),
	}
	if cfg.SASLPassword != "" {
		cl.SASLLogin = cfg.Username
		if cl.SASLLogin == "" {
			cl.SASLLogin = cfg.Nick
		}
		cl.SASLPassword = cfg.SASLPassword
	}
	// A taken nick gets a suffix while registering (nick_0, nick_1, …),
	// and the preferred nick is tried again with every keepalive.
	s := &session{irc: cl, q: queue{wake: make(chan struct{}, 1)}, chantypes: "#&", nick: cfg.Nick}
	// 900 RPL_LOGGEDIN <client> <nick!user@host> <account> :text, and
	// 901 RPL_LOGGEDOUT.
	cl.AddCallback("900", func(m ircmsg.Message) {
		if len(m.Params) >= 3 {
			s.setAccount(m.Params[2])
		}
	})
	cl.AddCallback("901", func(ircmsg.Message) { s.setAccount("") })
	return s
}

// negotiated reads what the server agreed to during registration.
func (s *session) negotiated() {
	caps := s.irc.AcknowledgedCaps()
	has := func(name string) bool { _, ok := caps[name]; return ok }
	s.labels = has("batch") && has("labeled-response")
	if has("echo-message") && !s.labels {
		// An unlabeled echo of a line say sent looks like one the person
		// wrote elsewhere: rather no echoes than doubled messages.
		s.irc.Send("CAP", "REQ", "-echo-message")
	}
	s.echo = has("echo-message") && s.labels
	s.history = (has("draft/chathistory") || has("chathistory")) && s.labels
	s.accounts = has("account-tag")
	s.tags = has("message-tags")
	is := s.irc.ISupport()
	if v, ok := is["CHANTYPES"]; ok {
		s.chantypes = v
	}
	s.statusmsg = is["STATUSMSG"]
}

func (s *session) isChannel(name string) bool {
	return name != "" && strings.ContainsRune(s.chantypes, rune(name[0]))
}

// Run connects, joins the channels and receives until ctx ends or the
// server goes away.
func (c *conn) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// What was seen before this run, read before any line can move it.
	since := c.hold()
	s := c.newSession()
	cl := s.irc

	// ircevent's Connect takes no context: the socket is kept so that
	// closing it ends a connect in progress, and a session that hangs.
	var sockMu sync.Mutex
	var sock net.Conn
	cl.DialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
		nc, err := (&net.Dialer{}).DialContext(dctx, network, addr)
		if err == nil {
			sockMu.Lock()
			sock = nc
			sockMu.Unlock()
		}
		return nc, err
	}
	closeSock := func() {
		sockMu.Lock()
		defer sockMu.Unlock()
		if sock != nil {
			sock.Close()
		}
	}
	down := make(chan struct{})
	var downOnce sync.Once
	cl.AddDisconnectCallback(func(ircmsg.Message) { downOnce.Do(func() { close(down) }) })
	// Callbacks run on ircevent's read loop, which also completes Sends:
	// they only queue, and a goroutine of their own passes lines on.
	cl.AddCallback("PRIVMSG", func(m ircmsg.Message) { s.q.push(item{msg: m}) })
	cl.AddCallback("TAGMSG", func(m ircmsg.Message) { s.q.push(item{msg: m}) })

	stop := context.AfterFunc(ctx, closeSock)
	err := cl.Connect()
	stop()
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return fmt.Errorf("irc: connect to %s: %w", c.cfg.Server, err)
	}
	defer func() {
		cl.Quit()
		select {
		case <-down:
		case <-time.After(2 * time.Second):
		}
		closeSock()
	}()
	go func() {
		select {
		case <-down:
			cancel()
		case <-ctx.Done():
		}
	}()
	s.negotiated()

	errc := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.receive(ctx, s); err != nil {
			errc <- err
			cancel()
		}
	}()
	defer wg.Wait()
	defer cancel()

	joined := c.join(ctx, s)
	c.setSession(s)
	defer c.setSession(nil)
	c.h.Connected(ctx)
	c.backfill(ctx, s, joined, since)

	<-ctx.Done()
	select {
	case err := <-errc:
		return err
	default:
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return errors.New("irc: disconnected from the server")
}

func (c *conn) setSession(s *session) {
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
}

func (c *conn) session() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// join joins the configured channels and those of rooms already bridged,
// and returns the ones joined.
func (c *conn) join(ctx context.Context, s *session) []string {
	var want []string
	for _, ch := range strings.Split(c.cfg.Channels, ",") {
		ch = strings.TrimSpace(ch)
		if ch == "" {
			continue
		}
		if !s.isChannel(ch) {
			ch = "#" + ch
		}
		want = append(want, ch)
	}
	// Chats are stored lowercased, which IRC takes as the same channel.
	if chats, err := c.h.Chats(ctx); err == nil {
		for _, ch := range chats {
			if s.isChannel(ch) {
				want = append(want, ch)
			}
		}
	}
	var joined []string
	done := map[string]bool{}
	for _, ch := range want {
		if done[fold(ch)] || ctx.Err() != nil {
			continue
		}
		done[fold(ch)] = true
		if err := s.join(ctx, ch); err == nil {
			joined = append(joined, ch)
		}
		// A channel that cannot be joined (banned, invite only, gone)
		// is skipped: the others still work.
	}
	return joined
}

func (s *session) join(ctx context.Context, ch string) error {
	if !s.labels {
		return s.irc.Join(ch)
	}
	b, err := s.request(ctx, "JOIN", ch)
	if err != nil {
		return err
	}
	if err := failure(b); err != nil {
		return err
	}
	// A server may replay recent lines on join; they are in the reply.
	s.queueLines(b, true)
	return nil
}

// backfill reads recent history, when the server keeps it.
// since is when each chat was last seen before the run: those chats
// read what came after, and their markers are held until it is read.
func (c *conn) backfill(ctx context.Context, s *session, joined []string, since map[string]time.Time) {
	if !s.history {
		// Nothing missed can be read: live lines may move the markers.
		c.release(nil)
		return
	}
	targets := joined
	now := time.Now().UTC()
	b, err := s.request(ctx, "CHATHISTORY", "TARGETS", "timestamp="+now.Format(tsFormat), "timestamp="+now.Add(-backfillAge).Format(tsFormat), fmt.Sprint(backfillChats))
	if err == nil && failure(b) == nil {
		walk(b, func(m ircmsg.Message) {
			// Channels not joined cannot be read; private chats can.
			if m.Command == "CHATHISTORY" && len(m.Params) >= 2 && m.Params[0] == "TARGETS" && !s.isChannel(m.Params[1]) {
				targets = append(targets, m.Params[1])
			}
		})
	}
	if len(targets) > backfillChats {
		targets = targets[:backfillChats]
	}
	// Chats not read now had nothing new (a private chat missing from
	// TARGETS), cannot be read (not joined), or are past the cap.
	read := map[string]bool{}
	for _, t := range targets {
		read[fold(t)] = true
	}
	c.release(func(chat string) bool { return !read[chat] })
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		var b *ircevent.Batch
		var err error
		if at := since[fold(t)]; !at.IsZero() {
			b, err = s.request(ctx, "CHATHISTORY", "AFTER", t, "timestamp="+at.UTC().Format(tsFormat), fmt.Sprint(catchUpLines))
		} else {
			b, err = s.request(ctx, "CHATHISTORY", "LATEST", t, "*", fmt.Sprint(backfillLines))
		}
		if err != nil || failure(b) != nil {
			// The marker stays held, so the next run tries again.
			continue
		}
		s.queueLines(b, true)
		// Released once the lines before it are received.
		s.q.push(item{caughtUp: fold(t)})
	}
}

// queueLines queues the PRIVMSGs of a reply, in order.
func (s *session) queueLines(b *ircevent.Batch, backfill bool) {
	walk(b, func(m ircmsg.Message) {
		if m.Command == "PRIVMSG" {
			s.q.push(item{msg: m, backfill: backfill})
		}
	})
}

// receive passes queued lines to the host, and saves state now and then.
func (c *conn) receive(ctx context.Context, s *session) error {
	t := time.NewTicker(saveEvery)
	defer t.Stop()
	defer c.saveState(context.WithoutCancel(ctx))
	for {
		for _, it := range s.q.take() {
			if it.caughtUp != "" {
				c.release(func(chat string) bool { return chat == it.caughtUp })
				continue
			}
			in, ok := s.incoming(it)
			if !ok || (it.backfill && c.wasSent(in.Chat.ID, in.ID)) {
				continue
			}
			if err := c.h.Receive(ctx, in); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			c.markSeen(in)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.q.wake:
		case <-t.C:
			c.saveState(ctx)
		}
	}
}

func (c *conn) markSeen(in bridge.Incoming) {
	c.see(in.Chat.ID, in.Time)
}

func (c *conn) see(chat string, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.seen[chat]) {
		c.seen[chat] = at
		c.dirty = true
	}
}

// hold starts a run: it returns the markers that are safe to read from
// (a chat held in an earlier run that never caught up keeps its old
// time) and holds them all until their catch-up is read.
func (c *conn) hold() map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	for chat, at := range c.held {
		c.seen[chat] = at
	}
	c.held = map[string]time.Time{}
	since := map[string]time.Time{}
	for chat, at := range c.seen {
		c.held[chat] = at
		since[chat] = at
	}
	return since
}

// release stops holding the chats that match, or all for nil.
func (c *conn) release(match func(chat string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for chat := range c.held {
		if match == nil || match(chat) {
			delete(c.held, chat)
			c.dirty = true
		}
	}
}

// noteSent remembers a line say sent: its id, to know it in history, and
// its time, so history after it starts past it.
func (c *conn) noteSent(chat, id string, at time.Time) {
	if id != "" {
		c.mu.Lock()
		ids := append(c.sent[chat], id)
		if len(ids) > maxSent {
			ids = ids[len(ids)-maxSent:]
		}
		c.sent[chat] = ids
		c.dirty = true
		c.mu.Unlock()
	}
	c.see(chat, at)
}

func (c *conn) wasSent(chat, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.sent[chat], id)
}

// saveState saves when lines were last seen, if that changed: for a
// chat still held, the time it had when the run began. Chats quiet for
// longer than backfill reaches are forgotten.
func (c *conn) saveState(ctx context.Context) {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	for k, at := range c.seen {
		if time.Since(at) > backfillAge {
			delete(c.seen, k)
			delete(c.held, k)
		}
	}
	for k := range c.sent {
		if _, ok := c.seen[k]; !ok {
			delete(c.sent, k)
		}
	}
	seen := maps.Clone(c.seen)
	maps.Copy(seen, c.held)
	data, err := json.Marshal(state{Seen: seen, Sent: c.sent})
	c.dirty = false
	c.mu.Unlock()
	if err == nil && c.h.SaveState(ctx, data) != nil {
		c.mu.Lock()
		c.dirty = true
		c.mu.Unlock()
	}
}

// Send posts the text to a channel or nick, a PRIVMSG per line.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	s := c.session()
	if s == nil {
		return "", bridge.Rejected(errNotConnected)
	}
	id := "say:" + m.EventID
	lines := split(m.Text, maxLine)
	for i, line := range lines {
		got, at, err := s.privmsg(ctx, m.Chat, line)
		if err == nil && s.echo {
			c.noteSent(fold(m.Chat), got, at)
		}
		if err != nil {
			if i == 0 {
				return "", err
			}
			// Part of it went out: the outcome is not a refusal.
			return id, fmt.Errorf("irc: sent %d of %d lines: %v", i, len(lines), err)
		}
		if i == 0 && got != "" {
			id = got
		}
	}
	return id, nil
}

// React sends a reaction as a TAGMSG, on servers with message-tags. Lines
// have no ids without them, so there is nothing to react to.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	s := c.session()
	if s == nil {
		return "", bridge.Rejected(errNotConnected)
	}
	if !s.tags || strings.HasPrefix(r.Target, "h:") || strings.HasPrefix(r.Target, "say:") {
		return "", bridge.Rejected(errors.New("irc: the server does not carry reactions"))
	}
	tag := tagReact
	if r.Removed {
		tag = tagUnreact
	}
	if err := s.irc.SendWithTags(map[string]string{tag: r.Key, tagReply: r.Target}, "TAGMSG", r.Chat); err != nil {
		return "", bridge.Rejected(err)
	}
	return "", nil
}

// privmsg sends one line and returns its id and server-time, if the
// server echoed it.
func (s *session) privmsg(ctx context.Context, target, line string) (string, time.Time, error) {
	if !s.echo {
		if err := s.irc.Privmsg(target, line); err != nil {
			// Not handed to the socket at all.
			return "", time.Time{}, bridge.Rejected(err)
		}
		return "", time.Time{}, nil
	}
	b, err := s.request(ctx, "PRIVMSG", target, line)
	if errors.Is(err, ircevent.ClientDisconnected) || errors.Is(err, ircevent.CapabilityNotNegotiated) {
		return "", time.Time{}, bridge.Rejected(err)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if err := failure(b); err != nil {
		return "", time.Time{}, bridge.Rejected(err)
	}
	var id string
	var at time.Time
	walk(b, func(m ircmsg.Message) {
		if m.Command == "PRIVMSG" && id == "" {
			id, at = messageID(m), serverTime(m)
		}
	})
	return id, at, nil
}

// request sends a labeled command and waits for the server's reply.
func (s *session) request(ctx context.Context, command string, params ...string) (*ircevent.Batch, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	done := make(chan *ircevent.Batch, 1)
	if err := s.irc.SendWithLabel(func(b *ircevent.Batch) { done <- b }, nil, command, params...); err != nil {
		return nil, err
	}
	select {
	case b := <-done:
		if b == nil {
			return nil, errNoReply
		}
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// walk calls fn with each line of a reply, in order.
func walk(b *ircevent.Batch, fn func(ircmsg.Message)) {
	if b == nil {
		return
	}
	if b.Command != "BATCH" {
		fn(b.Message)
	}
	for _, it := range b.Items {
		walk(it, fn)
	}
}

// failure returns the error in a reply: a FAIL or a 4xx/5xx numeric.
func failure(b *ircevent.Batch) error {
	var err error
	walk(b, func(m ircmsg.Message) {
		if err != nil {
			return
		}
		c := m.Command
		numericErr := len(c) == 3 && (c[0] == '4' || c[0] == '5') && c[1] >= '0' && c[1] <= '9' && c[2] >= '0' && c[2] <= '9'
		if c == "FAIL" || numericErr {
			err = fmt.Errorf("irc: %s %s", c, strings.Join(m.Params, " "))
		}
	})
	return err
}

// item is a line waiting to be received.
type item struct {
	msg      ircmsg.Message
	backfill bool
	// caughtUp, instead of a line, says a chat's catch-up is all queued.
	caughtUp string
}

// queue is an unbounded queue, so the read loop never waits on the host.
type queue struct {
	mu    sync.Mutex
	items []item
	wake  chan struct{}
}

func (q *queue) push(it item) {
	q.mu.Lock()
	q.items = append(q.items, it)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) take() []item {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}
