// Package core holds the domain model: principals, rooms, memberships and
// events. Every sender, human or not, goes through the same types.
package core

import "time"

// PrincipalKind says what sort of participant a principal is.
type PrincipalKind string

const (
	PrincipalHuman  PrincipalKind = "human"
	PrincipalAgent  PrincipalKind = "agent"
	PrincipalGuest  PrincipalKind = "guest"
	PrincipalGhost  PrincipalKind = "ghost" // a remote user seen through a bridge
	PrincipalSystem PrincipalKind = "system"
)

// Automated reports whether events from this kind count as an automated hop
// in the causation chain.
func (k PrincipalKind) Automated() bool {
	return k == PrincipalAgent || k == PrincipalSystem
}

type Principal struct {
	ID          string
	Kind        PrincipalKind
	DisplayName string
	// Admin principals manage the instance: they create users and agents.
	Admin     bool
	CreatedAt time.Time
}

type RoomKind string

const (
	RoomDM           RoomKind = "dm"
	RoomGroup        RoomKind = "group"
	RoomChannel      RoomKind = "channel"
	RoomWidgetThread RoomKind = "widget-thread"
)

type Room struct {
	ID        string
	Kind      RoomKind
	Name      string
	Agents    AgentPolicy
	CreatedAt time.Time
}

// AgentPolicy is a room's limits on automated senders
// (docs/adr/0005-agent-safety.md). A zero limit means the server default.
type AgentPolicy struct {
	// Paused rejects every automated send in the room.
	Paused bool
	// MaxHops is the longest automated causation chain in the room.
	MaxHops int
	// TurnLimit is how many automated messages, from all senders together,
	// the room takes in any TurnWindow.
	TurnLimit  int
	TurnWindow time.Duration
	// RateLimit is how many messages one automated sender may send to the
	// room per minute.
	RateLimit int
}

type Role string

const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleMember   Role = "member"
	RoleObserver Role = "observer"
)

type Membership struct {
	RoomID      string
	PrincipalID string
	Role        Role
	Scopes      []string
}

type EventType string

const (
	EventMessage  EventType = "message"
	EventEdit     EventType = "edit"
	EventReaction EventType = "reaction"
	EventTyping   EventType = "typing"
	EventReceipt  EventType = "receipt"

	// System events record who changed the room and when. They are logged
	// like any other event so the room's history shows them.
	EventAgentSettings EventType = "agent-settings"
	EventHandoff       EventType = "handoff"
)

// Event is one entry in a room's log. Seq is allocated by the store and
// increases monotonically per room; (RoomID, Seq) identifies the event.
type Event struct {
	ID        string
	RoomID    string
	Seq       int64
	Type      EventType
	SenderID  string
	Causation Causation
	// Parts are a message's content, kept up to date as it streams and is
	// edited, or the whole new content of an edit.
	Parts []Part
	// Stream is a message's state. On an edit it is StreamFinal when the
	// edit ended the message's stream, and empty otherwise.
	Stream StreamState
	// Edits is the message an EventEdit changes.
	Edits string
	// EditedAt is when a final message was last edited; zero if never.
	EditedAt time.Time
	// ReplyTo is the message in the same room that a message answers,
	// shown as a quote. It is not its cause: Causation tracks automated
	// chains, and an agent's answer is a reply only when it says so.
	ReplyTo string
	// IdempotencyKey is the key the sender sent the event with, if any.
	IdempotencyKey string
	Settings       *AgentSettingsChange // for EventAgentSettings
	Handoff        *Handoff             // for EventHandoff
	CreatedAt      time.Time
}

// StreamState says whether a message is complete. A streaming message's
// parts change in place, unlogged, until an edit event makes it final
// (docs/adr/0010-streaming-and-edits.md).
type StreamState string

const (
	StreamStreaming StreamState = "streaming"
	StreamFinal     StreamState = "final"
)

type PartKind string

const (
	PartText       PartKind = "text"
	PartAttachment PartKind = "attachment"
	PartToolCall   PartKind = "tool-call"
	PartToolResult PartKind = "tool-result"
	PartCard       PartKind = "card"
)

// Part is one typed piece of a message. Data holds the kind-specific JSON
// body as defined in protocol/.
type Part struct {
	Kind PartKind
	Data []byte
}
