// Package webhook is a relay bot over plain HTTP: other programs post
// messages to a room through the bridge's hook URL, and say posts the
// room's messages to a URL of theirs.
//
// Files travel in the JSON both ways, as "attachments": [{"name":
// "a.png", "mime": "image/png", "data": "<base64>"}]. say never fetches a
// URL a hook names: that would let anyone with the secret make the server
// reach into its own network.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/bridge"
	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/core"
	"github.com/lum1n/say-server/internal/store"
)

func init() {
	bridge.Register(bridge.Network{
		Name:         "webhook",
		Title:        "Webhooks",
		Modes:        []string{store.ModeBot},
		Fields:       fields,
		Capabilities: []connector.Capability{connector.CapEdits, connector.CapReplies, connector.CapReactions, connector.CapAttachments},
		Open:         open,
		Hook:         hook,
		HookSecret:   "token",
	})
}

var fields = []bridge.Field{
	{
		Key: "token", Label: "Incoming secret", Kind: "secret", Required: true,
		Help: "A long random string. Programs post JSON such as {\"chat\": \"alerts\", \"text\": \"…\", \"username\": \"Grafana\"} " +
			"to the bridge's hook URL with the header Authorization: Bearer <secret>, or ?token=<secret> in the URL. " +
			"Link a room to the chat name they use; without one, the chat is \"default\". " +
			"Files come as \"attachments\": [{\"name\": \"graph.png\", \"mime\": \"image/png\", \"data\": \"<base64>\"}], with or without text. " +
			"A reaction is {\"type\": \"reaction\", \"chat\": …, \"target\": \"<message id>\", \"key\": \"👍\", \"user_id\": …}, with \"removed\": true to take it back.",
	},
	{
		Key: "url", Label: "Outgoing URL", Kind: "text", Placeholder: "https://…",
		Help: "Where say posts the linked rooms' messages. Leave it empty to only receive.",
	},
	{
		Key: "format", Label: "Outgoing format", Kind: "text", Placeholder: "say",
		Help: "say (JSON with chat, sender, text, event_id and attachments as the hook takes them; edits and reactions too), slack (a Slack or Mattermost incoming webhook) " +
			"or discord (a Discord webhook).",
	},
	{
		Key: "signing_secret", Label: "Signing secret", Kind: "secret",
		Help: "If set, each outgoing request has the header X-Say-Signature: sha256=<hex HMAC-SHA256 of the body>.",
	},
}

type config struct {
	Token         string `json:"token"`
	URL           string `json:"url"`
	Format        string `json:"format"`
	SigningSecret string `json:"signing_secret"`
}

func parseConfig(raw json.RawMessage) (config, error) {
	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("webhook: bad config: %w", err)
	}
	switch c.Format {
	case "":
		c.Format = "say"
	case "say", "slack", "discord":
	default:
		return c, fmt.Errorf("webhook: the format is say, slack or discord, not %q", c.Format)
	}
	if c.URL != "" && !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
		return c, errors.New("webhook: the outgoing URL starts with https:// or http://")
	}
	return c, nil
}

// incoming is a message posted to the hook. Text may also come as
// content, as Discord webhooks name it. Type "reaction" makes it a
// reaction to the message Target instead.
type incoming struct {
	Type     string `json:"type"`
	Chat     string `json:"chat"`
	ID       string `json:"id"`
	Text     string `json:"text"`
	Content  string `json:"content"`
	Username string `json:"username"`
	UserID   string `json:"user_id"`
	ReplyTo  string `json:"reply_to"`
	Edit     bool   `json:"edit"`
	Time     string `json:"time"`
	Target   string `json:"target"`
	Key      string `json:"key"`
	Removed  bool   `json:"removed"`
	// Attachments are files sent with the message.
	Attachments []attachment `json:"attachments"`
}

// attachment is a file in a hook body or an outgoing request, its bytes
// in base64.
type attachment struct {
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Data string `json:"data"`
}

// maxAttachments bounds the files of one message, as say does.
const maxAttachments = 10

// hook reads a request to the hook URL.
func hook(b bridge.Info, r *http.Request) ([]bridge.Incoming, error) {
	c, err := parseConfig(b.Config)
	if err != nil {
		return nil, err
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		got = r.URL.Query().Get("token")
	}
	if c.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(c.Token)) != 1 {
		return nil, bridge.ErrUnauthorized
	}
	var in incoming
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return nil, errors.New("the body is not a JSON object")
	}
	chat := in.Chat
	if chat == "" {
		chat = r.URL.Query().Get("chat")
	}
	if chat == "" {
		chat = "default"
	}
	switch in.Type {
	case "", "message":
	case "reaction":
		return reaction(chat, in)
	default:
		return nil, fmt.Errorf("unknown type %q", in.Type)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		text = strings.TrimSpace(in.Content)
	}
	files, err := media(in.Attachments)
	if err != nil {
		return nil, err
	}
	if text == "" && (len(files) == 0 || in.Edit) {
		return nil, errors.New("text is empty")
	}
	if len(text) > 64<<10 {
		return nil, errors.New("text is longer than 64 KiB")
	}
	m := bridge.Incoming{
		Chat:    bridge.RemoteChat{ID: chat, Name: chat},
		ID:      in.ID,
		Author:  author(in),
		Text:    text,
		Edit:    in.Edit,
		ReplyTo: in.ReplyTo,
	}
	if !m.Edit {
		// An edit changes the text only.
		m.Media = files
	}
	if m.ID == "" {
		if m.Edit {
			return nil, errors.New("an edit needs the id of the message")
		}
		// Without an id, a repeat can't be told apart.
		m.ID = core.NewID()
	}
	if m.Time, err = sentAt(in); err != nil {
		return nil, err
	}
	return []bridge.Incoming{m}, nil
}

