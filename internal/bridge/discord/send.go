package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lum1n/say-server/internal/bridge"
)

// maxText is the longest message Discord takes. Longer text is cut with
// "…" rather than split: one say message stays one Discord message, which
// an edit can change.
const maxText = 2000

// noHookFor is how long a channel where the bot may not use webhooks is
// written to as the bot, before trying webhooks again.
const noHookFor = 10 * time.Minute

// webhook is a channel's say webhook, which posts under any name.
type webhook struct {
	ID    string `json:"id"`
	Type  int    `json:"type"`
	Token string `json:"token"`
	Name  string `json:"name"`
	App   string `json:"application_id"`
	User  *user  `json:"user"`
}

// noMentions keeps relayed text from pinging anyone.
var noMentions = map[string][]string{"parse": {}}

// Send posts a message through the channel's webhook, under the sender's
// name, or as the bot when it may not use webhooks.
func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	id, err := c.send(ctx, m)
	if _, ok := errors.AsType[*apiError](err); ok {
		return "", bridge.Rejected(err)
	}
	return id, err
}

func (c *conn) send(ctx context.Context, m bridge.Outgoing) (string, error) {
	ch, thread, err := c.hookChannel(ctx, m.Chat)
	if err != nil {
		return "", err
	}
	query := "?wait=true"
	if thread != "" {
		query += "&thread_id=" + thread
	}
	for try := 0; try < 2; try++ {
		h, err := c.hook(ctx, ch, true)
		if err != nil {
			return "", err
		}
		if h == nil {
			break
		}
		var r struct {
			ID string `json:"id"`
		}
		body := map[string]any{"content": clip(m.Text), "username": username(m.Sender), "allowed_mentions": noMentions}
		err = c.post(ctx, "/webhooks/"+h.ID+"/"+h.Token+query, body, m.Media, &r)
		if status(err) == http.StatusNotFound {
			// Someone deleted the webhook; find or make another.
			c.forgetHook(ch)
			continue
		}
		if err != nil {
			return "", err
		}
		c.sent(r.ID, "hook")
		return r.ID, nil
	}
	var r struct {
		ID string `json:"id"`
	}
	body := map[string]any{"content": botText(m), "allowed_mentions": noMentions}
	if m.ReplyTo != "" {
		body["message_reference"] = map[string]any{"message_id": m.ReplyTo, "fail_if_not_exists": false}
	}
	if err := c.post(ctx, "/channels/"+m.Chat+"/messages", body, m.Media, &r); err != nil {
		return "", err
	}
	c.sent(r.ID, "bot")
	return r.ID, nil
}

// post creates a message, with its files if it has any.
func (c *conn) post(ctx context.Context, path string, body map[string]any, files []bridge.Media, out any) error {
	if len(files) == 0 {
		return c.do(ctx, http.MethodPost, path, body, out)
	}
	return c.upload(ctx, http.MethodPost, path, body, files, out)
}

// React adds the bot's reaction to a message, or takes it back. A
// server's own emoji needs its id, which say does not keep, so a :name:
// key is refused.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	if strings.HasPrefix(r.Key, ":") {
		return "", bridge.Rejected(fmt.Errorf("discord: custom emoji %s cannot be sent", r.Key))
	}
	method := http.MethodPut
	if r.Removed {
		method = http.MethodDelete
	}
	path := "/channels/" + r.Chat + "/messages/" + r.Target + "/reactions/" + url.PathEscape(r.Key) + "/@me"
	err := c.do(ctx, method, path, nil, nil)
	if _, ok := errors.AsType[*apiError](err); ok {
		return "", bridge.Rejected(err)
	}
	return "", err
}

// Edit changes a message the bridge sent, through the webhook that sent
// it or as the bot.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	err := c.edit(ctx, m, remoteID)
	if _, ok := errors.AsType[*apiError](err); ok {
		return bridge.Rejected(err)
	}
	return err
}

func (c *conn) edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	c.mu.Lock()
	via := c.sentBy[remoteID]
	c.mu.Unlock()
	if via != "bot" {
		ch, thread, err := c.hookChannel(ctx, m.Chat)
		if err != nil {
			return err
		}
		query := ""
		if thread != "" {
			query = "?thread_id=" + thread
		}
		h, err := c.hook(ctx, ch, false)
		if err != nil {
			return err
		}
		if h != nil {
			body := map[string]any{"content": clip(m.Text), "allowed_mentions": noMentions}
			err := c.do(ctx, http.MethodPatch, "/webhooks/"+h.ID+"/"+h.Token+"/messages/"+remoteID+query, body, nil)
			if s := status(err); s != http.StatusNotFound && s != http.StatusForbidden {
				return err
			}
		}
	}
	body := map[string]any{"content": botText(m), "allowed_mentions": noMentions}
	return c.do(ctx, http.MethodPatch, "/channels/"+m.Chat+"/messages/"+remoteID, body, nil)
}

