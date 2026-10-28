// Package bridge connects say to other networks
// (docs/adr/0014-bridges.md).
//
// A bridge in account mode is a person's own account on a network: each of
// its chats becomes a room of the owner (a portal), people on the other
// side are ghosts, and what the owner writes in the room goes out as them.
// In bot mode an admin's relay bot mirrors existing rooms to remote
// channels.
//
// Every node runs a Runner. Each enabled bridge is held by one node at a
// time through a lease, and the holder runs the network connection and
// delivers the room log to it from a cursor per link. A deleted bridge is
// taken the same way, by the node that logs it out and removes it.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/lum1n/say-server/internal/connector"
	"github.com/lum1n/say-server/internal/core"
)

// Network is a kind of bridge: IRC, Matrix, Slack, …
type Network struct {
	Name  string // matrix, slack, …
	Title string // shown to people
	// Modes are the bridge modes the network supports.
	Modes []string
	// Fields describe the config, for forms and for keeping secrets.
	Fields       []Field
	Capabilities []connector.Capability
	// Open makes a connection for a bridge. It must not do I/O: Run does.
	Open func(b Info, h Host) (Conn, error)
	// Hook, if set, reads a request sent to the bridge's hook URL
	// (/api/v1/hooks/{bridge id}) and returns the messages in it. Any
	// node may call it, also one not running the bridge. An error
	// wrapping ErrUnauthorized is answered 401, others 400.
	Hook func(b Info, r *http.Request) ([]Incoming, error)
	// HookSecret, if set, names the secret Field that Hook checks, so
	// that it can be replaced with a new random one.
	HookSecret string
	// LogOut, if set, ends the session say holds on the network when the
	// bridge is deleted, so it no longer shows among the account's
	// devices or sessions. It runs on the node holding the bridge's lease,
	// with the connection stopped, and must succeed when there is no
	// session (never logged in, or already logged out).
	LogOut func(ctx context.Context, b Info) error
	// Idempotent says the network takes a repeated send of a message as
	// the same message (docs/adr/0015-sync.md). Its sends never expire: a
	// message waits for the network however long it is away. A send whose
	// outcome is unknown, or left sending by a node that stopped, is tried
	// again with backoff, and the messages after it wait in log order.
	Idempotent bool
}

