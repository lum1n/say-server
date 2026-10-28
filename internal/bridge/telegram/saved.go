package telegram

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
)

// maxPeers bounds the peer cache kept in the state. Peers seen longest
// ago are dropped first; a dropped one is found again through the dialogs.
const maxPeers = 2000

// maxSent bounds the messages kept in the state that say sent after the
// first of one of its messages.
const maxSent = 500

// state is what a connection keeps between runs, saved as the bridge
// state. It holds the MTProto session, the update sequence positions
// (so updates missed while away are fetched on reconnect) and the access
// hashes needed to address users and channels.
type state struct {
	// Phone is the number the session was made for: a new number in the
	// config means a new login.
	Phone string `json:"phone,omitempty"`
	// Session is gotd's session (keys and data centre), base64 in JSON.
	Session []byte `json:"session,omitempty"`
	// UserID is the logged-in account; updates state belongs to it.
	UserID   int64          `json:"user_id,omitempty"`
	Updates  *updates.State `json:"updates,omitempty"`
	Channels map[int64]int  `json:"channels,omitempty"` // pts per channel
	// Peers are users, chats and channels by chat id ("user:42").
	Peers map[string]*peer `json:"peers,omitempty"`
	// Sent are the messages after the first that say sent for one of its
	// messages, such as its second file, as "<chat id>/<message id>",
	// newest last: the core knows the first only, so their echoes are
	// dropped here.
	Sent []string `json:"sent,omitempty"`
}

// peer is a cached user, chat or channel.
type peer struct {
	Hash int64  `json:"h,omitempty"` // access hash; none for basic chats
	Name string `json:"n,omitempty"`
	Seen int64  `json:"t,omitempty"` // unix time, for eviction
}

// saved is the state of a connection, kept in memory and written to the
// bridge state now and then: most changes are update positions, which
// change with every message, and each save is a database write.
//
// It is gotd's session storage, update state storage and access hash
// store all in one.
type saved struct {
	save func(context.Context, json.RawMessage) error
	now  func() time.Time

	mu    sync.Mutex
	s     state
	dirty bool
	kick  chan struct{} // asks the save loop to save now
}

// newSaved restores the state last saved, for the account on phone.
func newSaved(raw json.RawMessage, phone string, save func(context.Context, json.RawMessage) error) *saved {
	var s state
	if len(raw) > 0 {
		// A state that does not parse only costs a new login.
		json.Unmarshal(raw, &s)
	}
	if s.Phone != phone {
		s = state{Phone: phone}
	}
	if s.Channels == nil {
		s.Channels = map[int64]int{}
	}
	if s.Peers == nil {
		s.Peers = map[string]*peer{}
	}
	return &saved{save: save, now: time.Now, s: s, kick: make(chan struct{}, 1)}
}

// changed marks the state for the next save; soon asks for it now.
func (st *saved) changed(soon bool) {
	st.dirty = true
	if soon {
		select {
		case st.kick <- struct{}{}:
		default:
		}
	}
}

// flush saves the state if it changed.
func (st *saved) flush(ctx context.Context) error {
	st.mu.Lock()
	if !st.dirty {
		st.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(st.s)
	st.dirty = false
	st.mu.Unlock()
	if err != nil {
		return err
	}
	if err := st.save(ctx, raw); err != nil {
		st.mu.Lock()
		st.dirty = true
		st.mu.Unlock()
		return err
	}
	return nil
}

// saveLoop saves the state every interval, or at once when asked, until
// ctx ends.
func (st *saved) saveLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-st.kick:
		}
		st.flush(ctx)
	}
}

// loggedIn records the account the session is logged in to. Another
// account than before starts its update state and peers afresh.
func (st *saved) loggedIn(self *tg.User) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.UserID != self.ID {
		st.s.UserID = self.ID
		st.s.Updates = nil
		st.s.Channels = map[int64]int{}
		st.s.Peers = map[string]*peer{}
	}
	st.rememberUser(self)
	st.changed(true)
}

// LoadSession implements session.Storage.
func (st *saved) LoadSession(context.Context) ([]byte, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s.Session) == 0 {
		return nil, session.ErrNotFound
	}
	return append([]byte(nil), st.s.Session...), nil
}

// StoreSession implements session.Storage. A session is worth saving at
// once: losing it means logging in again.
func (st *saved) StoreSession(_ context.Context, data []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Session = append([]byte(nil), data...)
	st.changed(true)
	return nil
}

// The methods below implement updates.StateStorage. There is one account
// per state, so the user id only guards against a stale one.

func (st *saved) GetState(_ context.Context, userID int64) (updates.State, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Updates == nil || st.s.UserID != userID {
		return updates.State{}, false, nil
	}
	return *st.s.Updates, true, nil
}

func (st *saved) SetState(_ context.Context, userID int64, s updates.State) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Updates = &s
	st.changed(false)
	return nil
}

// setField changes one field of the update state, if there is one.
func (st *saved) setField(f func(*updates.State)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Updates == nil {
		// gotd sets the whole state first; a lone field means nothing.
		return nil
	}
	f(st.s.Updates)
	st.changed(false)
	return nil
}

func (st *saved) SetPts(_ context.Context, _ int64, pts int) error {
	return st.setField(func(s *updates.State) { s.Pts = pts })
}

func (st *saved) SetQts(_ context.Context, _ int64, qts int) error {
	return st.setField(func(s *updates.State) { s.Qts = qts })
}

