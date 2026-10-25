package core

import "time"

// Trigger says why a message asks an agent for an answer
// (docs/adr/0005-agent-safety.md). Built-in agents answer triggers, and
// bots are told them with each event (docs/adr/0011-bot-api.md).
type Trigger string

const (
	TriggerDM        Trigger = "dm"
	TriggerResponder Trigger = "responder"
	TriggerMention   Trigger = "mention"
)

// MaxTriggerAge is the oldest message that still triggers an agent, so an
// agent that was down or just joined does not answer stale questions.
const MaxTriggerAge = 5 * time.Minute

// LookBack is how many events before joining an agent first reads, so a
// question asked just before it was added still gets an answer.
const LookBack = 20

// Triggers returns why msg, which became complete at at, asks agent for an
// answer, or "" when it does not. Only a recent message from someone else
// triggers, never one in a room where the agent observes; then it does in
// a DM, for a designated responder, or when it mentions the agent.
func Triggers(agent Principal, room Room, m Membership, msg Event, at, now time.Time) Trigger {
	if msg.SenderID == agent.ID || now.Sub(at) > MaxTriggerAge || m.Role == RoleObserver {
		return ""
	}
	if room.Kind == RoomDM {
		return TriggerDM
	}
	for _, s := range m.Scopes {
		if s == ScopeResponder {
			return TriggerResponder
		}
	}
	if Mentions(msg.Text(), agent.DisplayName) {
		return TriggerMention
	}
	return ""
}
