package telegrambot

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:         "telegram_bot",
		Title:        "Telegram bot",
		Modes:        []string{store.ModeBot},
		Fields:       fields,
		Capabilities: []connector.Capability{connector.CapEdits, connector.CapReplies, connector.CapReactions, connector.CapAttachments},
		Open:         open,
	})
}

var fields = []bridge.Field{
	{
		Key: "bot_token", Label: "Bot token", Kind: "secret", Required: true, Placeholder: "123456:ABC…",
		Help: "Make a bot by talking to @BotFather on Telegram (/newbot) and paste the token it gives you. " +
			"Turn privacy mode off (/setprivacy, pick the bot, Disable) so the bot sees all messages in groups, " +
			"then add it to the groups (or as an admin to the channels) to bridge. " +
			"Link a room by the chat's id, such as -1001234567890: send /chatid in the chat and the bot answers with it. " +
			"In a group with topics, /chatid in a topic also tells the topic's id, such as -1001234567890/42: a room " +
			"linked to it gets that topic and writes to it, while one linked to the group gets every topic and writes " +
			"to General.",
	},
}

// config is a bridge's settings.
type config struct {
	Token string `json:"bot_token"`
}

// state is what a connection keeps between runs: the next update to ask
// for.
type state struct {
	Offset int64 `json:"offset,omitempty"`
}

var tokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// open makes a connection; it only checks the config.
func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	var cfg config
	if err := json.Unmarshal(b.Config, &cfg); err != nil {
		return nil, fmt.Errorf("telegram bot: bad config: %w", err)
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	if !tokenPattern.MatchString(cfg.Token) {
		return nil, errors.New("telegram bot: the bot token looks like 123456:ABC…, as @BotFather gives it")
	}
	var st state
	if len(b.State) > 0 {
		// A state that does not parse only costs updates Telegram still
		// holds being read again, which the core ignores.
		json.Unmarshal(b.State, &st)
	}
	return &conn{
		id:     b.ID,
		h:      h,
		cfg:    cfg,
		api:    "https://api.telegram.org/",
		http:   &http.Client{},
		poll:   50,
		offset: st.Offset,
	}, nil
}

// conn is a running relay bot.
type conn struct {
	id   string // the bridge's
	h    bridge.Host
	cfg  config
	api  string // base URL of the Bot API, ending in /
	http *http.Client
	poll int // long-poll timeout of getUpdates, in seconds

	// The rest is only used by Run's goroutine.
	offset   int64
	botID    int64
	username string
	unlinked map[string]bool // chats and topics logged as not linked
}

// allowed are the update kinds asked for.
var allowed = []string{"message", "edited_message", "channel_post", "edited_channel_post", "message_reaction"}

// Run checks the token, then long-polls updates until ctx ends.
func (c *conn) Run(ctx context.Context) error {
	var me user
	if err := c.call(ctx, "getMe", struct{}{}, &me); err != nil {
		if e, ok := errors.AsType[*apiError](err); ok && (e.Code == http.StatusUnauthorized || e.Code == http.StatusNotFound) {
			return fmt.Errorf("telegram bot: the bot token was refused; check it with @BotFather: %w", err)
		}
		return err
	}
	c.botID, c.username = me.ID, me.Username
	c.unlinked = map[string]bool{}
	c.h.Connected(ctx)

	failures := 0
	for {
		var ups []update
		err := c.call(ctx, "getUpdates", map[string]any{
			"offset":          c.offset,
			"timeout":         c.poll,
			"allowed_updates": allowed,
		}, &ups)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e, ok := errors.AsType[*apiError](err); ok {
			switch {
			case e.Code == http.StatusConflict:
				return fmt.Errorf("telegram bot: another program gets this bot's updates: a webhook is set (remove it with deleteWebhook) or another copy of the bot runs: %w", err)
			case e.Code == http.StatusUnauthorized:
				return fmt.Errorf("telegram bot: the bot token was refused; check it with @BotFather: %w", err)
			case e.refused():
				return err
			}
		}
		if err != nil {
			// Network trouble and Telegram's own errors pass; a few
			// tries before the bridge shows it.
			failures++
			if failures > 4 {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<failures) * time.Second):
			}
			continue
		}
		failures = 0
		if len(ups) == 0 {
			continue
		}
		if err := c.batch(ctx, ups); err != nil {
			return err
		}
	}
}

