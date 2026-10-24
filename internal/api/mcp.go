package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vegard/say-v2/internal/core"
	"github.com/vegard/say-v2/internal/store"
)

// The MCP server at /mcp lets an agent such as Claude Code take part in
// rooms with its agent token (docs/adr/0008-mcp-server.md). It is stateless
// streamable HTTP, so any node of a cluster can serve any request, and it
// only reads and writes through the same paths as the REST API.

// maxMCPWait bounds one wait_for_messages call. It stays under the usual
// MCP client tool timeouts.
const maxMCPWait = 120 * time.Second

func (s *Server) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "say", Version: s.version}, &mcp.ServerOptions{
		Instructions: "say is a chat server. You are an agent principal in it. " +
			"list_rooms shows the rooms you are in; read a room before you reply. " +
			"Pass the id of the message you answer as reply_to: replies between agents " +
			"are limited by a hop count, and a send past the limit fails. " +
			"Rooms also limit how often agents may send, and people can pause agents in a room; " +
			"when a send is refused, do not retry it in a loop. " +
			"wait_for_messages blocks until someone else writes in a room.",
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_rooms",
		Description: "List the rooms you are a member of, and who you are.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.mcpListRooms)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "read",
		Description: "Read messages in a room, oldest first. Without after or before it returns the latest messages. " +
			"Use the returned cursor as after in wait_for_messages.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.mcpRead)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "send",
		Description: "Send a text message to a room. Set reply_to to the id of the message you are answering.",
	}, s.mcpSend)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "wait_for_messages",
		Description: "Wait until someone else sends a message in a room after cursor after, then return the new messages. " +
			"Returns an empty list when timeout_seconds pass first; call it again with the returned cursor.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.mcpWait)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_room",
		Description: "Create a room that you own. Kind is group (default) or channel.",
	}, s.mcpCreateRoom)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	// Bearer only: the cookie never authorizes /mcp, so a web page cannot
	// drive it with a signed-in user's session.
	return auth.RequireBearerToken(s.verifyMCPToken, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)
}

func (s *Server) verifyMCPToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	me, err := s.store.PrincipalByToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil, auth.ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	// Loop safety counts hops for automated senders only, so a person's
	// token must not let a model write as that person.
	if me.Kind != core.PrincipalAgent {
		return nil, errors.Join(auth.ErrInvalidToken, errors.New("only agent tokens may use /mcp"))
	}
	return &auth.TokenInfo{UserID: me.ID, Extra: map[string]any{"principal": me}}, nil
}

func mcpCaller(req *mcp.CallToolRequest) core.Principal {
	me, _ := req.Extra.TokenInfo.Extra["principal"].(core.Principal)
	return me
}

// mcpErr turns an API error into the message a model sees.
func mcpErr(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return errors.New(ae.Message)
	}
	return err
}

type mcpRoom struct {
	ID   string        `json:"id"`
	Kind core.RoomKind `json:"kind"`
	Name string        `json:"name"`
	// AgentsPaused rooms refuse your messages until a person unpauses them.
	AgentsPaused bool `json:"agents_paused,omitempty"`
	MaxHopCount  int  `json:"max_hop_count"`
}

func (s *Server) mcpRoom(r core.Room) mcpRoom {
	p := s.AgentPolicy(r)
	return mcpRoom{ID: r.ID, Kind: r.Kind, Name: r.Name, AgentsPaused: p.Paused, MaxHopCount: p.MaxHops}
}

type mcpMessage struct {
	ID         string    `json:"id"`
	Seq        int64     `json:"seq"`
	SenderID   string    `json:"sender_id"`
	SenderName string    `json:"sender_name"`
	SenderKind string    `json:"sender_kind"`
	Text       string    `json:"text"`
	ReplyTo    string    `json:"reply_to,omitempty"`
	HopCount   int       `json:"hop_count"`
	SentAt     time.Time `json:"sent_at"`
}

type mcpListRoomsOut struct {
	You   mcpSelf   `json:"you"`
	Rooms []mcpRoom `json:"rooms"`
}

type mcpSelf struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) mcpListRooms(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcpListRoomsOut, error) {
	me := mcpCaller(req)
	rooms, err := s.store.RoomsFor(ctx, me.ID)
	if err != nil {
		return nil, mcpListRoomsOut{}, err
	}
	out := mcpListRoomsOut{You: mcpSelf{ID: me.ID, Name: me.DisplayName}, Rooms: make([]mcpRoom, len(rooms))}
	for i, r := range rooms {
		out.Rooms[i] = s.mcpRoom(r)
	}
	return nil, out, nil
}

type mcpReadIn struct {
	RoomID string `json:"room_id" jsonschema:"the room to read"`
	After  int64  `json:"after,omitempty" jsonschema:"return messages with seq greater than this"`
	Before int64  `json:"before,omitempty" jsonschema:"return the messages just before this seq"`
	Limit  int    `json:"limit,omitempty" jsonschema:"at most this many messages (default 30, max 200)"`
}

type mcpMessagesOut struct {
	Messages []mcpMessage `json:"messages"`
	// Cursor is the highest seq the call has seen; pass it as after.
	Cursor int64 `json:"cursor"`
}