// Field is one config setting of a network.
type Field struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Kind is text, secret, number or bool. Secrets are never read back.
	Kind        string `json:"kind"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	// Modes limits the field to some modes; empty is all.
	Modes []string `json:"modes,omitempty"`
	// Advanced fields are folded away in forms: few people need them.
	Advanced bool `json:"advanced,omitempty"`
}

// Info is what a connection knows of its bridge.
type Info struct {
	ID     string
	Mode   string
	Config json.RawMessage
	// State is what the connection last saved with Host.SaveState.
	State json.RawMessage
}

// Conn is a running connection to a network.
type Conn interface {
	// Run connects, logs in and receives until ctx ends, passing what
	// arrives to Host.Receive. A returned error is shown on the bridge and
	// Run is called again after a pause.
	Run(ctx context.Context) error
	// Send posts a message to a remote chat and returns its remote id.
	// An error the network gave for certain should wrap ErrRejected; any
	// other error leaves the outcome unknown, and it is never retried
	// unless the network is Idempotent.
	Send(ctx context.Context, m Outgoing) (remoteID string, err error)
}

// Editor is a Conn that can change a message it sent.
type Editor interface {
	Edit(ctx context.Context, m Outgoing, remoteID string) error
}

// Reactor is a Conn that can react to messages as the account. React
// returns the reaction's own remote id on networks that give it one. A
// key the network does not take, such as an emoji outside Telegram's set,
// should be an error wrapping ErrRejected: it is dropped and logged.
type Reactor interface {
	React(ctx context.Context, r Reaction) (remoteID string, err error)
}

// Reaction is the owner reacting on the network.
type Reaction struct {
	Chat    string // remote chat id
	EventID string // the reaction event
	// Target is the remote id of the message reacted to.
	Target string
	Key    string
	// Removed takes the reaction back. Added is then the remote id the
	// network gave the reaction when it was added, if it gave one.
	Removed bool
	Added   string
}

// IncomingReaction is someone on the network reacting to a message.
type IncomingReaction struct {
	// Target is the remote id of the message. A removal may leave it out
	// and name the reaction it takes back by the Incoming's ID instead.
	Target  string
	Key     string
	Removed bool
}

// ErrRejected marks a send the network refused, so it surely did not
// arrive.
var ErrRejected = errors.New("rejected")

// ErrPending is what Receive returns for the account's own message on an
// Idempotent network while a send to the chat is unfinished: the message
// may be that send's echo. The connection should receive it again later.
var ErrPending = errors.New("bridge: a send to this chat is unfinished")

// ErrUnauthorized marks a hook request without the bridge's secret.
var ErrUnauthorized = errors.New("unauthorized")

// Rejected wraps err as a refusal.
func Rejected(err error) error { return fmt.Errorf("%w: %w", ErrRejected, err) }

// Outgoing is a message to post on a network.
type Outgoing struct {
	Chat    string // remote chat id
	EventID string
	// Text is the message's prose. Parts are all of its content, for a
	// network that carries more than text.
	Text  string
	Parts []core.Part
	// Sender is the display name of who wrote it. In account mode it is
	// always the owner, who the message goes out as; a bot shows it.
	Sender string
	// ReplyTo is the remote id of the message this one answers, if known.
	ReplyTo string
	// Media are the message's files, on a network with
	// connector.CapAttachments; elsewhere Text names them instead. A
	// message may have media and no text.
	Media []Media
}

// Media is a file of a message, going out to a network or coming in.
type Media struct {
	Mime string
	Name string
	// Size, Width and Height are 0 when not known; Width and Height are
	// an image's size in pixels.
	Size          int64
	Width, Height int
	// Open reads the file. Each call reads it from the start.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// Incoming is a message that arrived from a network.
type Incoming struct {
	Chat   RemoteChat
	ID     string // remote message id, unique in the chat
	Author Author
	Text   string
	// Parts, if set, are the message's content instead of Text.
	Parts []core.Part
	// Media are files sent with the message. Each is downloaded and kept
	// by say as it takes the message in; one it can't keep is named in
	// the text instead.
	Media []Media
	// Time is when it was sent; zero is now.
	Time time.Time
	// Edit says that the message ID now reads Text, or Parts.
	Edit bool
	// ReplyTo is the remote id of the message this one answers.
	ReplyTo string
	// Reaction, when set, makes this a reaction rather than a message.
	// ID is then the reaction's own remote id, if it has one.
	Reaction *IncomingReaction
	// Quote is the first line of the message ReplyTo, if the network sends
	// it along. It leads the text as a "> " line when say has not seen
	// that message, so the reply still makes sense.
	Quote string
}

// content returns the message's parts.
func (m Incoming) content() []core.Part {
	if len(m.Parts) > 0 {
		return m.Parts
	}
	return []core.Part{core.TextPart(m.Text)}
}

// RemoteChat is a conversation on the network.
type RemoteChat struct {
	ID   string
	Name string
	// Direct is a one-to-one chat; others become group rooms.
	Direct bool
}

// Author is who wrote an incoming message. Self is the account's own
// user, writing from another client.
type Author struct {
	ID   string
	Name string
	Self bool
}

// Prompt is a login step only the owner can do: type a code or a
// password, or scan a QR code.
type Prompt struct {
	Kind string `json:"kind"` // code, password, text, qr
	Text string `json:"text"`
	// Data is what to show, such as the QR code's contents.
	Data string `json:"data,omitempty"`
}

// Host is the core as a connection sees it.
type Host interface {
	// Receive takes in a message. Repeats of a remote id are ignored.
	Receive(ctx context.Context, m Incoming) error
	// Ask shows the owner a prompt and waits for the answer.
	Ask(ctx context.Context, p Prompt) (string, error)
	// SaveState keeps state for the next Run, also on another node.
	SaveState(ctx context.Context, state json.RawMessage) error
	// Chats are the remote chats linked to rooms.
	Chats(ctx context.Context) ([]string, error)
	// Connected says the connection is logged in and receiving: delivery
	// starts and the bridge shows as live, until Run returns.
	Connected(ctx context.Context)
}

var networks = map[string]Network{}

// Register adds a network; drivers call it from init.
func Register(n Network) {
	if _, dup := networks[n.Name]; dup {
		panic("bridge: network registered twice: " + n.Name)
	}
	networks[n.Name] = n
}

// Lookup returns a registered network.
func Lookup(name string) (Network, bool) {
	n, ok := networks[name]
	return n, ok
}

// Networks returns the registered networks by name.
func Networks() []Network {
	out := make([]Network, 0, len(networks))
	for _, n := range networks {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Supports reports whether the network runs in mode.
func (n Network) Supports(mode string) bool { return slices.Contains(n.Modes, mode) }

// FieldsFor returns the fields that apply in mode.
func (n Network) FieldsFor(mode string) []Field {
	var out []Field
	for _, f := range n.Fields {
		if len(f.Modes) == 0 || slices.Contains(f.Modes, mode) {
			out = append(out, f)
		}
	}
	return out
}

// MergeConfig applies patch to config for mode: keys of known fields are
// set, an empty string clears a field, and a secret left out keeps its
// value. It checks required fields and kinds.
func (n Network) MergeConfig(mode string, config, patch json.RawMessage) (json.RawMessage, error) {
	cur := map[string]any{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cur); err != nil {
			return nil, err
		}
	}
	in := map[string]any{}
	if len(patch) > 0 {
		if err := json.Unmarshal(patch, &in); err != nil {
			return nil, errors.New("config is not a JSON object")
		}
	}
	fields := n.FieldsFor(mode)
	known := map[string]Field{}
	for _, f := range fields {
		known[f.Key] = f
	}
	for k, v := range in {
		f, ok := known[k]
		if !ok {
			return nil, fmt.Errorf("unknown setting %q", k)
		}
		switch f.Kind {
		case "bool":
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("%s is true or false", f.Label)
			}
		case "number":
			if _, ok := v.(float64); !ok {
				return nil, fmt.Errorf("%s is a number", f.Label)
			}
		default:
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s is text", f.Label)
			}
			if len(s) > 4096 {
				return nil, fmt.Errorf("%s is too long", f.Label)
			}
			if s == "" {
				delete(cur, k)
				continue
			}
		}
		cur[k] = v
	}
	for _, f := range fields {
		if _, ok := cur[f.Key]; f.Required && !ok {
			return nil, fmt.Errorf("%s is required", f.Label)
		}
	}
	return json.Marshal(cur)
}

// PublicConfig is config without its secrets, and the secrets that are
// set.
func (n Network) PublicConfig(mode string, config json.RawMessage) (map[string]any, []string) {
	cur := map[string]any{}
	json.Unmarshal(config, &cur)
	var set []string
	for _, f := range n.Fields {
		if f.Kind != "secret" {
			continue
		}
		if _, ok := cur[f.Key]; ok {
			set = append(set, f.Key)
			delete(cur, f.Key)
		}
	}
	return cur, set
}
