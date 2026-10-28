package slack

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/lum1n/say-server/internal/bridge"
)

// emoji are the Slack names of common emoji, for reactions. A name not
// here comes to say as :name:, and an emoji not here cannot be sent.
var emoji = map[string]string{
	"+1": "👍", "thumbsup": "👍", "-1": "👎", "thumbsdown": "👎",
	"heart": "❤️", "joy": "😂", "tada": "🎉", "eyes": "👀", "fire": "🔥",
	"white_check_mark": "✅", "heavy_check_mark": "✔️", "x": "❌", "pray": "🙏",
	"smile": "😄", "smiley": "😃", "grinning": "😀", "laughing": "😆", "slightly_smiling_face": "🙂",
	"wink": "😉", "blush": "😊", "heart_eyes": "😍", "open_mouth": "😮", "astonished": "😲",
	"cry": "😢", "sob": "😭", "rage": "😡", "thinking_face": "🤔", "raised_hands": "🙌",
	"clap": "👏", "100": "💯", "ok_hand": "👌", "wave": "👋", "rocket": "🚀",
	"muscle": "💪", "star": "⭐", "sparkles": "✨", "pizza": "🍕", "coffee": "☕",
	"see_no_evil": "🙈", "sweat_smile": "😅", "upside_down_face": "🙃", "party_popper": "🎉",
	"heavy_plus_sign": "➕", "question": "❓", "exclamation": "❗", "point_up": "☝️",
	"saluting_face": "🫡", "melting_face": "🫠", "skull": "💀", "handshake": "🤝",
}

// emojiNames are the Slack names of emoji, the first in emoji for each.
var emojiNames = func() map[string]string {
	out := map[string]string{}
	for _, n := range []string{"+1", "-1", "party_popper", "tada"} {
		out[emoji[n]] = n
	}
	for n, e := range emoji {
		if _, ok := out[e]; !ok {
			out[e] = n
		}
	}
	return out
}()

// reactionKey is a Slack reaction as say keys it: the emoji when it is
// known, else :name:. Skin tones are left off.
func reactionKey(name string) string {
	name, _, _ = strings.Cut(name, "::")
	if e, ok := emoji[name]; ok {
		return e
	}
	return ":" + name + ":"
}

// reactionName is a say key as Slack names it; false for an emoji it does
// not know.
func reactionName(key string) (string, bool) {
	if strings.HasPrefix(key, ":") && strings.HasSuffix(key, ":") && len(key) > 2 {
		return key[1 : len(key)-1], true
	}
	n, ok := emojiNames[key]
	if !ok {
		// Emoji come with and without the variation selector.
		n, ok = emojiNames[strings.TrimSuffix(key, "️")]
	}
	return n, ok
}

// receiveReaction hands a reaction to a message to the core. Slack gives
// reactions no id; a removal names the message and the emoji.
func (c *conn) receiveReaction(ctx context.Context, m message) error {
	if m.Item == nil || m.Item.Type != "message" || m.Item.Channel == "" || m.Item.Ts == "" || m.User == "" || m.Reaction == "" {
		return nil
	}
	c.mu.Lock()
	self := c.self
	c.mu.Unlock()
	return c.h.Receive(ctx, bridge.Incoming{
		Chat:     c.chat(ctx, m.Item.Channel),
		Author:   bridge.Author{ID: m.User, Name: c.userName(ctx, m.User), Self: m.User == self},
		Time:     tsTime(m.EventTs),
		Reaction: &bridge.IncomingReaction{Target: m.Item.Ts, Key: reactionKey(m.Reaction), Removed: m.Type == "reaction_removed"},
	})
}

// React adds the person's reaction to a message, or takes it back.
// Reacting twice, or taking back what is not there, is not an error.
func (c *conn) React(ctx context.Context, r bridge.Reaction) (string, error) {
	name, ok := reactionName(r.Key)
	if !ok {
		return "", bridge.Rejected(fmt.Errorf("slack: no Slack name for %s", r.Key))
	}
	method, done := "reactions.add", "already_reacted"
	if r.Removed {
		method, done = "reactions.remove", "no_reaction"
	}
	var resp response
	err := c.call(ctx, method, c.cfg.UserToken, url.Values{"channel": {r.Chat}, "timestamp": {r.Target}, "name": {name}}, &resp)
	if ae, ok := errors.AsType[*apiError](err); ok {
		if ae.Code == done {
			return "", nil
		}
		return "", bridge.Rejected(err)
	}
	return "", err
}
