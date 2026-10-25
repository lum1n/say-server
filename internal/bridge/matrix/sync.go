package matrix

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/bridge"
)

// Limits of the backfill on the first sync.
const (
	backfillAge   = 30 * 24 * time.Hour
	backfillChats = 50
)

// syncWait is how long a sync waits on the homeserver for news.
var syncWait = 30 * time.Second

// Filters of /sync. Presence, typing and receipts are left out; members
// are lazy-loaded, so a big room brings only the members who wrote. The
// first sync takes the last 20 events of each room, as backfill; later
// ones take more so a busy room rarely leaves a gap.
var (
	firstFilter = filter(20)
	nextFilter  = filter(100)
)

func filter(limit int) string {
	f := map[string]any{
		"presence":     map[string]any{"not_types": []string{"*"}},
		"account_data": map[string]any{"types": []string{"m.direct"}},
		"room": map[string]any{
			"timeline":     map[string]any{"limit": limit, "lazy_load_members": true},
			"state":        map[string]any{"lazy_load_members": true},
			"ephemeral":    map[string]any{"not_types": []string{"*"}},
			"account_data": map[string]any{"not_types": []string{"*"}},
		},
	}
	data, _ := json.Marshal(f)
	return string(data)
}

type syncResponse struct {
	NextBatch   string `json:"next_batch"`
	AccountData struct {
		Events []event `json:"events"`
	} `json:"account_data"`
	Rooms struct {
		Join map[string]joinedRoom `json:"join"`
	} `json:"rooms"`
}

type joinedRoom struct {
	Summary struct {
		Heroes []string `json:"m.heroes"`
		Joined *int     `json:"m.joined_member_count"`
	} `json:"summary"`
	State struct {
		Events []event `json:"events"`
	} `json:"state"`
	Timeline struct {
		Events  []event `json:"events"`
		Limited bool    `json:"limited"`
	} `json:"timeline"`
}