// batch handles a batch of updates and saves the offset past them, or
// past those handled when one fails.
func (c *conn) batch(ctx context.Context, ups []update) error {
	from := c.offset
	defer func() {
		if c.offset == from {
			return
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		data, _ := json.Marshal(state{Offset: c.offset})
		if err := c.h.SaveState(sctx, data); err != nil {
			slog.Warn("telegram bot: save offset failed", "bridge", c.id, "err", err)
		}
	}()
	chats, err := c.h.Chats(ctx)
	if err != nil {
		return err
	}
	linked := map[string]bool{}
	for _, id := range chats {
		linked[id] = true
	}
	for _, u := range ups {
		if u.UpdateID < c.offset {
			continue
		}
		if err := c.handle(ctx, u, linked); err != nil {
			return err
		}
		c.offset = u.UpdateID + 1
	}
	return nil
}

// handle passes an update's message or reactions on, if its chat is
// linked.
func (c *conn) handle(ctx context.Context, u update, linked map[string]bool) error {
	if r := u.MessageReaction; r != nil {
		// A reaction does not say which topic its message is in, so it
		// reaches a room linked to the whole group only.
		id := strconv.FormatInt(r.Chat.ID, 10)
		if !linked[id] {
			return nil
		}
		for _, in := range reactions(r, c.botID) {
			if err := c.h.Receive(ctx, in); err != nil {
				return err
			}
		}
		return nil
	}
	m, edit := u.Message, false
	switch {
	case u.ChannelPost != nil:
		m = u.ChannelPost
	case u.EditedMessage != nil:
		m, edit = u.EditedMessage, true
	case u.EditedChannelPost != nil:
		m, edit = u.EditedChannelPost, true
	}
	if m == nil {
		return nil
	}
	if !edit && c.isChatIDCommand(m.Text) {
		c.answerChatID(ctx, m)
		return nil
	}
	// A topic linked on its own goes to its room, else the group's.
	id := strconv.FormatInt(m.Chat.ID, 10)
	if t := topicID(m); linked[t] {
		id = t
	}
	if !linked[id] {
		if t := cmp.Or(topicID(m), id); !c.unlinked[t] {
			c.unlinked[t] = true
			slog.Info("telegram bot: message from a chat no room is linked to", "bridge", c.id, "chat_id", t, "title", chatName(m.Chat))
		}
		return nil
	}
	in, ok := incoming(m, edit, c.botID, c.download)
	if !ok {
		return nil
	}
	in.Chat.ID = id
	return c.h.Receive(ctx, in)
}

// isChatIDCommand reports whether text is /chatid, maybe addressed to
// this bot.
func (c *conn) isChatIDCommand(text string) bool {
	cmd, to, at := strings.Cut(text, "@")
	return cmd == "/chatid" && (!at || c.username != "" && strings.EqualFold(to, c.username))
}

// answerChatID tells a chat its id, and a topic its own, to link a room
// to it.
func (c *conn) answerChatID(ctx context.Context, m *message) {
	text := fmt.Sprintf("This chat's id is <code>%d</code>. Link a room to it with that id.", m.Chat.ID)
	if t := topicID(m); t != "" {
		text = fmt.Sprintf("This topic's id is <code>%s</code> and the group's is <code>%d</code>. "+
			"Link a room to the topic with its id, or to the group to get every topic and write to General.", t, m.Chat.ID)
	}
	if _, err := c.sendMessage(ctx, cmp.Or(topicID(m), strconv.FormatInt(m.Chat.ID, 10)), text, m.MessageID); err != nil {
		slog.Warn("telegram bot: answer /chatid failed", "bridge", c.id, "chat_id", m.Chat.ID, "err", err)
	}
}

// Send posts a message as the bot, with the sender's name. A text too
// long for one message goes out in several, and each file in one of its
// own with the text as the first one's caption; a text too long for a
// caption goes before the files. The id is the first message's, which
// carries the reply.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	reply, _ := strconv.ParseInt(m.ReplyTo, 10, 64)
	var steps []func(reply int64) (int64, error)
	text := m.Text
	if len(m.Media) == 0 || text != "" && !fits(m.Sender, text) {
		for _, p := range parts(m.Sender, text) {
			steps = append(steps, func(reply int64) (int64, error) { return c.sendMessage(ctx, m.Chat, p, reply) })
		}
		text = ""
	}
	for k, f := range m.Media {
		capt := caption(m.Sender, "")
		if k == 0 {
			capt = caption(m.Sender, text)
		}
		steps = append(steps, func(reply int64) (int64, error) { return c.sendFile(ctx, m.Chat, f, capt, reply) })
	}
	first := ""
	for n, step := range steps {
		id, err := step(reply)
		if err != nil && n == 0 {
			return "", classify(err)
		}
		if err != nil {
			// Part of it arrived, so it is not refused.
			return first, fmt.Errorf("telegram bot: only %d of %d parts sent: %s", n, len(steps), err.Error())
		}
		if n == 0 {
			first = strconv.FormatInt(id, 10)
		}
		reply = 0
	}
	return first, nil
}

