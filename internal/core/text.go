package core

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ScopeResponder on an agent's membership makes it a designated responder:
// a built-in agent with it answers every message in the room, not only
// those that mention it (docs/adr/0005-agent-safety.md).
const ScopeResponder = "responder"

// Text flattens a message to plain text: text parts as they are, one per
// line, tool calls as [used name] and other parts as [kind].
func (e Event) Text() string {
	var text []string
	for _, part := range e.Parts {
		switch part.Kind {
		case PartText:
			var d struct{ Text string }
			json.Unmarshal(part.Data, &d)
			text = append(text, d.Text)
		case PartToolCall:
			var c ToolCall
			if json.Unmarshal(part.Data, &c) != nil || c.Name == "" {
				text = append(text, "[tool-call]")
			} else if c.Status == ToolFailed {
				text = append(text, "[used "+c.Name+", failed]")
			} else {
				text = append(text, "[used "+c.Name+"]")
			}
		default:
			text = append(text, "["+string(part.Kind)+"]")
		}
	}
	return strings.Join(text, "\n")
}

// Prose is the message's text parts only, separated by blank lines.
func (e Event) Prose() string {
	var text []string
	for _, part := range e.Parts {
		if part.Kind != PartText {
			continue
		}
		var d struct{ Text string }
		json.Unmarshal(part.Data, &d)
		text = append(text, d.Text)
	}
	return strings.Join(text, "\n\n")
}

// Mentions reports whether text mentions name as @name, ignoring case. The
// mention must stand on its own: @Ada does not match @Adam, and an email
// address is not a mention.
func Mentions(text, name string) bool {
	if name == "" {
		return false
	}
	t, n := strings.ToLower(text), "@"+strings.ToLower(name)
	for i := 0; ; {
		j := strings.Index(t[i:], n)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(n)
		before, _ := utf8.DecodeLastRuneInString(t[:start])
		after, _ := utf8.DecodeRuneInString(t[end:])
		if (start == 0 || !wordRune(before)) && (end == len(t) || !wordRune(after)) {
			return true
		}
		i += j + 1
	}
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
