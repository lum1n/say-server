// Package say syncs a node with the person's home say
// (docs/adr/0015-sync.md). It is a bridge in account mode whose network is
// another say: each home room the person is in becomes a portal, the
// other people, agents and ghosts there are ghosts here, and the
// person's messages from the web, the TUI or another device come in as
// the node's local user.
//
// It is a client of home's REST and WebSocket API (protocol/) with a
// device token from POST /api/v1/devices. Sends carry the idempotency key
// sync:<bridge id>:<event id>, so the network is Idempotent: a message
// written offline goes out whenever home is back.
package say

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/client"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:  "say",
		Title: "say (home)",
		Modes: []string{store.ModeAccount},
		Fields: []bridge.Field{
			{Key: "url", Label: "Home address", Kind: "text", Required: true, Placeholder: "https://chat.example.com",
				Help: "The say server this device syncs with."},
			{Key: "username", Label: "Username", Kind: "text", Required: true,
				Help: "Your username there. say asks for the password once and keeps a token for this device instead."},
			{Key: "device", Label: "Device name", Kind: "text", Placeholder: "this computer's name",
				Help: "How this device's token is named on home."},
		},
		Capabilities: []connector.Capability{connector.CapEdits, connector.CapReactions, connector.CapAttachments},
		Idempotent:   true,
		Open:         open,
		LogOut:       logOut,
	})
}

// Timings and limits; tests change them.
var (
	// BackfillAge and BackfillMax bound what a room newly followed
	// fetches: events from the last BackfillAge, at most BackfillMax.
	BackfillAge = 30 * 24 * time.Hour
	BackfillMax = 500
	// SaveEvery is how often changed cursors are saved.
	SaveEvery = 2 * time.Second
	// StallFor is how long a room waits before it is followed again
	// after a message that could not be taken in yet.
	StallFor = 2 * time.Second
)

// config is a bridge's settings.
type config struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Device   string `json:"device"`
}

// state is what a connection keeps between runs.
type state struct {
	// Account is the home and username the rest is for; a change of
	// either pairs afresh.
	Account string `json:"account"`
	// Token is the device's token on home. The password is not kept.
	Token string `json:"token,omitempty"`
	// Cursors are the last seq taken in per home room. A room without one
	// is backfilled.
	Cursors map[string]int64 `json:"cursors,omitempty"`
}

// conn is a running sync with home.
type conn struct {
	id   string // the bridge's
	cfg  config
	h    bridge.Host
	http *http.Client

	mu    sync.Mutex
	st    state
	dirty bool           // st changed since it was saved
	cl    *client.Client // while Run is connected
}

func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, fmt.Errorf("say: bad config: %w", err)
	}
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if cfg.URL == "" {
		return nil, errors.New("say: home's address is required")
	}
	if !strings.Contains(cfg.URL, "://") {
		cfg.URL = "https://" + cfg.URL
	}
	cfg.Username = strings.ToLower(strings.TrimSpace(cfg.Username))
	if cfg.Username == "" {
		return nil, errors.New("say: the username is required")
	}
	if cfg.Device = strings.TrimSpace(cfg.Device); cfg.Device == "" {
		cfg.Device, _ = os.Hostname()
		if cfg.Device == "" {
			cfg.Device = "say"
		}
	}
	c := &conn{id: b.ID, cfg: cfg, h: h, http: &http.Client{}}
	if len(b.State) > 0 {
		json.Unmarshal(b.State, &c.st)
	}
	if c.st.Account != c.account() {
		c.st = state{Account: c.account()}
	}
	if c.st.Cursors == nil {
		c.st.Cursors = map[string]int64{}
	}
	return c, nil
}

// logOut revokes the device's token on home, which unpairs it.
func logOut(ctx context.Context, b bridge.Info) error {
	bc, err := open(b, nil)
	if err != nil {
		return nil // never ran, so never paired
	}
	c := bc.(*conn)
	if c.st.Token == "" {
		return nil
	}
	err = c.client(c.st.Token).Logout(ctx)
	if status(err) == http.StatusUnauthorized {
		return nil // revoked already
	}
	return err
}

func (c *conn) account() string { return c.cfg.URL + " " + c.cfg.Username }

func (c *conn) client(token string) *client.Client {
	return &client.Client{BaseURL: c.cfg.URL, Token: token, HTTP: c.http}
}

