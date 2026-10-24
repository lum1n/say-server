// Package connector defines what bridges, bots and agents declare when they
// attach to the core. All of them speak one protocol: they dial out to the
// hub over WebSocket, and first-party connectors use the same protocol over an
// in-process loopback (docs/adr/0003-connector-protocol.md).
package connector

import "slices"

// Capability is a feature a connector supports. The core degrades features
// a connector lacks and shows a "bridged" fallback in native rooms.
type Capability string

const (
	CapEdits       Capability = "edits"
	CapReactions   Capability = "reactions"
	CapThreads     Capability = "threads"
	CapTyping      Capability = "typing"
	CapReceipts    Capability = "receipts"
	CapAttachments Capability = "attachments"
	CapStreaming   Capability = "streaming"
)

// Scope is a permission on a connector credential.
type Scope string

const (
	ScopeAgent  Scope = "agent"
	ScopePuppet Scope = "puppet" // post as ghost principals; bridges only
)

// Reachability is shown to users. Sending to an offline connector fails with
// 409 instead of queueing silently.
type Reachability string

const (
	Live     Reachability = "live"
	Offline  Reachability = "offline"
	Unlinked Reachability = "unlinked"
)

// Descriptor is what a connector announces when it connects.
type Descriptor struct {
	Name         string
	Version      string
	Capabilities []Capability
}

func (d Descriptor) Has(c Capability) bool {
	return slices.Contains(d.Capabilities, c)
}
