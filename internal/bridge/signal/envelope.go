package signal

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/lum1n/say-server/internal/bridge"
)

// receiveParams are the params of a "receive" notification. signal-cli
// sends them flat for the subscription it makes on start, and wrapped in
// result for one made with subscribeReceive.
type receiveParams struct {
	Account  string    `json:"account"`
	Envelope *envelope `json:"envelope"`
	Result   *struct {
		Account  string    `json:"account"`
		Envelope *envelope `json:"envelope"`
	} `json:"result"`
}

// envelope is what signal-cli makes of one message from the Signal server.
type envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	Timestamp    int64        `json:"timestamp"`
	DataMessage  *dataMessage `json:"dataMessage"`
	EditMessage  *editMessage `json:"editMessage"`
	SyncMessage  *struct {
		SentMessage *sentMessage `json:"sentMessage"`
	} `json:"syncMessage"`
}

type dataMessage struct {
	Timestamp    int64           `json:"timestamp"`
	Message      string          `json:"message"`
	Reaction     json.RawMessage `json:"reaction"`
	RemoteDelete json.RawMessage `json:"remoteDelete"`
	Sticker      json.RawMessage `json:"sticker"`
	Quote        *quote          `json:"quote"`
	Mentions     []mention       `json:"mentions"`
	Attachments  []attachment    `json:"attachments"`
	GroupInfo    *struct {
		GroupID   string `json:"groupId"`
		GroupName string `json:"groupName"`
	} `json:"groupInfo"`
}

// editMessage replaces the text of the message sent at TargetSentTimestamp.
type editMessage struct {
	TargetSentTimestamp int64        `json:"targetSentTimestamp"`
	DataMessage         *dataMessage `json:"dataMessage"`
}

// sentMessage is a message the account's user sent from another device:
// the data message's fields inline, or an edit.
type sentMessage struct {
	Destination       string       `json:"destination"`
	DestinationNumber string       `json:"destinationNumber"`
	DestinationUUID   string       `json:"destinationUuid"`
	EditMessage       *editMessage `json:"editMessage"`
	dataMessage
}

type quote struct {
	ID           int64  `json:"id"`
	Author       string `json:"author"`
	AuthorNumber string `json:"authorNumber"`
	AuthorUUID   string `json:"authorUuid"`
	Text         string `json:"text"`
}

type mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

type attachment struct {
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
}

// incoming turns an envelope into a message for the core; false skips
// it: receipts, typing, reactions, deletes, stories and group updates.
func (c *conn) incoming(ctx context.Context, env *envelope) (bridge.Incoming, bool) {
	var (
		dm     *dataMessage
		target int64 // the edited message's timestamp
		self   bool
		peer   string // the other side of a direct chat
	)
	switch {
	case env.SyncMessage != nil && env.SyncMessage.SentMessage != nil:
		// Sent by the person from their phone or another linked device.
		s := env.SyncMessage.SentMessage
		self = true
		c.learnSelf(ctx, env.SourceUUID)
		peer = c.person(ctx, s.DestinationUUID, first(s.DestinationNumber, s.Destination))
		if s.EditMessage != nil {
			dm, target = s.EditMessage.DataMessage, s.EditMessage.TargetSentTimestamp
		} else {
			dm = &s.dataMessage
		}
	case env.EditMessage != nil:
		dm, target = env.EditMessage.DataMessage, env.EditMessage.TargetSentTimestamp
		peer = c.person(ctx, env.SourceUUID, first(env.SourceNumber, env.Source))
	case env.DataMessage != nil:
		dm = env.DataMessage
		peer = c.person(ctx, env.SourceUUID, first(env.SourceNumber, env.Source))
	default:
		return bridge.Incoming{}, false
	}
	if dm == nil || dm.Reaction != nil || dm.RemoteDelete != nil {
		return bridge.Incoming{}, false
	}
	if !self && c.isSelf(env.SourceUUID, env.SourceNumber) {
		self = true
	}

	in := bridge.Incoming{Text: c.render(ctx, dm)}
	if in.Text == "" {
		return bridge.Incoming{}, false
	}
	if self {
		in.Author = bridge.Author{ID: c.selfKey(), Self: true}
	} else {
		id := c.person(ctx, env.SourceUUID, first(env.SourceNumber, env.Source))
		in.Author = bridge.Author{ID: id, Name: c.contactName(ctx, id, first(env.SourceName, env.SourceNumber, id))}
	}
	if g := dm.GroupInfo; g != nil && g.GroupID != "" {
		in.Chat = bridge.RemoteChat{ID: "group:" + g.GroupID, Name: c.groupName(ctx, g.GroupID, g.GroupName)}
	} else {
		if peer == "" {
			return bridge.Incoming{}, false
		}
		fallback := first(env.SourceName, env.SourceNumber, peer)
		if self {
			fallback = first(env.SyncMessage.SentMessage.DestinationNumber, peer)
		}
		in.Chat = bridge.RemoteChat{ID: "user:" + peer, Name: c.contactName(ctx, peer, fallback), Direct: true}
	}
	// Signal names a message by its author and the time it was sent.
	ts := first64(dm.Timestamp, env.Timestamp)
	if ts == 0 {
		return bridge.Incoming{}, false
	}
	in.Time = time.UnixMilli(ts)
	if target != 0 {
		in.Edit = true
		in.ID = in.Author.ID + ":" + strconv.FormatInt(target, 10)
	} else {
		in.ID = in.Author.ID + ":" + strconv.FormatInt(ts, 10)
	}
	if q := dm.Quote; q != nil && q.ID != 0 {
		if a := c.authorKey(ctx, q.AuthorUUID, first(q.AuthorNumber, q.Author)); a != "" {
			in.ReplyTo = a + ":" + strconv.FormatInt(q.ID, 10)
		}
	}
	return in, true
}