func (s *Server) mcpRead(ctx context.Context, req *mcp.CallToolRequest, in mcpReadIn) (*mcp.CallToolResult, mcpMessagesOut, error) {
	me := mcpCaller(req)
	if _, err := s.member(ctx, me, in.RoomID); err != nil {
		return nil, mcpMessagesOut{}, mcpErr(err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 200)
	var events []core.Event
	var err error
	switch {
	case in.After > 0:
		events, err = s.store.EventsAfter(ctx, in.RoomID, in.After, limit)
	case in.Before > 0:
		events, err = s.store.EventsBefore(ctx, in.RoomID, in.Before, limit)
	default:
		events, err = s.store.EventsBefore(ctx, in.RoomID, math.MaxInt64, limit)
	}
	if err != nil {
		return nil, mcpMessagesOut{}, err
	}
	out := mcpMessagesOut{Cursor: in.After}
	if len(events) > 0 {
		out.Cursor = max(out.Cursor, events[len(events)-1].Seq)
	}
	out.Messages, err = s.mcpMessages(ctx, events)
	return nil, out, err
}

type mcpSendIn struct {
	RoomID         string `json:"room_id" jsonschema:"the room to send to"`
	Text           string `json:"text" jsonschema:"the message text"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"id of the message this answers"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"repeat a key to retry a send without sending it twice"`
}

func (s *Server) mcpSend(ctx context.Context, req *mcp.CallToolRequest, in mcpSendIn) (*mcp.CallToolResult, mcpMessage, error) {
	me := mcpCaller(req)
	if strings.TrimSpace(in.Text) == "" {
		return nil, mcpMessage{}, errors.New("text is empty")
	}
	data, _ := json.Marshal(map[string]string{"text": in.Text})
	msg := sendMessage{Parts: []wirePart{{Kind: core.PartText, Data: data}}, CausedBy: in.ReplyTo}
	e, _, err := s.send(ctx, me, in.RoomID, msg, in.IdempotencyKey)
	if err != nil {
		return nil, mcpMessage{}, mcpErr(err)
	}
	ms, err := s.mcpMessages(ctx, []core.Event{e})
	if err != nil {
		return nil, mcpMessage{}, err
	}
	return nil, ms[0], nil
}

type mcpWaitIn struct {
	RoomID         string `json:"room_id" jsonschema:"the room to watch"`
	After          int64  `json:"after" jsonschema:"the cursor from read or an earlier wait_for_messages"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait (default 60, max 120)"`
}

// mcpWait returns the events after the cursor once one of them is from
// someone else. The caller's own messages are included for context but do
// not end the wait, so an agent does not wake up to its own reply.
func (s *Server) mcpWait(ctx context.Context, req *mcp.CallToolRequest, in mcpWaitIn) (*mcp.CallToolResult, mcpMessagesOut, error) {
	me := mcpCaller(req)
	if _, err := s.member(ctx, me, in.RoomID); err != nil {
		return nil, mcpMessagesOut{}, mcpErr(err)
	}
	wait := time.Duration(in.TimeoutSeconds) * time.Second
	if wait <= 0 {
		wait = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, min(wait, maxMCPWait))
	defer cancel()
	var events []core.Event
	errOther := errors.New("got a message from someone else")
	err := s.follow(ctx, in.RoomID, in.After, func(e core.Event) error {
		events = append(events, e)
		if e.SenderID != me.ID && e.Type == core.EventMessage {
			return errOther
		}
		if len(events) >= maxPage {
			return errOther
		}
		return nil
	})
	if !errors.Is(err, errOther) && !errors.Is(err, context.DeadlineExceeded) {
		return nil, mcpMessagesOut{}, err
	}
	out := mcpMessagesOut{Cursor: in.After}
	if len(events) > 0 {
		out.Cursor = events[len(events)-1].Seq
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// Own messages alone are not news, but the cursor moves past them.
		events = nil
	}
	out.Messages, err = s.mcpMessages(context.WithoutCancel(ctx), events)
	return nil, out, err
}

type mcpCreateRoomIn struct {
	Name string `json:"name" jsonschema:"the room name"`
	Kind string `json:"kind,omitempty" jsonschema:"group or channel"`
}

func (s *Server) mcpCreateRoom(ctx context.Context, req *mcp.CallToolRequest, in mcpCreateRoomIn) (*mcp.CallToolResult, mcpRoom, error) {
	me := mcpCaller(req)
	kind := core.RoomKind(in.Kind)
	if kind == "" {
		kind = core.RoomGroup
	}
	room, err := s.newRoom(ctx, me, kind, in.Name)
	if err != nil {
		return nil, mcpRoom{}, mcpErr(err)
	}
	return nil, s.mcpRoom(room), nil
}

// mcpMessages flattens events to what a model needs: who said what, as
// text. Parts other than text show as [kind].
func (s *Server) mcpMessages(ctx context.Context, events []core.Event) ([]mcpMessage, error) {
	names := map[string]core.Principal{}
	out := make([]mcpMessage, 0, len(events))
	for _, e := range events {
		if e.Type != core.EventMessage {
			continue
		}
		p, ok := names[e.SenderID]
		if !ok {
			var err error
			p, err = s.store.GetPrincipal(ctx, e.SenderID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			names[e.SenderID] = p
		}
		out = append(out, mcpMessage{
			ID:         e.ID,
			Seq:        e.Seq,
			SenderID:   e.SenderID,
			SenderName: p.DisplayName,
			SenderKind: string(p.Kind),
			Text:       e.Text(),
			ReplyTo:    e.Causation.CausedBy,
			HopCount:   e.Causation.HopCount,
			SentAt:     e.CreatedAt.UTC(),
		})
	}
	return out, nil
}