// Edit changes a message the bot sent. A message that was sent in parts
// has its first part changed, cut short to fit; one that is a file has its
// caption changed.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	id, err := strconv.ParseInt(remoteID, 10, 64)
	if err != nil {
		return bridge.Rejected(fmt.Errorf("telegram bot: bad message id %q", remoteID))
	}
	chat, _ := splitChat(m.Chat)
	err = c.call(ctx, "editMessageText", map[string]any{
		"chat_id":              chat,
		"message_id":           id,
		"text":                 single(m.Sender, m.Text, maxText),
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}, nil)
	if e, ok := errors.AsType[*apiError](err); ok && strings.Contains(e.Description, "no text in the message") {
		err = c.call(ctx, "editMessageCaption", map[string]any{
			"chat_id":    chat,
			"message_id": id,
			"caption":    caption(m.Sender, m.Text),
			"parse_mode": "HTML",
		}, nil)
	}
	if e, ok := errors.AsType[*apiError](err); ok && strings.Contains(e.Description, "message is not modified") {
		return nil
	}
	return classify(err)
}

// React sets the bot's reaction on a message: the key, or none when it is
// taken back. Bots may use only Telegram's standard reaction emoji; others
// are refused.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	id, err := strconv.ParseInt(r.Target, 10, 64)
	if err != nil {
		return "", bridge.Rejected(fmt.Errorf("telegram bot: bad message id %q", r.Target))
	}
	chat, _ := splitChat(r.Chat)
	set := []map[string]string{}
	if !r.Removed {
		set = append(set, map[string]string{"type": "emoji", "emoji": r.Key})
	}
	err = c.call(ctx, "setMessageReaction", map[string]any{"chat_id": chat, "message_id": id, "reaction": set}, nil)
	return "", classify(err)
}

// sendMessage posts HTML text to a chat or topic, answering reply if it
// is not 0, and returns the message's id.
func (c *conn) sendMessage(ctx context.Context, chat, text string, reply int64) (int64, error) {
	chat, topic := splitChat(chat)
	p := map[string]any{
		"chat_id":              chat,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if topic != 0 {
		p["message_thread_id"] = topic
	}
	if reply != 0 {
		p["reply_parameters"] = map[string]any{"message_id": reply, "allow_sending_without_reply": true}
	}
	var r struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.call(ctx, "sendMessage", p, &r); err != nil {
		return 0, err
	}
	return r.MessageID, nil
}

// maxUpload is the largest file a bot may send, and maxPhoto the largest
// it may send as a photo.
const (
	maxUpload = 50 << 20
	maxPhoto  = 10 << 20
)

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// sendFile posts a file to a chat or topic with an HTML caption, answering
// reply if it is not 0, and returns the message's id. An image goes as a
// photo, which Telegram shows inline; anything else as a document.
func (c *conn) sendFile(ctx context.Context, chat string, f bridge.Media, caption string, reply int64) (int64, error) {
	r, err := f.Open(ctx)
	if err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(r, maxUpload+1))
	r.Close()
	if err != nil {
		return 0, err
	}
	if len(data) > maxUpload {
		return 0, bridge.Rejected(fmt.Errorf("telegram bot: %s is larger than the 50 MB a bot may send", f.Name))
	}
	method, field := "sendDocument", "document"
	if photos[f.Mime] && len(data) <= maxPhoto {
		method, field = "sendPhoto", "photo"
	}
	chat, topic := splitChat(chat)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("chat_id", chat)
	if topic != 0 {
		w.WriteField("message_thread_id", strconv.FormatInt(topic, 10))
	}
	if caption != "" {
		w.WriteField("caption", caption)
		w.WriteField("parse_mode", "HTML")
	}
	if reply != 0 {
		w.WriteField("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, reply))
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, quoteEscaper.Replace(cmp.Or(f.Name, "file"))))
	h.Set("Content-Type", cmp.Or(f.Mime, "application/octet-stream"))
	pw, err := w.CreatePart(h)
	if err != nil {
		return 0, err
	}
	pw.Write(data)
	if err := w.Close(); err != nil {
		return 0, err
	}
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.do(ctx, method, body.Bytes(), w.FormDataContentType(), uploadTimeout, &m); err != nil {
		return 0, err
	}
	return m.MessageID, nil
}

// photos are the image types Telegram takes as photos.
var photos = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

// classify marks errors Telegram gave for certain as refusals.
func classify(err error) error {
	if e, ok := errors.AsType[*apiError](err); ok && e.refused() {
		return bridge.Rejected(err)
	}
	return err
}
