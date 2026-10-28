package matrix

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/lum1n/say-server/internal/bridge"
)

// encryptedText stands in for a message of an encrypted room.
const encryptedText = "[encrypted message: say can't read encrypted rooms yet]"

// content is the content of a message event, or the new content of an
// edit.
type content struct {
	MsgType    string    `json:"msgtype"`
	Body       string    `json:"body"`
	FileName   string    `json:"filename"`
	URL        string    `json:"url"`
	GeoURI     string    `json:"geo_uri"`
	NewContent *content  `json:"m.new_content"`
	RelatesTo  *relation `json:"m.relates_to"`
}

type relation struct {
	RelType   string `json:"rel_type,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Key       string `json:"key,omitempty"` // of an m.annotation: the reaction
	InReplyTo *struct {
		EventID string `json:"event_id"`
	} `json:"m.in_reply_to,omitempty"`
}

// incoming turns a timeline event into a message for say; false means it
// is not one.
func (c *conn) incoming(ctx context.Context, roomID string, e event) (bridge.Incoming, bool, error) {
	if e.ID == "" || e.Sender == "" {
		return bridge.Incoming{}, false, nil
	}
	var cn content
	text := ""
	switch e.Type {
	case "m.room.encrypted":
		text = encryptedText
	case "m.room.message", "m.sticker":
		if err := json.Unmarshal(e.Content, &cn); err != nil {
			return bridge.Incoming{}, false, nil
		}
	case "m.reaction", "m.room.redaction":
		return c.reaction(ctx, roomID, e)
	default:
		return bridge.Incoming{}, false, nil
	}
	ch, err := c.chat(ctx, roomID)
	if err != nil {
		return bridge.Incoming{}, false, err
	}
	name, err := c.memberName(ctx, roomID, e.Sender)
	if err != nil {
		return bridge.Incoming{}, false, err
	}
	c.remember(e.ID, e.Sender)
	in := bridge.Incoming{
		Chat:   ch,
		ID:     e.ID,
		Author: bridge.Author{ID: e.Sender, Name: name, Self: e.Sender == c.self()},
		Time:   e.time(),
	}
	if e.Type == "m.room.encrypted" {
		in.Text = text
		return in, true, nil
	}
	if r := cn.RelatesTo; r != nil && r.RelType == "m.replace" && r.EventID != "" {
		if cn.NewContent == nil {
			return bridge.Incoming{}, false, nil
		}
		// Homeservers let anyone in a room send an edit of any message;
		// only the original sender's edits count.
		if c.originalSender(ctx, roomID, r.EventID) != e.Sender {
			return bridge.Incoming{}, false, nil
		}
		// An edit names the message it changes; its own id is not kept.
		in.Edit, in.ID = true, r.EventID
		in.Text = render(e.Type, *cn.NewContent, name)
		return in, in.Text != "", nil
	}
	if r := cn.RelatesTo; r != nil && r.InReplyTo != nil {
		in.ReplyTo = r.InReplyTo.EventID
		cn.Body = stripReply(cn.Body)
	}
	in.Text = render(e.Type, cn, name)
	return in, in.Text != "", nil
}

// reaction turns an m.reaction annotation into a reaction for say, keyed
// by its event id, and a redaction into taking back the reaction it
// removes. A redaction may remove anything; say ignores ids it does not
// know as reactions.
func (c *conn) reaction(ctx context.Context, roomID string, e event) (bridge.Incoming, bool, error) {
	r := &bridge.IncomingReaction{}
	id := e.ID
	if e.Type == "m.reaction" {
		var cn content
		if json.Unmarshal(e.Content, &cn) != nil || cn.RelatesTo == nil || cn.RelatesTo.RelType != "m.annotation" ||
			cn.RelatesTo.EventID == "" || cn.RelatesTo.Key == "" {
			return bridge.Incoming{}, false, nil
		}
		r.Target, r.Key = cn.RelatesTo.EventID, cn.RelatesTo.Key
	} else {
		var cn struct {
			Redacts string `json:"redacts"`
		}
		json.Unmarshal(e.Content, &cn)
		id = cmp.Or(e.Redacts, cn.Redacts)
		if id == "" {
			return bridge.Incoming{}, false, nil
		}
		r.Removed = true
	}
	ch, err := c.chat(ctx, roomID)
	if err != nil {
		return bridge.Incoming{}, false, err
	}
	name, err := c.memberName(ctx, roomID, e.Sender)
	if err != nil {
		return bridge.Incoming{}, false, err
	}
	return bridge.Incoming{
		Chat:     ch,
		ID:       id,
		Author:   bridge.Author{ID: e.Sender, Name: name, Self: e.Sender == c.self()},
		Time:     e.time(),
		Reaction: r,
	}, true, nil
}

// render makes plain text of a message: text as it is, and anything else
// as a bracketed note with its link.
func render(typ string, cn content, sender string) string {
	body := strings.TrimSpace(cn.Body)
	if typ == "m.sticker" {
		return note("sticker", body, "")
	}
	switch cn.MsgType {
	case "m.text", "m.notice", "":
		return body
	case "m.emote":
		return "* " + sender + " " + body
	case "m.image", "m.video", "m.audio", "m.file":
		label := strings.TrimPrefix(cn.MsgType, "m.")
		if cn.FileName != "" && cn.FileName != body {
			// The body is a caption.
			return strings.TrimSpace(note(label, cn.FileName, cn.URL) + "\n" + body)
		}
		return note(label, body, cn.URL)
	case "m.location":
		return note("location", body, cn.GeoURI)
	default:
		return note(strings.TrimPrefix(cn.MsgType, "m."), body, "")
	}
}

// note is "[kind: what] link".
func note(kind, what, link string) string {
	s := "[" + kind
	if what != "" {
		s += ": " + what
	}
	s += "]"
	if link != "" {
		s += " " + link
	}
	return s
}

// stripReply removes the quote of the message replied to that clients
// put before a reply's text: "> <@ada:example.org> what she said" lines
// and a blank line.
func stripReply(body string) string {
	if !strings.HasPrefix(body, "> ") {
		return body
	}
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], ">") {
		i++
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	return strings.Join(lines[i:], "\n")
}

// Send posts a text message. The transaction id is say's event id, so a
// send the homeserver already took is not taken twice; the event id it
// answers is the one the sync later brings back.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	cn := map[string]any{"msgtype": "m.text", "body": m.Text}
	if m.ReplyTo != "" {
		cn["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]string{"event_id": m.ReplyTo}}
	}
	return c.send(ctx, m.Chat, m.EventID, cn)
}

// Edit changes a message the account sent.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	cn := map[string]any{
		"msgtype":       "m.text",
		"body":          "* " + m.Text,
		"m.new_content": map[string]string{"msgtype": "m.text", "body": m.Text},
		"m.relates_to":  map[string]string{"rel_type": "m.replace", "event_id": remoteID},
	}
	// One transaction per message and text: the same edit tried again is
	// the same edit.
	sum := sha256.Sum256([]byte(m.Text))
	txn := "edit-" + m.EventID + "-" + hex.EncodeToString(sum[:8])
	_, err := c.send(ctx, m.Chat, txn, cn)
	return err
}

// React annotates a message with the owner's reaction, returning the
// reaction's event id, or takes one back by redacting the reaction it
// added. The transaction id is the reaction event's, so a retry is the
// same reaction.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	if r.Removed {
		if r.Added == "" {
			return "", nil
		}
		err := c.do(ctx, http.MethodPut, path("rooms", r.Chat, "redact", r.Added, r.EventID), nil, map[string]any{}, nil)
		if isRefusal(err) {
			return "", bridge.Rejected(err)
		}
		return "", err
	}
	cn := map[string]any{"m.relates_to": map[string]string{"rel_type": "m.annotation", "event_id": r.Target, "key": r.Key}}
	return c.sendEvent(ctx, r.Chat, "m.reaction", r.EventID, cn)
}

func (c *conn) send(ctx context.Context, roomID, txn string, cn any) (string, error) {
	return c.sendEvent(ctx, roomID, "m.room.message", txn, cn)
}

func (c *conn) sendEvent(ctx context.Context, roomID, typ, txn string, cn any) (string, error) {
	var resp struct {
		EventID string `json:"event_id"`
	}
	err := c.do(ctx, http.MethodPut, path("rooms", roomID, "send", typ, txn), nil, cn, &resp)
	if isRefusal(err) {
		return "", bridge.Rejected(err)
	}
	if err != nil {
		return "", err
	}
	if resp.EventID == "" {
		return "", fmt.Errorf("matrix: the homeserver gave no event id")
	}
	c.remember(resp.EventID, c.self())
	return resp.EventID, nil
}

// maxSenders bounds the senders remembered for checking edits.
const maxSenders = 10000

// remember keeps who sent an event.
func (c *conn) remember(eventID, sender string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.senders == nil {
		c.senders = map[string]string{}
	}
	if _, ok := c.senders[eventID]; ok {
		return
	}
	c.senders[eventID] = sender
	c.order = append(c.order, eventID)
	if len(c.order) > maxSenders {
		delete(c.senders, c.order[0])
		c.order = c.order[1:]
	}
}

// originalSender returns who sent an event, asking the homeserver when it
// is not remembered; "" means it is not known, and an edit of it is
// dropped.
func (c *conn) originalSender(ctx context.Context, roomID, eventID string) string {
	c.mu.Lock()
	sender, ok := c.senders[eventID]
	c.mu.Unlock()
	if ok {
		return sender
	}
	var e struct {
		Sender string `json:"sender"`
	}
	if err := c.do(ctx, http.MethodGet, path("rooms", roomID, "event", eventID), nil, nil, &e); err != nil || e.Sender == "" {
		return ""
	}
	c.remember(eventID, e.Sender)
	return e.Sender
}
