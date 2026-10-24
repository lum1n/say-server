// Package bus fans out notifications between the parts of one node and, in
// cluster mode, between nodes. The store is the source of truth: the bus only
// says "room R has new events", and subscribers that miss a message catch up
// by reading from their cursor (docs/adr/0002-storage-and-scaling.md).
package bus

import "context"

// Handler receives one published message. It must not block for long.
type Handler func(subject string, data []byte)

type Bus interface {
	Publish(ctx context.Context, subject string, data []byte) error
	// Subscribe registers h for an exact subject. The returned function
	// removes the subscription.
	Subscribe(subject string, h Handler) (unsubscribe func(), err error)
	Close() error
}

// RoomSubject is where a room announces new events.
func RoomSubject(roomID string) string { return "room." + roomID }

// RoomStreamSubject carries a room's streaming messages as they change.
// Unlike RoomSubject it has a payload, the message as JSON, and a missed
// frame is not caught up: the stream's final edit is in the log.
func RoomStreamSubject(roomID string) string { return "room." + roomID + ".stream" }

// RoomsSubject is where a principal hears that its set of rooms, or a
// room's settings, changed.
func RoomsSubject(principalID string) string { return "principal." + principalID + ".rooms" }

// BuiltinAgentsSubject announces that a built-in agent was created or
// changed.
const BuiltinAgentsSubject = "builtin_agents"

// BridgesSubject announces that a bridge was created or changed.
const BridgesSubject = "bridges"

// BridgeAnswerSubject carries the owner's answer to a bridge's login
// prompt, as text, to the node running the bridge.
func BridgeAnswerSubject(bridgeID string) string { return "bridge." + bridgeID + ".answer" }

// RoomDeliverySubject announces that the delivery of a room's message to
// a bridge changed; the payload is the event id.
func RoomDeliverySubject(roomID string) string { return "room." + roomID + ".delivery" }
