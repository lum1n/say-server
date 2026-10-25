package irc

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ergochat/irc-go/ircfmt"
	"github.com/ergochat/irc-go/ircmsg"

	"github.com/lum1n/say-v2/internal/bridge"
)

// incoming turns a PRIVMSG into a message for the host; false skips it.
func (s *session) incoming(it item) (bridge.Incoming, bool) {
	m := it.msg
	if m.Command != "PRIVMSG" || len(m.Params) < 2 {
		return bridge.Incoming{}, false
	}
	nick := m.Nick()
	if nick == "" {
		return bridge.Incoming{}, false
	}
	self := s.isSelf(m)
	if it.backfill && self && !s.echo {
		// Without echoes Send could not learn these ids: what say sent
		// would come back as new messages.
		return bridge.Incoming{}, false
	}
	text, ok := render(nick, m.Params[1])
	if !ok {
		return bridge.Incoming{}, false
	}
	target := strings.TrimLeft(m.Params[0], s.statusmsg)
	var chat bridge.RemoteChat
	switch {
	case s.isChannel(target):
		chat = bridge.RemoteChat{ID: fold(target), Name: target}
	case self:
		if s.isMe(target) {
			return bridge.Incoming{}, false
		}
		chat = bridge.RemoteChat{ID: fold(target), Name: target, Direct: true}
	default:
		chat = bridge.RemoteChat{ID: fold(nick), Name: nick, Direct: true}
	}
	at := serverTime(m)
	if it.backfill && !at.IsZero() && time.Since(at) > backfillAge {
		return bridge.Incoming{}, false
	}
	in := bridge.Incoming{
		Chat:   chat,
		ID:     messageID(m),
		Author: bridge.Author{ID: fold(nick), Name: nick, Self: self},
		Text:   text,
		Time:   at,
	}
	for _, tag := range []string{"+draft/reply", "+reply"} {
		if ok, v := m.GetTag(tag); ok && v != "" {
			in.ReplyTo = v
			break
		}
	}
	return in, true
}

// render makes a line plain text: formatting codes go, a /me becomes
// "* nick text", and other CTCP requests are skipped.
func render(nick, text string) (string, bool) {
	if body, ok := strings.CutPrefix(text, "\x01"); ok {
		body = strings.TrimSuffix(body, "\x01")
		action, ok := strings.CutPrefix(body, "ACTION")
		if !ok || (action != "" && action[0] != ' ') {
			return "", false
		}
		text = "* " + nick + action
	}
	text = strings.TrimSpace(ircfmt.Strip(text))
	return text, text != ""
}

// messageID is a line's msgid, or a hash of what makes it unique: when
// it was sent, by whom, to where, and what it says.
func messageID(m ircmsg.Message) string {
	if ok, id := m.GetTag("msgid"); ok && id != "" {
		return id
	}
	_, at := m.GetTag("time")
	if at == "" {
		at = time.Now().UTC().Format(time.RFC3339Nano)
	}
	var target, text string
	if len(m.Params) >= 2 {
		target, text = m.Params[0], m.Params[1]
	}
	sum := sha256.Sum256([]byte(at + "\x00" + fold(m.Nick()) + "\x00" + fold(target) + "\x00" + text))
	return "h:" + hex.EncodeToString(sum[:12])
}

// serverTime is the line's server-time tag; zero if it has none.
func serverTime(m ircmsg.Message) time.Time {
	ok, v := m.GetTag("time")
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// fold makes a nick or channel name comparable. IRC names are case
// insensitive; ASCII folding is what most servers use.
func fold(s string) string { return strings.ToLower(s) }

// split breaks text into lines of at most max bytes: at newlines, then at
// the last space that leaves a line at least half full, else at a rune
// boundary. Empty lines are dropped, as IRC cannot send them.
func split(text string, max int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		for len(line) > max {
			cut := max
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			if sp := strings.LastIndexByte(line[:cut], ' '); sp >= max/2 {
				cut = sp
			}
			out = append(out, line[:cut])
			line = strings.TrimLeft(line[cut:], " ")
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
