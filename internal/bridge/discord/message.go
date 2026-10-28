package discord

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
)

// message is a Discord message, in an event or in history.
type message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Type      int    `json:"type"`
	// Content is nil in an update that left the text alone.
	Content         *string `json:"content"`
	Timestamp       string  `json:"timestamp"`
	EditedTimestamp string  `json:"edited_timestamp"`
	Author          *user   `json:"author"`
	WebhookID       string  `json:"webhook_id"`
	ApplicationID   string  `json:"application_id"`
	Mentions        []user  `json:"mentions"`
	Attachments     []struct {
		Filename string `json:"filename"`
	} `json:"attachments"`
	Reference *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`

	update bool // came as MESSAGE_UPDATE
	// reaction, when set, makes this a reaction to message ID rather
	// than the message itself.
	reaction *reaction
}

// reaction is a MESSAGE_REACTION_ADD or _REMOVE event.
type reaction struct {
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	Member    *struct {
		User user `json:"user"`
	} `json:"member"`
	Emoji struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"emoji"`

	removed bool
}

// key is the reaction as say keys it: the emoji itself, or a server's
// own emoji by its name, as :name:.
func (r reaction) key() string {
	if r.Emoji.ID != "" {
		return ":" + strings.ToLower(r.Emoji.Name) + ":"
	}
	return r.Emoji.Name
}

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

func (u user) name() string { return cmp.Or(u.GlobalName, u.Username, u.ID) }

type channel struct {
	ID       string `json:"id"`
	Type     int    `json:"type"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

// Channel types of threads: in an announcement channel, public, private.
// A forum or media channel's posts are public threads.
var threadTypes = []int{10, 11, 12}

// parent returns the channel a thread is in, or "" for a channel that
// is not a thread (whose parent_id is its category).
func (ch channel) parent() string {
	if slices.Contains(threadTypes, ch.Type) {
		return ch.ParentID
	}
	return ""
}

// Message types that are something someone wrote: default, reply, and
// the answers to slash and context menu commands.
var shown = []int{0, 19, 20, 23}

// name caches channel names, for rooms and #mentions, and the channels
// threads are in, for webhooks.
func (c *conn) name(chs ...channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range chs {
		if ch.ID == "" {
			continue
		}
		if ch.Name != "" {
			c.names[ch.ID] = ch.Name
		}
		c.parents[ch.ID] = ch.parent()
	}
}

// receive hands a message of a linked channel to the core, if it is one
// to show.
func (c *conn) receive(ctx context.Context, m message) error {
	if m.ChannelID == "" || m.ID == "" || !c.isLinked(ctx, m.ChannelID) {
		return nil
	}
	if m.reaction != nil {
		return c.receiveReaction(ctx, *m.reaction)
	}
	if in, ok := c.incoming(m); ok {
		if err := c.h.Receive(ctx, in); err != nil {
			return err
		}
	}
	if !m.update {
		c.seen(m.ChannelID, m.ID)
	}
	return nil
}

// receiveReaction hands a reaction to the core. Discord gives reactions no
// id of their own; a removal names the message and the emoji.
func (c *conn) receiveReaction(ctx context.Context, r reaction) error {
	key := r.key()
	if key == "" || r.UserID == "" {
		return nil
	}
	c.mu.Lock()
	self := r.UserID == c.self
	chat := bridge.RemoteChat{ID: r.ChannelID, Name: cmp.Or(c.names[r.ChannelID], r.ChannelID)}
	c.mu.Unlock()
	name := r.UserID
	if r.Member != nil {
		name = r.Member.User.name()
	}
	return c.h.Receive(ctx, bridge.Incoming{
		Chat:     chat,
		Author:   bridge.Author{ID: r.UserID, Name: name, Self: self},
		Reaction: &bridge.IncomingReaction{Target: r.MessageID, Key: key, Removed: r.removed},
	})
}

// incoming converts a message; false means it is not shown.
func (c *conn) incoming(m message) (bridge.Incoming, bool) {
	if m.Author == nil {
		return bridge.Incoming{}, false
	}
	if m.update {
		// Updates without an edit time are link previews being added.
		if m.Content == nil || m.EditedTimestamp == "" {
			return bridge.Incoming{}, false
		}
	} else if !slices.Contains(shown, m.Type) {
		// Joins, pins, boosts, thread starts and the like.
		return bridge.Incoming{}, false
	}
	text := c.text(m)
	if text == "" {
		return bridge.Incoming{}, false
	}
	c.mu.Lock()
	self, app := c.self, c.appID
	ownHook := c.ownHook(m.WebhookID)
	chat := bridge.RemoteChat{ID: m.ChannelID, Name: cmp.Or(c.names[m.ChannelID], m.ChannelID)}
	c.mu.Unlock()
	// The bot's own posts and its webhook's are say's relays, which the
	// core drops.
	isSelf := m.Author.ID == self ||
		m.WebhookID != "" && (ownHook || app != "" && m.ApplicationID == app)
	in := bridge.Incoming{
		Chat:   chat,
		ID:     m.ID,
		Author: bridge.Author{ID: m.Author.ID, Name: m.Author.name(), Self: isSelf},
		Text:   text,
		Edit:   m.update,
	}
	if !m.update {
		in.Time, _ = time.Parse(time.RFC3339Nano, m.Timestamp)
		if m.Type == 19 && m.Reference != nil {
			in.ReplyTo = m.Reference.MessageID
		}
	}
	return in, true
}

// ownHook reports whether a webhook id is one of the bridge's. c.mu is
// held.
func (c *conn) ownHook(id string) bool {
	if id == "" {
		return false
	}
	for _, h := range c.hookIDs {
		if h == id {
			return true
		}
	}
	return false
}

var markup = regexp.MustCompile(`<(@!?|@&|#)(\d+)>|<a?:(\w+):\d+>`)

// text renders a message as plain text: mentions by name, custom emoji
// by name, and a line per attachment.
func (c *conn) text(m message) string {
	var s string
	if m.Content != nil {
		s = *m.Content
	}
	s = markup.ReplaceAllStringFunc(s, func(tok string) string {
		g := markup.FindStringSubmatch(tok)
		switch {
		case g[3] != "":
			return ":" + g[3] + ":"
		case g[1] == "#":
			c.mu.Lock()
			n, ok := c.names[g[2]]
			c.mu.Unlock()
			if ok {
				return "#" + n
			}
		case g[1] == "@" || g[1] == "@!":
			for _, u := range m.Mentions {
				if u.ID == g[2] {
					return "@" + u.name()
				}
			}
		}
		return tok
	})
	var b strings.Builder
	b.WriteString(strings.TrimSpace(s))
	for _, a := range m.Attachments {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[attachment: " + a.Filename + "]")
	}
	return b.String()
}

// Snowflakes are ids that grow with time: milliseconds since 2015 in
// the high bits.

const discordEpoch = 1420070400000

// snowflake returns the smallest id of a moment.
func snowflake(t time.Time) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-discordEpoch)<<22, 10)
}

func snowLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func sortByID(ms []message) {
	slices.SortFunc(ms, func(a, b message) int {
		switch {
		case snowLess(a.ID, b.ID):
			return -1
		case snowLess(b.ID, a.ID):
			return 1
		}
		return 0
	})
}
