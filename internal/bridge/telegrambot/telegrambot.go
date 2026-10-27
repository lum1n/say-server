package telegrambot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/say-v2/internal/bridge"
	"github.com/lum1n/say-v2/internal/connector"
	"github.com/lum1n/say-v2/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:         "telegram_bot",
		Title:        "Telegram bot",
		Modes:        []string{store.ModeBot},
		Fields:       fields,
		Capabilities: []connector.Capability{connector.CapEdits},
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
var allowed = []string{"message", "edited_message", "channel_post", "edited_channel_post"}

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

// handle passes an update's message on, if its chat is linked.
func (c *conn) handle(ctx context.Context, u update, linked map[string]bool) error {
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
	in, ok := incoming(m, edit, c.botID)
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
// long for one message goes out in several; the id is the first one's.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	reply, _ := strconv.ParseInt(m.ReplyTo, 10, 64)
	ps := parts(m.Sender, m.Text)
	first := ""
	for n, p := range ps {
		id, err := c.sendMessage(ctx, m.Chat, p, reply)
		if err != nil && n == 0 {
			return "", classify(err)
		}
		if err != nil {
			// Part of it arrived, so it is not refused.
			return first, fmt.Errorf("telegram bot: only %d of %d parts sent: %s", n, len(ps), err.Error())
		}
		if n == 0 {
			first = strconv.FormatInt(id, 10)
		}
		reply = 0
	}
	return first, nil
}

// Edit changes a message the bot sent. A message that was sent in parts
// has its first part changed, cut short to fit.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	id, err := strconv.ParseInt(remoteID, 10, 64)
	if err != nil {
		return bridge.Rejected(fmt.Errorf("telegram bot: bad message id %q", remoteID))
	}
	chat, _ := splitChat(m.Chat)
	err = c.call(ctx, "editMessageText", map[string]any{
		"chat_id":              chat,
		"message_id":           id,
		"text":                 single(m.Sender, m.Text),
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}, nil)
	if e, ok := errors.AsType[*apiError](err); ok && strings.Contains(e.Description, "message is not modified") {
		return nil
	}
	return classify(err)
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

// classify marks errors Telegram gave for certain as refusals.
func classify(err error) error {
	if e, ok := errors.AsType[*apiError](err); ok && e.refused() {
		return bridge.Rejected(err)
	}
	return err
}
