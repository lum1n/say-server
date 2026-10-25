package core

import "fmt"

// AgentSettingsChange is the body of an EventAgentSettings event: a change
// to a room's agent settings (docs/adr/0005-agent-safety.md). Only the
// fields that changed are set. The event's sender is who made the change.
type AgentSettingsChange struct {
	Paused      *bool
	MaxHopCount *int
}

// Handoff is the body of an EventHandoff event: who handles a conversation,
// such as a widget thread passing from an agent to a human. From is empty
// when nobody handled it before. The event's sender is who made the change;
// when that is To, they took over themselves.
type Handoff struct {
	From string
	To   string
}

// Describe renders a system event as one line for the room log, such as
// "Ada paused agents" or "Bob took over from Support bot". name maps a
// principal id to a display name. It returns "" for other event types.
func Describe(e Event, name func(principalID string) string) string {
	actor := name(e.SenderID)
	switch e.Type {
	case EventAgentSettings:
		s := e.Settings
		if s == nil {
			return ""
		}
		var line string
		if s.Paused != nil {
			if *s.Paused {
				line = actor + " paused agents"
			} else {
				line = actor + " resumed agents"
			}
		}
		if s.MaxHopCount != nil {
			if line == "" {
				line = actor + " set"
			} else {
				line += " and set"
			}
			line += fmt.Sprintf(" the agent hop limit to %d", *s.MaxHopCount)
		}
		return line
	case EventHandoff:
		h := e.Handoff
		if h == nil {
			return ""
		}
		other := h.From != "" && h.From != e.SenderID
		switch {
		case h.To == e.SenderID && other:
			return actor + " took over from " + name(h.From)
		case h.To == e.SenderID:
			return actor + " took over"
		case other:
			return actor + " passed the conversation from " + name(h.From) + " to " + name(h.To)
		default:
			return actor + " handed over to " + name(h.To)
		}
	}
	return ""
}