// Run pairs if need be, follows every home room the person is in and
// takes in what they log, until ctx ends.
func (c *conn) Run(ctx context.Context) error {
	defer c.save(context.WithoutCancel(ctx))
	token, err := c.pair(ctx)
	if err != nil {
		return err
	}
	cl := c.client(token)
	me, err := cl.Me(ctx)
	if status(err) == http.StatusUnauthorized {
		// Revoked on home: the next run asks for the password again.
		c.update(func(st *state) { st.Token = "" })
		return errors.New("home no longer accepts this device's token; it asks for the password to pair again")
	}
	if err != nil {
		return err
	}
	rt, err := cl.Realtime(ctx)
	if err != nil {
		return err
	}
	defer rt.Close()
	c.mu.Lock()
	c.cl = cl
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.cl = nil
		c.mu.Unlock()
	}()
	s := &session{
		c: c, cl: cl, rt: rt, me: me,
		chats:    map[string]bridge.RemoteChat{},
		followed: map[string]bool{},
		names:    map[string]string{},
		stalled:  map[string]bool{},
		unstall:  make(chan string),
	}
	return s.run(ctx)
}

// pair returns the device's token, asking the owner for the password and
// getting a token from home when there is none.
func (c *conn) pair(ctx context.Context) (string, error) {
	c.mu.Lock()
	token := c.st.Token
	c.mu.Unlock()
	if token != "" {
		return token, nil
	}
	pw, err := c.h.Ask(ctx, bridge.Prompt{Kind: "password", Text: fmt.Sprintf("Password of %s on %s", c.cfg.Username, c.cfg.URL)})
	if err != nil {
		return "", err
	}
	d, err := c.client("").Pair(ctx, c.cfg.Username, pw, c.cfg.Device)
	if err != nil {
		return "", fmt.Errorf("pairing with home: %w", err)
	}
	c.update(func(st *state) { st.Token = d.Token })
	return d.Token, c.save(ctx)
}

// update changes the state; save keeps it.
func (c *conn) update(f func(*state)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.st)
	c.dirty = true
}

// save keeps the state if it changed.
func (c *conn) save(ctx context.Context) error {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	data, err := json.Marshal(c.st)
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

func (c *conn) cursor(roomID string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq, ok := c.st.Cursors[roomID]
	return seq, ok
}

func (c *conn) connected() (*client.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cl == nil {
		return nil, errors.New("say: not connected to home")
	}
	return c.cl, nil
}

// Send posts the owner's message to home, its files uploaded first. A
// repeat of the same message returns the one home already has.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	cl, err := c.connected()
	if err != nil {
		return "", err
	}
	parts, err := upload(ctx, cl, m)
	if err != nil {
		return "", err
	}
	e, err := cl.Send(ctx, m.Chat, parts, "sync:"+c.id+":"+m.EventID)
	if err != nil {
		return "", classify(err)
	}
	return e.ID, nil
}

// upload converts a message's parts for home: each attachment becomes
// the file uploaded to home, in its place. A file home won't take, or a
// home too old to take files, leaves its placeholder instead.
func upload(ctx context.Context, cl *client.Client, m bridge.Outgoing) ([]client.Part, error) {
	var out []client.Part
	files := m.Media
	for _, p := range m.Parts {
		if p.Kind != core.PartAttachment {
			out = append(out, client.Part{Kind: string(p.Kind), Data: p.Data})
			continue
		}
		var a core.Attachment
		if json.Unmarshal(p.Data, &a) != nil || a.Media == "" || len(files) == 0 {
			continue
		}
		// Media are the attachment parts' files, in order.
		f := files[0]
		files = files[1:]
		got, err := uploadFile(ctx, cl, m.Chat, f)
		switch s := status(err); {
		case err == nil:
			out = append(out, client.Part{Kind: string(core.PartAttachment), Data: got.Part().Data})
		case s == http.StatusNotFound && client.Code(err) == "", s == http.StatusRequestEntityTooLarge, s == http.StatusUnsupportedMediaType,
			errors.Is(err, store.ErrNotFound):
			// A 404 without a code is a route home doesn't have: it
			// predates files. Retrying, or a file gone from here, would
			// never get further.
			out = append(out, client.TextPart(core.Attachment{Mime: f.Mime, Name: f.Name}.Placeholder()))
		default:
			return nil, classify(err)
		}
	}
	return out, nil
}

func uploadFile(ctx context.Context, cl *client.Client, roomID string, f bridge.Media) (core.Attachment, error) {
	r, err := f.Open(ctx)
	if err != nil {
		return core.Attachment{}, err
	}
	defer r.Close()
	return cl.Upload(ctx, roomID, r, f.Mime, f.Name)
}

// React adds or takes back the owner's reaction on home, where the
// message's remote id is its event id.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	cl, err := c.connected()
	if err != nil {
		return "", err
	}
	_, err = cl.React(ctx, r.Chat, r.Target, r.Key, r.Removed)
	return "", classify(err)
}

