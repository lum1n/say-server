// Package webhook is a relay bot over plain HTTP: other programs post
// messages to a room through the bridge's hook URL, and say posts the
// room's messages to a URL of theirs.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
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
		Capabilities: []connector.Capability{connector.CapEdits},
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
			"Link a room to the chat name they use; without one, the chat is \"default\".",
	},
	{
		Key: "url", Label: "Outgoing URL", Kind: "text", Placeholder: "https://…",
		Help: "Where say posts the linked rooms' messages. Leave it empty to only receive.",
	},
	{
		Key: "format", Label: "Outgoing format", Kind: "text", Placeholder: "say",
		Help: "say (JSON with chat, sender, text and event_id; edits too), slack (a Slack or Mattermost incoming webhook) " +
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
// content, as Discord webhooks name it.
type incoming struct {
	Chat     string `json:"chat"`
	ID       string `json:"id"`
	Text     string `json:"text"`
	Content  string `json:"content"`
	Username string `json:"username"`
	UserID   string `json:"user_id"`
	ReplyTo  string `json:"reply_to"`
	Edit     bool   `json:"edit"`
	Time     string `json:"time"`
}

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
	text := strings.TrimSpace(in.Text)
	if text == "" {
		text = strings.TrimSpace(in.Content)
	}
	if text == "" {
		return nil, errors.New("text is empty")
	}
	if len(text) > 64<<10 {
		return nil, errors.New("text is longer than 64 KiB")
	}
	chat := in.Chat
	if chat == "" {
		chat = r.URL.Query().Get("chat")
	}
	if chat == "" {
		chat = "default"
	}
	m := bridge.Incoming{
		Chat:    bridge.RemoteChat{ID: chat, Name: chat},
		ID:      in.ID,
		Author:  bridge.Author{ID: in.UserID, Name: in.Username},
		Text:    text,
		Edit:    in.Edit,
		ReplyTo: in.ReplyTo,
	}
	if m.ID == "" {
		if m.Edit {
			return nil, errors.New("an edit needs the id of the message")
		}
		// Without an id, a repeat can't be told apart.
		m.ID = core.NewID()
	}
	if m.Author.Name == "" {
		m.Author.Name = "Webhook"
	}
	if m.Author.ID == "" {
		m.Author.ID = m.Author.Name
	}
	if in.Time != "" {
		t, err := time.Parse(time.RFC3339, in.Time)
		if err != nil {
			return nil, errors.New("time is not RFC 3339")
		}
		m.Time = t
	}
	return []bridge.Incoming{m}, nil
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
		body = map[string]any{"text": "*" + m.Sender + "*: " + m.Text, "username": m.Sender}
	case "discord":
		body = map[string]any{"content": m.Text, "username": m.Sender, "allowed_mentions": map[string]any{"parse": []string{}}}
	default:
		body = map[string]any{"type": "message", "chat": m.Chat, "event_id": m.EventID, "sender": m.Sender, "text": m.Text, "reply_to": m.ReplyTo}
	}
	id, err := c.post(ctx, body)
	if id == "" && err == nil {
		id = m.EventID
	}
	return id, err
}

// Edit sends an edit in the say format; the others can't edit.
func (c *conn) Edit(ctx context.Context, m bridge.Outgoing, remoteID string) error {
	if c.cfg.URL == "" || c.cfg.Format != "say" {
		return nil
	}
	_, err := c.post(ctx, map[string]any{"type": "edit", "chat": m.Chat, "event_id": m.EventID, "remote_id": remoteID, "sender": m.Sender, "text": m.Text})
	return err
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
