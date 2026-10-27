package telegram

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/lum1n/say-server/internal/bridge"
)

// receiver turns updates into incoming messages. It is the handler of
// gotd's update manager, which calls it from its own goroutines and from
// inside API calls (the result of a send is an update too), so it only
// queues: Host.Receive may wait for a send in progress.
type receiver struct {
	st *saved
	q  *queue
}

// Handle implements telegram.UpdateHandler. The update manager hands it
// *tg.Updates; the other forms arrive only before the manager runs.
func (r *receiver) Handle(_ context.Context, u tg.UpdatesClass) error {
	var list []tg.UpdateClass
	switch u := u.(type) {
	case *tg.Updates:
		r.st.remember(u.Users, u.Chats)
		list = u.Updates
	case *tg.UpdatesCombined:
		r.st.remember(u.Users, u.Chats)
		list = u.Updates
	default:
		return nil
	}
	for _, upd := range list {
		var msg tg.MessageClass
		edit := false
		switch upd := upd.(type) {
		case *tg.UpdateNewMessage:
			msg = upd.Message
		case *tg.UpdateNewChannelMessage:
			msg = upd.Message
		case *tg.UpdateEditMessage:
			msg, edit = upd.Message, true
		case *tg.UpdateEditChannelMessage:
			msg, edit = upd.Message, true
		default:
			continue
		}
		if in, ok := incoming(r.st, msg); ok {
			in.Edit = edit
			r.q.push(in)
		}
	}
	return nil
}

// incoming converts a message. Service messages (joins, pins, calls…) and
// empty ones are skipped.
func incoming(st *saved, mc tg.MessageClass) (bridge.Incoming, bool) {
	m, ok := mc.(*tg.Message)
	if !ok || m.PeerID == nil {
		return bridge.Incoming{}, false
	}
	chat, ok := remoteChat(st, m.PeerID)
	if !ok {
		return bridge.Incoming{}, false
	}
	in := bridge.Incoming{
		Chat:   chat,
		ID:     strconv.Itoa(m.ID),
		Author: author(st, m),
		Text:   messageText(m),
		Time:   time.Unix(int64(m.Date), 0),
	}
	if h, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok && h.ReplyToMsgID != 0 && h.ReplyToPeerID == nil {
		// A reply to a message of another chat has no id here.
		in.ReplyTo = strconv.Itoa(h.ReplyToMsgID)
	}
	if in.Text == "" {
		return bridge.Incoming{}, false
	}
	return in, true
}

// peerID returns the chat id of a peer.
func peerID(p tg.PeerClass) (string, bool) {
	switch p := p.(type) {
	case *tg.PeerUser:
		return chatID("user", p.UserID), true
	case *tg.PeerChat:
		return chatID("chat", p.ChatID), true
	case *tg.PeerChannel:
		return chatID("channel", p.ChannelID), true
	}
	return "", false
}

// remoteChat returns the chat a message was sent in.
func remoteChat(st *saved, p tg.PeerClass) (bridge.RemoteChat, bool) {
	id, ok := peerID(p)
	if !ok {
		return bridge.RemoteChat{}, false
	}
	chat := bridge.RemoteChat{ID: id, Name: st.name(id)}
	if u, ok := p.(*tg.PeerUser); ok {
		chat.Direct = true
		if u.UserID == st.self() {
			chat.Name = "Saved Messages"
		}
	}
	return chat, true
}

// author returns who wrote a message: the owner for their own, else the
// sender, which is the other side in a private chat and the channel
// itself for a channel post.
func author(st *saved, m *tg.Message) bridge.Author {
	if m.Out {
		return bridge.Author{Self: true}
	}
	from := m.FromID
	if from == nil {
		from = m.PeerID
	}
	if u, ok := from.(*tg.PeerUser); ok && u.UserID == st.self() {
		return bridge.Author{Self: true}
	}
	id, _ := peerID(from)
	name := st.name(id)
	if name == "" {
		name = id
	}
	return bridge.Author{ID: id, Name: name}
}

// userName is how a user is shown: their name, else their username.
func userName(u *tg.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	switch {
	case name != "":
		return name
	case u.Username != "":
		return "@" + u.Username
	case u.Deleted:
		return "Deleted Account"
	}
	return ""
}

// messageText is a message as plain text: a placeholder for its media,
// then its text or caption.
func messageText(m *tg.Message) string {
	text := plainText(m.Message, m.Entities)
	media := mediaText(m.Media)
	switch {
	case media == "":
		return text
	case text == "":
		return media
	}
	return media + "\n" + text
}

// plainText renders formatted text readably. Mentions and formatting are
// already plain in the text; a link hidden behind words gets its URL
// after them.
func plainText(text string, entities []tg.MessageEntityClass) string {
	type insert struct {
		from, to int // UTF-16 offsets, as Telegram counts
		url      string
	}
	var ins []insert
	for _, e := range entities {
		if e, ok := e.(*tg.MessageEntityTextURL); ok {
			ins = append(ins, insert{from: e.Offset, to: e.Offset + e.Length, url: e.URL})
		}
	}
	if len(ins) == 0 {
		return text
	}
	sort.SliceStable(ins, func(i, j int) bool { return ins[i].to < ins[j].to })
	u := utf16.Encode([]rune(text))
	var b strings.Builder
	last := 0
	for _, in := range ins {
		to := min(max(in.to, last), len(u))
		from := min(max(in.from, 0), to)
		b.WriteString(string(utf16.Decode(u[last:to])))
		if string(utf16.Decode(u[from:to])) != in.url {
			b.WriteString(" (" + in.url + ")")
		}
		last = to
	}
	b.WriteString(string(utf16.Decode(u[last:])))
	return b.String()
}

