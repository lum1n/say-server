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
// delivers the room log to it from a cursor per link.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/vegard/say-v2/internal/connector"
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
	// other error leaves the outcome unknown, and it is never retried.
	Send(ctx context.Context, m Outgoing) (remoteID string, err error)
}

// Editor is a Conn that can change a message it sent.
type Editor interface {
	Edit(ctx context.Context, m Outgoing, remoteID string) error
}

// ErrRejected marks a send the network refused, so it surely did not
// arrive.
var ErrRejected = errors.New("rejected")

// ErrUnauthorized marks a hook request without the bridge's secret.
var ErrUnauthorized = errors.New("unauthorized")

// Rejected wraps err as a refusal.
func Rejected(err error) error { return fmt.Errorf("%w: %w", ErrRejected, err) }

// Outgoing is a message to post on a network.
type Outgoing struct {
	Chat    string // remote chat id
	EventID string
	Text    string
	// Sender is the display name of who wrote it. In account mode it is
	// always the owner, who the message goes out as; a bot shows it.
	Sender string
	// ReplyTo is the remote id of the message this one answers, if known.
	ReplyTo string
}

// Incoming is a message that arrived from a network.
type Incoming struct {
	Chat   RemoteChat
	ID     string // remote message id, unique in the chat
	Author Author
	Text   string
	// Time is when it was sent; zero is now.
	Time time.Time
	// Edit says that the message ID now reads Text.
	Edit bool
	// ReplyTo is the remote id of the message this one answers.
	ReplyTo string
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