// hookChannel returns the channel whose webhook posts to chat, and
// chat's id again if it is a thread in that channel. A thread, such as a forum
// post, has no webhooks of its own: its channel's post into it with
// thread_id. What a channel is comes from the gateway, else is asked.
func (c *conn) hookChannel(ctx context.Context, chat string) (ch, thread string, err error) {
	c.mu.Lock()
	parent, known := c.parents[chat]
	c.mu.Unlock()
	if !known {
		var info channel
		err := c.do(ctx, http.MethodGet, "/channels/"+chat, nil, &info)
		if _, ok := errors.AsType[*apiError](err); err != nil && !ok {
			return "", "", err
		}
		// One the bot may not see is taken for a plain channel; posting
		// to it says why it fails.
		info.ID = chat
		c.name(info)
		parent = info.parent()
	}
	if parent == "" {
		return chat, "", nil
	}
	return parent, chat, nil
}

// hook returns a channel's say webhook: cached, found among the
// channel's webhooks, or made if create. It is nil when the bot may not
// manage webhooks there.
func (c *conn) hook(ctx context.Context, ch string, create bool) (*webhook, error) {
	c.hookMu.Lock()
	defer c.hookMu.Unlock()
	c.mu.Lock()
	h, at := c.hooks[ch], c.noHook[ch]
	self, app := c.self, c.appID
	c.mu.Unlock()
	if h != nil {
		return h, nil
	}
	if time.Since(at) < noHookFor {
		return nil, nil
	}
	var list []webhook
	err := c.do(ctx, http.MethodGet, "/channels/"+ch+"/webhooks", nil, &list)
	if status(err) == http.StatusForbidden {
		c.mu.Lock()
		c.noHook[ch] = time.Now()
		c.mu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, w := range list {
		mine := app != "" && w.App == app || w.User != nil && w.User.ID == self
		// Type 1 is an incoming webhook, the kind that can post.
		if w.Type == 1 && w.Name == "say" && w.Token != "" && mine {
			h = &w
			break
		}
	}
	if h == nil && !create {
		return nil, nil
	}
	if h == nil {
		var w webhook
		err := c.do(ctx, http.MethodPost, "/channels/"+ch+"/webhooks", map[string]string{"name": "say"}, &w)
		if _, ok := errors.AsType[*apiError](err); ok {
			// No permission, or the channel has all the webhooks it
			// may: write as the bot.
			c.mu.Lock()
			c.noHook[ch] = time.Now()
			c.mu.Unlock()
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		h = &w
	}
	c.mu.Lock()
	c.hooks[ch] = h
	if c.hookIDs[ch] != h.ID {
		c.hookIDs[ch] = h.ID
		c.dirty = true
	}
	c.mu.Unlock()
	return h, nil
}

// forgetHook drops a channel's webhook after Discord lost it.
func (c *conn) forgetHook(ch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.hooks, ch)
}

// sent notes how a message went out, for editing it.
func (c *conn) sent(id, via string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sentBy) >= 10000 {
		clear(c.sentBy)
	}
	c.sentBy[id] = via
}

// botText is a message as the bot posts it: the sender's name in bold.
func botText(m bridge.Outgoing) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`)
	if m.Text == "" {
		return clip("**" + r.Replace(m.Sender) + "**") // files alone
	}
	return clip("**" + r.Replace(m.Sender) + "**: " + m.Text)
}

// clip cuts text to what Discord takes.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxText {
		return s
	}
	r := []rune(s)
	return string(r[:maxText-1]) + "…"
}

// banned are the words Discord refuses in webhook names.
var banned = regexp.MustCompile(`(?i)(d)(iscord)|(c)(lyde)`)

// username makes a name Discord takes for a webhook post: 1 to 80
// characters, without "discord" or "clyde" in it.
func username(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	name = banned.ReplaceAllString(name, "$1$3.$2$4")
	if name == "" {
		name = "someone"
	}
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	return name
}
