package say

import "github.com/lum1n/say-server/internal/client"

// The client of the say API lives in internal/client, so that the say
// bridge network can use it too (docs/adr/0015-sync.md); these are its
// names for programs that embed say.
type (
	// Client calls the say API as one principal, on an embedded node or a
	// remote server. The types mirror protocol/openapi.yaml.
	Client      = client.Client
	Principal   = client.Principal
	Room        = client.Room
	AgentPolicy = client.AgentPolicy
	AgentPatch  = client.AgentPatch
	Member      = client.Member
	Event       = client.Event
	Part        = client.Part
	Error       = client.Error
	EventsQuery = client.EventsQuery
	Device      = client.Device
	Pairing     = client.Pairing
	Token       = client.Token
	Push        = client.Push
	Realtime    = client.Realtime
)

// Client returns a client of the node that acts with token.
func (n *Node) Client(token string) *Client {
	return &Client{BaseURL: BaseURL, Token: token, HTTP: n.client}
}

// NewClient returns a client of a remote server.
func NewClient(baseURL, token string) *Client { return client.New(baseURL, token) }

// TextPart makes a text part.
func TextPart(text string) Part { return client.TextPart(text) }