// Edit changes the owner's message on home. Its files stay as home has
// them: an edit changes text, and can only drop files.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	cl, err := c.connected()
	if err != nil {
		return err
	}
	var theirs []client.Part
	if slices.ContainsFunc(m.Parts, func(p core.Part) bool { return p.Kind == core.PartAttachment }) {
		e, err := cl.Event(ctx, m.Chat, remoteID)
		if err != nil {
			return classify(err)
		}
		theirs = e.Parts
	}
	_, err = cl.Edit(ctx, m.Chat, remoteID, toHome(m.Parts, theirs))
	return classify(err)
}

// toHome converts an edit's parts for home, its attachments matched to
// those of the message on home by name and size: the ids differ.
func toHome(in []core.Part, theirs []client.Part) []client.Part {
	var out []client.Part
	used := map[int]bool{}
	for _, p := range in {
		if p.Kind != core.PartAttachment {
			out = append(out, client.Part{Kind: string(p.Kind), Data: p.Data})
			continue
		}
		var a core.Attachment
		json.Unmarshal(p.Data, &a)
		for k, t := range theirs {
			var h core.Attachment
			if t.Kind == string(core.PartAttachment) && !used[k] && json.Unmarshal(t.Data, &h) == nil && h.Name == a.Name && h.Size == a.Size {
				used[k] = true
				h.URL = ""
				out = append(out, client.Part{Kind: t.Kind, Data: h.Part().Data})
				break
			}
		}
	}
	return out
}

// fromHome converts parts from home: its files are read from home as
// the message is taken in.
func (s *session) fromHome(roomID string, in []client.Part) ([]core.Part, []bridge.Media) {
	var out []core.Part
	var files []bridge.Media
	for _, p := range in {
		if p.Kind != string(core.PartAttachment) {
			out = append(out, core.Part{Kind: core.PartKind(p.Kind), Data: p.Data})
			continue
		}
		var a core.Attachment
		if json.Unmarshal(p.Data, &a) != nil || a.Media == "" {
			continue
		}
		cl := s.cl
		files = append(files, bridge.Media{
			Mime: a.Mime, Name: a.Name, Size: a.Size, Width: a.Width, Height: a.Height,
			Open: func(ctx context.Context) (io.ReadCloser, error) { return cl.OpenMedia(ctx, roomID, a.Media) },
		})
	}
	return out, files
}

// classify marks the errors home gave for certain as rejections. Others,
// such as a lost connection, a revoked token or a busy server, leave the
// send to be tried again.
func classify(err error) error {
	switch s := status(err); {
	case err == nil:
		return nil
	case s >= 400 && s < 500 && s != http.StatusUnauthorized && s != http.StatusRequestTimeout && s != http.StatusTooManyRequests:
		return bridge.Rejected(err)
	}
	return err
}

// status is the HTTP status of an error answer from home, or 0.
func status(err error) int {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// session is one connection to home.
type session struct {
	c  *conn
	cl *client.Client
	rt *client.Realtime
	me client.Principal

	chats    map[string]bridge.RemoteChat // by home room id
	followed map[string]bool
	names    map[string]string // principal id → display name
	// stalled rooms wait to be followed again, from their cursor, after
	// a message home sent that could not be taken in yet.
	stalled map[string]bool
	unstall chan string
}

func (s *session) run(ctx context.Context) error {
	if err := s.syncRooms(ctx); err != nil {
		return err
	}
	s.c.h.Connected(ctx)
	save := time.NewTicker(SaveEvery)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-save.C:
			if err := s.c.save(ctx); err != nil {
				return err
			}
		case id := <-s.unstall:
			delete(s.stalled, id)
			if s.followed[id] {
				cursor, _ := s.c.cursor(id)
				s.rt.Subscribe(id, cursor)
			}
		case p, ok := <-s.rt.Pushes():
			if !ok {
				return errors.New("the connection to home ended")
			}
			var err error
			switch p.Type {
			case "rooms":
				err = s.syncRooms(ctx)
			case "event":
				err = s.take(ctx, *p.Event)
			}
			if err != nil {
				return err
			}
		}
	}
}

// syncRooms follows the rooms the person is in on home, from their
// cursors or, for rooms new to the device, from the start of their
// backfill, and stops following the rooms they left.
func (s *session) syncRooms(ctx context.Context) error {
	rooms, err := s.cl.Rooms(ctx)
	if err != nil {
		return err
	}
	in := map[string]bool{}
	for _, r := range rooms {
		in[r.ID] = true
		s.chats[r.ID] = bridge.RemoteChat{ID: r.ID, Name: r.Name, Direct: r.Kind == string(core.RoomDM)}
		if s.followed[r.ID] {
			continue
		}
		cursor, ok := s.c.cursor(r.ID)
		if !ok {
			if cursor, err = s.backfill(ctx, r.ID); err != nil {
				return err
			}
			s.c.update(func(st *state) { st.Cursors[r.ID] = cursor })
		}
		s.followed[r.ID] = true
		s.rt.Subscribe(r.ID, cursor)
	}
	for id := range s.followed {
		if !in[id] {
			s.rt.Unsubscribe(id)
			delete(s.followed, id)
			delete(s.chats, id)
			s.c.update(func(st *state) { delete(st.Cursors, id) })
		}
	}
	return nil
}