// mediaText is a short placeholder for an attachment, or "" for none and
// for link previews, whose link is in the text.
func mediaText(media tg.MessageMediaClass) string {
	switch m := media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return ""
	case *tg.MessageMediaPhoto:
		return "[photo]"
	case *tg.MessageMediaDocument:
		return documentText(m)
	case *tg.MessageMediaGeo:
		return "[location" + geoText(m.Geo) + "]"
	case *tg.MessageMediaGeoLive:
		return "[live location" + geoText(m.Geo) + "]"
	case *tg.MessageMediaVenue:
		return "[location: " + strings.TrimSpace(m.Title+", "+m.Address) + geoText(m.Geo) + "]"
	case *tg.MessageMediaContact:
		return "[contact: " + strings.Join(strings.Fields(m.FirstName+" "+m.LastName+" "+m.PhoneNumber), " ") + "]"
	case *tg.MessageMediaPoll:
		return "[poll: " + m.Poll.Question.Text + "]"
	case *tg.MessageMediaDice:
		return fmt.Sprintf("[%s %d]", m.Emoticon, m.Value)
	case *tg.MessageMediaGame:
		return "[game: " + m.Game.Title + "]"
	case *tg.MessageMediaInvoice:
		return "[invoice: " + m.Title + "]"
	case *tg.MessageMediaStory:
		return "[story]"
	}
	return "[attachment]"
}

// geoText is a map link for a point, or "".
func geoText(g tg.GeoPointClass) string {
	p, ok := g.(*tg.GeoPoint)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" https://www.openstreetmap.org/?mlat=%.6f&mlon=%.6f", p.Lat, p.Long)
}

// documentText names the kind of a file from its attributes.
func documentText(m *tg.MessageMediaDocument) string {
	d, ok := m.Document.(*tg.Document)
	if !ok {
		return "[file]"
	}
	var name string
	video, gif := false, false
	for _, a := range d.Attributes {
		switch a := a.(type) {
		case *tg.DocumentAttributeSticker:
			return "[" + strings.TrimSpace("sticker "+a.Alt) + "]"
		case *tg.DocumentAttributeCustomEmoji:
			return "[" + strings.TrimSpace("sticker "+a.Alt) + "]"
		case *tg.DocumentAttributeAudio:
			if a.Voice {
				return "[voice message]"
			}
			title := strings.Trim(a.Performer+" - "+a.Title, " -")
			if title == "" {
				return "[audio]"
			}
			return "[audio: " + title + "]"
		case *tg.DocumentAttributeVideo:
			if a.RoundMessage {
				return "[video message]"
			}
			video = true
		case *tg.DocumentAttributeAnimated:
			gif = true
		case *tg.DocumentAttributeFilename:
			name = a.FileName
		}
	}
	switch {
	case m.Round:
		return "[video message]"
	case m.Voice:
		return "[voice message]"
	case gif:
		return "[GIF]"
	case video || m.Video:
		return "[video]"
	case name != "":
		return "[file: " + name + "]"
	}
	return "[file]"
}

// sentID finds the id of the message a send made in its result: the
// server maps our random id to it, or returns it short.
func sentID(u tg.UpdatesClass, randomID int64) (int, bool) {
	var list []tg.UpdateClass
	switch u := u.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID, true
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	default:
		return 0, false
	}
	for _, upd := range list {
		if upd, ok := upd.(*tg.UpdateMessageID); ok && upd.RandomID == randomID {
			return upd.ID, true
		}
	}
	// Without the mapping, the one new outgoing message is ours.
	for _, upd := range list {
		var mc tg.MessageClass
		switch upd := upd.(type) {
		case *tg.UpdateNewMessage:
			mc = upd.Message
		case *tg.UpdateNewChannelMessage:
			mc = upd.Message
		}
		if m, ok := mc.(*tg.Message); ok && m.Out {
			return m.ID, true
		}
	}
	return 0, false
}

// queue holds incoming messages for the goroutine that passes them to
// the host. It is unbounded, so whoever pushes never waits.
type queue struct {
	mu    sync.Mutex
	items []bridge.Incoming
	wake  chan struct{}
}

func newQueue() *queue { return &queue{wake: make(chan struct{}, 1)} }

func (q *queue) push(in bridge.Incoming) {
	q.mu.Lock()
	q.items = append(q.items, in)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pop waits for the next message.
func (q *queue) pop(ctx context.Context) (bridge.Incoming, error) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			in := q.items[0]
			q.items[0] = bridge.Incoming{}
			q.items = q.items[1:]
			q.mu.Unlock()
			return in, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return bridge.Incoming{}, ctx.Err()
		case <-q.wake:
		}
	}
}