// render makes plain text of a message: mentions become @names, and
// attachments and stickers short placeholders. A quote leads as a "> "
// line, since the core does not show replies yet.
func (c *conn) render(ctx context.Context, dm *dataMessage) string {
	var lines []string
	if q := dm.Quote; q != nil && q.Text != "" {
		line, _, _ := strings.Cut(q.Text, "\n")
		lines = append(lines, "> "+line)
	}
	if text := c.mentions(ctx, dm.Message, dm.Mentions); strings.TrimSpace(text) != "" {
		lines = append(lines, text)
	}
	for _, a := range dm.Attachments {
		lines = append(lines, "[attachment: "+first(a.Filename, a.ContentType, "file")+"]")
	}
	if dm.Sticker != nil {
		lines = append(lines, "[sticker]")
	}
	if len(lines) == 1 && dm.Quote != nil && dm.Quote.Text != "" {
		return "" // a quote of nothing
	}
	return strings.Join(lines, "\n")
}

// mentions replaces the placeholder of each mention in text with @name.
// Their positions count UTF-16 code units.
func (c *conn) mentions(ctx context.Context, text string, ms []mention) string {
	if len(ms) == 0 {
		return text
	}
	u := utf16.Encode([]rune(text))
	ms = append([]mention(nil), ms...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Start > ms[j].Start })
	for _, m := range ms {
		if m.Start < 0 || m.Length < 0 || m.Start+m.Length > len(u) {
			continue
		}
		id := c.person(ctx, m.UUID, first(m.Number, m.Name))
		name := utf16.Encode([]rune("@" + c.contactName(ctx, id, first(m.Name, m.Number, id))))
		u = append(u[:m.Start], append(name, u[m.Start+m.Length:]...)...)
	}
	return string(utf16.Decode(u))
}

func first(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func first64(s ...int64) int64 {
	for _, v := range s {
		if v != 0 {
			return v
		}
	}
	return 0
}

// person returns the one id say knows a person by: their ACI (uuid) when
// known, else their number. Signal sends one or the other or both, and
// the same person must not become two chats or two ghosts. Numbers seen
// with a uuid are remembered; for an unknown number signal-cli's contacts
// are read again.
func (c *conn) person(ctx context.Context, uuid, number string) string {
	if !strings.HasPrefix(number, "+") {
		// signal-cli's "source" is the uuid when it has no number.
		if uuid == "" {
			uuid = number
		}
		number = ""
	}
	if uuid != "" {
		if number != "" {
			c.uuids[number] = uuid
		}
		return uuid
	}
	if number == "" {
		return ""
	}
	if u, ok := c.uuids[number]; ok {
		return u
	}
	if c.maybeRefresh(ctx) {
		if u, ok := c.uuids[number]; ok {
			return u
		}
	}
	return number
}