type event struct {
	Type     string          `json:"type"`
	ID       string          `json:"event_id"`
	Sender   string          `json:"sender"`
	TS       int64           `json:"origin_server_ts"`
	StateKey *string         `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

func (e event) time() time.Time { return time.UnixMilli(e.TS) }

// room is what a run knows of a room, for its name and its members' names.
type room struct {
	name, alias string
	heroes      []string
	// joined is the number of joined members; 0 is unknown.
	joined int
	// members are display names by user id; "" means none.
	members map[string]string
	// loaded says the room's name state is known: from the first sync, or
	// looked up.
	loaded bool
}

// sync asks the homeserver for what happened since since. Without wait it
// answers at once.
func (c *conn) sync(ctx context.Context, since string, wait bool) (*syncResponse, error) {
	q := url.Values{}
	q.Set("filter", nextFilter)
	if since == "" {
		q.Set("filter", firstFilter)
	} else {
		q.Set("since", since)
	}
	timeout := time.Duration(0)
	if wait {
		timeout = syncWait
	}
	q.Set("timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	sctx, cancel := context.WithTimeout(ctx, timeout+requestTimeout)
	defer cancel()
	var resp syncResponse
	if err := c.do(sctx, http.MethodGet, path("sync"), q, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// loadDirect reads which rooms the account marks as direct chats.
func (c *conn) loadDirect(ctx context.Context) error {
	var direct map[string][]string
	err := c.do(ctx, http.MethodGet, path("user", c.self(), "account_data", "m.direct"), nil, nil, &direct)
	if err != nil && !isAbsent(err) {
		return err
	}
	c.setDirect(direct)
	return nil
}

func (c *conn) setDirect(direct map[string][]string) {
	set := map[string]bool{}
	for _, rooms := range direct {
		for _, id := range rooms {
			set[id] = true
		}
	}
	c.mu.Lock()
	c.direct = set
	c.mu.Unlock()
}

// handle passes on what a sync brought. The first sync of an account is
// the backfill: only rooms with recent activity, the latest first, up to
// backfillChats of them.
func (c *conn) handle(ctx context.Context, resp *syncResponse, first bool) error {
	for _, e := range resp.AccountData.Events {
		if e.Type == "m.direct" {
			var direct map[string][]string
			json.Unmarshal(e.Content, &direct)
			c.setDirect(direct)
		}
	}
	ids := make([]string, 0, len(resp.Rooms.Join))
	last := map[string]int64{}
	for id, jr := range resp.Rooms.Join {
		for _, e := range jr.Timeline.Events {
			last[id] = max(last[id], e.TS)
		}
		ids = append(ids, id)
	}
	if first {
		cutoff := time.Now().Add(-backfillAge).UnixMilli()
		// The most recent first, to keep those within the cap.
		slices.SortFunc(ids, func(a, b string) int { return cmp.Compare(last[b], last[a]) })
		recent := ids[:0:0]
		for _, id := range ids {
			if last[id] >= cutoff && len(recent) < backfillChats {
				recent = append(recent, id)
			}
		}
		// All rooms' state is learned, so names are known later on.
		for id, jr := range resp.Rooms.Join {
			c.learn(id, jr, true)
		}
		ids = recent
	}
	for _, id := range ids {
		jr := resp.Rooms.Join[id]
		if !first {
			c.learn(id, jr, false)
		}
		for _, e := range jr.Timeline.Events {
			if e.StateKey != nil {
				c.learnState(id, e)
				continue
			}
			in, ok, err := c.incoming(ctx, id, e)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := c.h.Receive(ctx, in); err != nil {
				return err
			}
		}
	}
	return nil
}

// roomLocked returns the run's record of a room. c.mu is held.
func (c *conn) roomLocked(id string) *room {
	r := c.rooms[id]
	if r == nil {
		r = &room{members: map[string]string{}}
		c.rooms[id] = r
	}
	return r
}

// learn takes in a room's summary and state from a sync. The state of a
// first sync is complete, which makes the room loaded.
func (c *conn) learn(id string, jr joinedRoom, complete bool) {
	c.mu.Lock()
	r := c.roomLocked(id)
	if jr.Summary.Heroes != nil {
		r.heroes = jr.Summary.Heroes
	}
	if jr.Summary.Joined != nil {
		r.joined = *jr.Summary.Joined
	}
	if complete {
		r.loaded = true
	}
	c.mu.Unlock()
	for _, e := range jr.State.Events {
		c.learnState(id, e)
	}
	if complete {
		// A room left out of the backfill still has its latest name.
		for _, e := range jr.Timeline.Events {
			c.learnState(id, e)
		}
	}
}

// learnState takes in a state event: a room's name, alias or a member.
func (c *conn) learnState(id string, e event) {
	if e.StateKey == nil {
		return
	}
	var content struct {
		Name        string `json:"name"`
		Alias       string `json:"alias"`
		DisplayName string `json:"displayname"`
		Membership  string `json:"membership"`
	}
	json.Unmarshal(e.Content, &content)
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.roomLocked(id)
	switch e.Type {
	case "m.room.name":
		r.name = content.Name
	case "m.room.canonical_alias":
		r.alias = content.Alias
	case "m.room.member":
		r.members[*e.StateKey] = content.DisplayName
	}
}

// load looks up the name state of a room the run has not seen in full,
// as after resuming a sync. Only an unnamed room's members are listed,
// for its name and to tell whether it is a chat of two: a named room
// may be huge.
func (c *conn) load(ctx context.Context, id string) error {
	c.mu.Lock()
	loaded := c.roomLocked(id).loaded
	c.mu.Unlock()
	if loaded {
		return nil
	}
	var name struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, path("rooms", id, "state", "m.room.name"), nil, nil, &name); err != nil && !isAbsent(err) {
		return err
	}
	var alias struct {
		Alias string `json:"alias"`
	}
	if err := c.do(ctx, http.MethodGet, path("rooms", id, "state", "m.room.canonical_alias"), nil, nil, &alias); err != nil && !isAbsent(err) {
		return err
	}
	var members struct {
		Joined map[string]struct {
			DisplayName string `json:"display_name"`
		} `json:"joined"`
	}
	if name.Name == "" && alias.Alias == "" {
		if err := c.do(ctx, http.MethodGet, path("rooms", id, "joined_members"), nil, nil, &members); err != nil && !isAbsent(err) {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.roomLocked(id)
	if r.name == "" {
		r.name = name.Name
	}
	if r.alias == "" {
		r.alias = alias.Alias
	}
	if len(members.Joined) > 0 {
		r.joined = len(members.Joined)
		r.heroes = r.heroes[:0:0]
		for uid, m := range members.Joined {
			if _, ok := r.members[uid]; !ok {
				r.members[uid] = m.DisplayName
			}
			r.heroes = append(r.heroes, uid)
		}
		slices.Sort(r.heroes)
	}
	r.loaded = true
	return nil
}

// chat describes a room for say: its name from m.room.name, else its
// alias, else its members.
func (c *conn) chat(ctx context.Context, id string) (bridge.RemoteChat, error) {
	if err := c.load(ctx, id); err != nil {
		return bridge.RemoteChat{}, err
	}
	self := c.self()
	c.mu.Lock()
	r := c.roomLocked(id)
	ch := bridge.RemoteChat{ID: id, Name: r.name, Direct: c.direct[id] || r.joined == 2}
	if ch.Name == "" {
		ch.Name = r.alias
	}
	var heroes []string
	if ch.Name == "" {
		heroes = slices.Clone(r.heroes)
		if len(heroes) == 0 {
			for uid := range r.members {
				heroes = append(heroes, uid)
			}
			slices.Sort(heroes)
		}
	}
	c.mu.Unlock()
	var names []string
	for _, uid := range heroes {
		if uid == self || len(names) == 5 {
			continue
		}
		n, err := c.memberName(ctx, id, uid)
		if err != nil {
			return bridge.RemoteChat{}, err
		}
		names = append(names, n)
	}
	if ch.Name == "" {
		ch.Name = strings.Join(names, ", ")
	}
	return ch, nil
}

// memberName returns the display name of a user in a room, looking up
// their member event when the sync did not bring it, else their localpart.
func (c *conn) memberName(ctx context.Context, roomID, userID string) (string, error) {
	c.mu.Lock()
	n, ok := c.roomLocked(roomID).members[userID]
	c.mu.Unlock()
	if !ok {
		var m struct {
			DisplayName string `json:"displayname"`
		}
		err := c.do(ctx, http.MethodGet, path("rooms", roomID, "state", "m.room.member", userID), nil, nil, &m)
		if err != nil && !isAbsent(err) {
			return "", err
		}
		n = m.DisplayName
		c.mu.Lock()
		c.roomLocked(roomID).members[userID] = n
		c.mu.Unlock()
	}
	if n == "" {
		n = localpart(userID)
	}
	return n, nil
}

// localpart returns "ada" for "@ada:example.org".
func localpart(userID string) string {
	s := strings.TrimPrefix(userID, "@")
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}