// media decodes the files of a hook body. say reads them from memory.
func media(as []attachment) ([]bridge.Media, error) {
	if len(as) > maxAttachments {
		return nil, fmt.Errorf("a message has at most %d attachments", maxAttachments)
	}
	var out []bridge.Media
	for i, a := range as {
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("attachment %d: data is not base64", i)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("attachment %d is empty", i)
		}
		out = append(out, bridge.Media{
			Mime: a.Mime, Name: a.Name, Size: int64(len(data)),
			Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		})
	}
	return out, nil
}

// reaction reads a reaction posted to the hook.
func reaction(chat string, in incoming) ([]bridge.Incoming, error) {
	if in.Target == "" || in.Key == "" {
		return nil, errors.New("a reaction needs a target and a key")
	}
	m := bridge.Incoming{
		Chat:     bridge.RemoteChat{ID: chat, Name: chat},
		ID:       in.ID,
		Author:   author(in),
		Reaction: &bridge.IncomingReaction{Target: in.Target, Key: in.Key, Removed: in.Removed},
	}
	var err error
	if m.Time, err = sentAt(in); err != nil {
		return nil, err
	}
	return []bridge.Incoming{m}, nil
}

func author(in incoming) bridge.Author {
	a := bridge.Author{ID: in.UserID, Name: in.Username}
	if a.Name == "" {
		a.Name = "Webhook"
	}
	if a.ID == "" {
		a.ID = a.Name
	}
	return a
}

func sentAt(in incoming) (time.Time, error) {
	if in.Time == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, in.Time)
	if err != nil {
		return time.Time{}, errors.New("time is not RFC 3339")
	}
	return t, nil
}

func open(b bridge.Info, h bridge.Host) (bridge.Conn, error) {
	c, err := parseConfig(b.Config)
	if err != nil {
		return nil, err
	}
	return &conn{h: h, cfg: c, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// conn sends to the outgoing URL. Messages come in through the hook,
// on any node, so Run only waits.
type conn struct {
	h    bridge.Host
	cfg  config
	http *http.Client
}

func (c *conn) Run(ctx context.Context) error {
	// There is nothing to connect to.
	c.h.Connected(ctx)
	<-ctx.Done()
	return ctx.Err()
}

func (c *conn) Send(ctx context.Context, m bridge.Outgoing) (string, error) {
	if c.cfg.URL == "" {
		return "", bridge.Rejected(errors.New("the bridge has no outgoing URL"))
	}
	var body any
	switch c.cfg.Format {
	case "slack":
		body = map[string]any{"text": "*" + m.Sender + "*: " + named(m), "username": m.Sender}
	case "discord":
		body = map[string]any{"content": named(m), "username": m.Sender, "allowed_mentions": map[string]any{"parse": []string{}}}
	default:
		files, err := attachments(ctx, m.Media)
		if err != nil {
			return "", err
		}
		msg := map[string]any{"type": "message", "chat": m.Chat, "event_id": m.EventID, "sender": m.Sender, "text": m.Text, "reply_to": m.ReplyTo}
		if len(files) > 0 {
			msg["attachments"] = files
		}
		body = msg
	}
	id, err := c.post(ctx, body)
	if id == "" && err == nil {
		id = m.EventID
	}
	return id, err
}

// named is the text of m with its files named, for the formats that
// carry no files.
func named(m bridge.Outgoing) string {
	text := m.Text
	for _, f := range m.Media {
		text = strings.TrimSpace(text + "\n" + core.Attachment{Mime: f.Mime, Name: f.Name}.Placeholder())
	}
	return text
}

// attachments reads a message's files into the say format.
func attachments(ctx context.Context, media []bridge.Media) ([]attachment, error) {
	var out []attachment
	for _, f := range media {
		r, err := f.Open(ctx)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, attachment{Name: f.Name, Mime: f.Mime, Data: base64.StdEncoding.EncodeToString(data)})
	}
	return out, nil
}

// Edit sends an edit in the say format; the others can't edit.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	if c.cfg.URL == "" || c.cfg.Format != "say" {
		return nil
	}
	_, err := c.post(ctx, map[string]any{"type": "edit", "chat": m.Chat, "event_id": m.EventID, "remote_id": remoteID, "sender": m.Sender, "text": m.Text})
	return err
}

// React sends a reaction in the say format; the others have none.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	if c.cfg.URL == "" || c.cfg.Format != "say" {
		return "", nil
	}
	_, err := c.post(ctx, map[string]any{"type": "reaction", "chat": r.Chat, "event_id": r.EventID, "target": r.Target, "key": r.Key, "removed": r.Removed})
	return "", err
}

// post sends body to the outgoing URL and returns the id the answer
// gives, if any.
func (c *conn) post(ctx context.Context, body any) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(data))
	if err != nil {
		return "", bridge.Rejected(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "say-webhook")
	if c.cfg.SigningSecret != "" {
		mac := hmac.New(sha256.New, []byte(c.cfg.SigningSecret))
		mac.Write(data)
		req.Header.Set("X-Say-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout:
		return "", fmt.Errorf("webhook: %s", resp.Status)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "", bridge.Rejected(fmt.Errorf("webhook: %s", resp.Status))
	case resp.StatusCode >= 300:
		return "", fmt.Errorf("webhook: %s", resp.Status)
	}
	var got struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(answer, &got) != nil || len(got.ID) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(got.ID, &s) != nil {
		s = string(got.ID) // a number
	}
	return s, nil
}
