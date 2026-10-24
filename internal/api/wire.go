package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/vegard/say-v2/internal/core"
)

// The JSON shapes of protocol/openapi.yaml. Keep the two in step.

type wirePrincipal struct {
	ID          string             `json:"id"`
	Kind        core.PrincipalKind `json:"kind"`
	DisplayName string             `json:"display_name"`
	Admin       bool               `json:"admin,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
}

func toPrincipal(p core.Principal) wirePrincipal {
	return wirePrincipal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName, Admin: p.Admin, CreatedAt: p.CreatedAt.UTC()}
}

type wireRoom struct {
	ID        string          `json:"id"`
	Kind      core.RoomKind   `json:"kind"`
	Name      string          `json:"name"`
	Agents    wireAgentPolicy `json:"agents"`
	CreatedAt time.Time       `json:"created_at"`
}

// wireAgentPolicy shows the limits that apply in a room, the server's
// defaults included.
type wireAgentPolicy struct {
	Paused            bool `json:"paused"`
	MaxHopCount       int  `json:"max_hop_count"`
	TurnLimit         int  `json:"turn_limit"`
	TurnWindowSeconds int  `json:"turn_window_seconds"`
	RateLimit         int  `json:"rate_limit_per_minute"`
}

func (s *Server) toRoom(r core.Room) wireRoom {
	p := s.agentPolicy(r)
	return wireRoom{
		ID:   r.ID,
		Kind: r.Kind,
		Name: r.Name,
		Agents: wireAgentPolicy{
			Paused:            p.Paused,
			MaxHopCount:       p.MaxHops,
			TurnLimit:         p.TurnLimit,
			TurnWindowSeconds: int(p.TurnWindow / time.Second),
			RateLimit:         p.RateLimit,
		},
		CreatedAt: r.CreatedAt.UTC(),
	}
}

type wireMember struct {
	PrincipalID string    `json:"principal_id"`
	Role        core.Role `json:"role"`
	Scopes      []string  `json:"scopes"`
}

func toMember(m core.Membership) wireMember {
	scopes := m.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return wireMember{PrincipalID: m.PrincipalID, Role: m.Role, Scopes: scopes}
}

type wireCausation struct {
	CausedBy string `json:"caused_by,omitempty"`
	HopCount int    `json:"hop_count"`
}

type wirePart struct {
	Kind core.PartKind   `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type wireEvent struct {
	ID        string           `json:"id"`
	RoomID    string           `json:"room_id"`
	Seq       int64            `json:"seq"`
	Type      core.EventType   `json:"type"`
	SenderID  string           `json:"sender_id"`
	Causation wireCausation    `json:"causation"`
	Parts     []wirePart       `json:"parts"`
	Stream    core.StreamState `json:"stream,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

func toEvent(e core.Event) wireEvent {
	parts := make([]wirePart, len(e.Parts))
	for i, p := range e.Parts {
		parts[i] = wirePart{Kind: p.Kind, Data: p.Data}
	}
	return wireEvent{
		ID:        e.ID,
		RoomID:    e.RoomID,
		Seq:       e.Seq,
		Type:      e.Type,
		SenderID:  e.SenderID,
		Causation: wireCausation{CausedBy: e.Causation.CausedBy, HopCount: e.Causation.HopCount},
		Parts:     parts,
		Stream:    e.Stream,
		CreatedAt: e.CreatedAt.UTC(),
	}
}

func toEvents(es []core.Event) []wireEvent {
	out := make([]wireEvent, len(es))
	for i, e := range es {
		out[i] = toEvent(e)
	}
	return out
}

// sendMessage is the body of POST /rooms/{id}/messages and the payload of a
// WebSocket "send".
type sendMessage struct {
	Parts    []wirePart `json:"parts"`
	CausedBy string     `json:"caused_by,omitempty"`
}

// maxParts and maxPartBytes bound one message; files go through signed
// URLs, never inline.
const (
	maxParts     = 32
	maxPartBytes = 64 << 10
)

var partKinds = map[core.PartKind]bool{
	core.PartText: true, core.PartAttachment: true, core.PartToolCall: true,
	core.PartToolResult: true, core.PartCard: true,
}

// toParts validates the parts of a message. Every part's data is a JSON
// object; a text part has a non-empty "text" string.
func (m sendMessage) toParts() ([]core.Part, error) {
	if len(m.Parts) == 0 {
		return nil, errors.New("a message needs at least one part")
	}
	if len(m.Parts) > maxParts {
		return nil, fmt.Errorf("a message has at most %d parts", maxParts)
	}
	parts := make([]core.Part, len(m.Parts))
	for i, p := range m.Parts {
		if !partKinds[p.Kind] {
			return nil, fmt.Errorf("part %d: unknown kind %q", i, p.Kind)
		}
		if len(p.Data) > maxPartBytes {
			return nil, fmt.Errorf("part %d: data is larger than %d bytes", i, maxPartBytes)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(p.Data, &obj); err != nil || obj == nil {
			return nil, fmt.Errorf("part %d: data must be a JSON object", i)
		}
		if p.Kind == core.PartText {
			var text string
			if json.Unmarshal(obj["text"], &text) != nil || text == "" {
				return nil, fmt.Errorf("part %d: a text part needs a non-empty \"text\" string", i)
			}
		}
		parts[i] = core.Part{Kind: p.Kind, Data: p.Data}
	}
	return parts, nil
}

// apiError is the body of every error response:
// {"error": {"code": "...", "message": "..."}}.
type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	// RetryAfter, when set, is sent as the Retry-After header.
	RetryAfter time.Duration `json:"-"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

var (
	errUnauthorized = errorf(http.StatusUnauthorized, "unauthorized", "a valid access token is required")
	errRoomNotFound = errorf(http.StatusNotFound, "not_found", "no such room")
)

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = errorf(http.StatusInternalServerError, "internal", "internal error")
	}
	if ae.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	if ae.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(ae.RetryAfter/time.Second)))
	}
	writeJSON(w, ae.Status, map[string]*apiError{"error": ae})
}

// maxBody bounds request bodies.
const maxBody = 1 << 20

// readJSON decodes a JSON request body into v. It insists on a JSON
// content type, so a cross-site form post cannot reach a handler with the
// user's cookie (browsers preflight JSON requests).
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return errorf(http.StatusUnsupportedMediaType, "unsupported_media_type", "the body must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errorf(http.StatusBadRequest, "bad_request", "invalid JSON body: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errorf(http.StatusBadRequest, "bad_request", "the body must hold one JSON value")
	}
	return nil
}