// backfill returns the cursor a room new to the device is followed from:
// before its events of the last BackfillAge, at most BackfillMax of them.
// Older history stays on home.
func (s *session) backfill(ctx context.Context, roomID string) (int64, error) {
	page, err := s.cl.Events(ctx, roomID, client.EventsQuery{Limit: BackfillMax})
	if err != nil || len(page) == 0 {
		return 0, err
	}
	since := time.Now().Add(-BackfillAge)
	cursor := page[len(page)-1].Seq
	for i := len(page) - 1; i >= 0 && !page[i].CreatedAt.Before(since); i-- {
		cursor = page[i].Seq - 1
	}
	return cursor, nil
}

// take passes an event of a followed room to the bridge and moves the
// room's cursor past it.
func (s *session) take(ctx context.Context, e client.Event) error {
	cursor, _ := s.c.cursor(e.RoomID)
	if !s.followed[e.RoomID] || s.stalled[e.RoomID] || e.Seq <= cursor {
		return nil
	}
	if in, ok := s.incoming(ctx, e); ok {
		err := s.c.h.Receive(ctx, in)
		if errors.Is(err, bridge.ErrPending) {
			s.stall(ctx, e.RoomID)
			return nil
		}
		if err != nil {
			return err
		}
	}
	s.c.update(func(st *state) { st.Cursors[e.RoomID] = e.Seq })
	return nil
}

// stall stops following a room for StallFor, to read it again from its
// cursor then.
func (s *session) stall(ctx context.Context, roomID string) {
	s.stalled[roomID] = true
	s.rt.Unsubscribe(roomID)
	time.AfterFunc(StallFor, func() {
		select {
		case s.unstall <- roomID:
		case <-ctx.Done():
		}
	})
}

// incoming converts a home event to a message for the bridge: a final
// message, the end of a stream, an edit, or a reaction. Typing, streaming
// frames and the rest are not synced.
func (s *session) incoming(ctx context.Context, e client.Event) (bridge.Incoming, bool) {
	author := func() bridge.Author {
		return bridge.Author{ID: e.SenderID, Name: s.name(ctx, e.SenderID), Self: e.SenderID == s.me.ID}
	}
	if e.Type == string(core.EventReaction) {
		if e.Reaction == nil || e.Reacts == "" {
			return bridge.Incoming{}, false
		}
		// Reactions have no remote id of their own: their message names them.
		return bridge.Incoming{
			Chat:     s.chats[e.RoomID],
			Author:   author(),
			Time:     e.CreatedAt,
			Reaction: &bridge.IncomingReaction{Target: e.Reacts, Key: e.Reaction.Key, Removed: e.Reaction.Removed},
		}, true
	}
	parts, files := s.fromHome(e.RoomID, e.Parts)
	if len(parts) == 0 && len(files) == 0 {
		return bridge.Incoming{}, false
	}
	in := bridge.Incoming{
		Chat:   s.chats[e.RoomID],
		ID:     e.ID,
		Author: author(),
		Parts:  parts,
		Media:  files,
		Time:   e.CreatedAt,
		// Home's ids are the remote ids, so a reply's target is found.
		ReplyTo: e.ReplyTo,
	}
	switch {
	case e.Type == string(core.EventMessage) && e.Stream != string(core.StreamStreaming):
	case e.Type == string(core.EventEdit) && e.Stream == string(core.StreamFinal):
		// The message is complete now, and new to the device.
		in.ID = e.Edits
	case e.Type == string(core.EventEdit):
		// An edit changes the text; the files came with the message.
		in.ID, in.Edit, in.Media = e.Edits, true, nil
		if len(parts) == 0 {
			return bridge.Incoming{}, false
		}
	default:
		return bridge.Incoming{}, false
	}
	return in, true
}

// name returns a home principal's display name.
func (s *session) name(ctx context.Context, id string) string {
	if n, ok := s.names[id]; ok {
		return n
	}
	if ps, err := s.cl.Principals(ctx); err == nil {
		for _, p := range ps {
			s.names[p.ID] = p.DisplayName
		}
	}
	if n, ok := s.names[id]; ok {
		return n
	}
	s.names[id] = "someone"
	return "someone"
}