func (st *saved) SetDate(_ context.Context, _ int64, date int) error {
	return st.setField(func(s *updates.State) { s.Date = date })
}

func (st *saved) SetSeq(_ context.Context, _ int64, seq int) error {
	return st.setField(func(s *updates.State) { s.Seq = seq })
}

func (st *saved) SetDateSeq(_ context.Context, _ int64, date, seq int) error {
	return st.setField(func(s *updates.State) { s.Date, s.Seq = date, seq })
}

func (st *saved) GetChannelPts(_ context.Context, _, channelID int64) (int, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	pts, ok := st.s.Channels[channelID]
	return pts, ok, nil
}

func (st *saved) SetChannelPts(_ context.Context, _, channelID int64, pts int) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Channels[channelID] = pts
	st.changed(false)
	return nil
}

func (st *saved) ForEachChannels(ctx context.Context, _ int64, f func(context.Context, int64, int) error) error {
	// A copy, so f may call back in.
	st.mu.Lock()
	chans := make(map[int64]int, len(st.s.Channels))
	for id, pts := range st.s.Channels {
		chans[id] = pts
	}
	st.mu.Unlock()
	for id, pts := range chans {
		if err := f(ctx, id, pts); err != nil {
			return err
		}
	}
	return nil
}

// The methods below implement updates.ChannelAccessHasher and
// updates.UserAccessHasher on the peer cache.

func (st *saved) SetChannelAccessHash(_ context.Context, _, channelID, hash int64) error {
	st.setHash(chatID("channel", channelID), hash)
	return nil
}

func (st *saved) GetChannelAccessHash(_ context.Context, _, channelID int64) (int64, bool, error) {
	h, ok := st.hash(chatID("channel", channelID))
	return h, ok, nil
}

func (st *saved) SetUserAccessHash(_ context.Context, _, userID, hash int64) error {
	st.setHash(chatID("user", userID), hash)
	return nil
}

func (st *saved) GetUserAccessHash(_ context.Context, _, userID int64) (int64, bool, error) {
	h, ok := st.hash(chatID("user", userID))
	return h, ok, nil
}

// hash returns the access hash of a user or channel.
func (st *saved) hash(id string) (int64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.s.Peers[id]
	if !ok || p.Hash == 0 {
		return 0, false
	}
	return p.Hash, true
}

func (st *saved) setHash(id string, hash int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.put(id, hash, "")
}

// name returns the cached name of a peer, or "".
func (st *saved) name(id string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if p, ok := st.s.Peers[id]; ok {
		return p.Name
	}
	return ""
}

// self returns the logged-in account's id.
func (st *saved) self() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.UserID
}

// remember caches the names and access hashes of users and chats that
// came with a response or update.
func (st *saved) remember(users []tg.UserClass, chats []tg.ChatClass) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, u := range users {
		if u, ok := u.(*tg.User); ok {
			st.rememberUser(u)
		}
	}
	for _, c := range chats {
		switch c := c.(type) {
		case *tg.Chat:
			st.put(chatID("chat", c.ID), 0, c.Title)
		case *tg.ChatForbidden:
			st.put(chatID("chat", c.ID), 0, c.Title)
		case *tg.Channel:
			hash := c.AccessHash
			if c.Min {
				// A min channel's hash cannot be used to address it.
				hash = 0
			}
			st.put(chatID("channel", c.ID), hash, c.Title)
		case *tg.ChannelForbidden:
			st.put(chatID("channel", c.ID), c.AccessHash, c.Title)
		}
	}
}

// rememberUser caches a user; st.mu is held.
func (st *saved) rememberUser(u *tg.User) {
	hash := u.AccessHash
	if u.Min {
		hash = 0
	}
	st.put(chatID("user", u.ID), hash, userName(u))
}

// put sets what is known of a peer, keeping what is not given; st.mu is
// held. A change is saved with the next batch.
func (st *saved) put(id string, hash int64, name string) {
	p, ok := st.s.Peers[id]
	if !ok {
		if len(st.s.Peers) >= maxPeers {
			st.evict()
		}
		p = &peer{}
		st.s.Peers[id] = p
	}
	p.Seen = st.now().Unix()
	if hash != 0 && hash != p.Hash {
		p.Hash = hash
		st.changed(false)
	}
	if name != "" && name != p.Name {
		p.Name = name
		st.changed(false)
	}
}

// evict drops the peer seen longest ago; st.mu is held.
func (st *saved) evict() {
	var oldest string
	var at int64
	for id, p := range st.s.Peers {
		if oldest == "" || p.Seen < at {
			oldest, at = id, p.Seen
		}
	}
	delete(st.s.Peers, oldest)
}

// sentToo notes messages sent for a message after its first.
func (st *saved) sentToo(chat string, ids []int) {
	if len(ids) == 0 {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, id := range ids {
		st.s.Sent = append(st.s.Sent, chat+"/"+strconv.Itoa(id))
	}
	if n := len(st.s.Sent) - maxSent; n > 0 {
		st.s.Sent = append([]string(nil), st.s.Sent[n:]...)
	}
	st.changed(true)
}

// wasSentToo reports whether a message was sent after the first of one of
// say's.
func (st *saved) wasSentToo(chat, id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return slices.Contains(st.s.Sent, chat+"/"+id)
}

// chatID is a say chat id: "user:42", "chat:7", "channel:9".
func chatID(kind string, id int64) string {
	return kind + ":" + strconv.FormatInt(id, 10)
}
