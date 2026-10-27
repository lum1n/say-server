// Package store persists principals, rooms and the per-room event log.
// SQLite (WAL) is the default engine and Postgres the cluster engine
// (docs/adr/0002-storage-and-scaling.md).
//
// Both engines share one schema (migrations/), one set of queries
// (queries.sql) and one implementation. sqlc checks the queries with its
// Postgres engine and generates package db; the SQLite engine rewrites the
// $N placeholders to ?N, which SQLite reads as the same numbered parameter.
// Run `make generate` after editing queries.sql or a migration.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/lum1n/say-server/internal/core"
)

var (
	ErrNotFound      = errors.New("store: not found")
	ErrSetupDone     = errors.New("store: setup is already done")
	ErrUsernameTaken = errors.New("store: username is taken")
	// ErrNotEditable refuses an edit of an event that is not the editor's
	// own message, or an edit of a final message that changes nothing.
	ErrNotEditable = errors.New("store: the event cannot be edited")
	// ErrNotStreaming refuses a streaming update of a message that is
	// final, someone else's or unknown.
	ErrNotStreaming = errors.New("store: the message is not streaming")
)

// AppendOptions are the conditions of an AppendEvent.
type AppendOptions struct {
	// A repeated non-empty IdempotencyKey from the same sender in the same
	// room returns the originally stored event instead of writing a new one.
	IdempotencyKey string
	// Automated marks an event from an automated sender. Only automated
	// events count against the budgets, and only they are checked.
	Automated bool
	// Room budgets the room's automated events from all senders, Sender
	// those from e.SenderID.
	Room, Sender Budget
}

// Budget allows at most Max events in any Window. A zero Max has no limit.
type Budget struct {
	Max    int
	Window time.Duration
}

// BudgetError is AppendEvent's error when an automated event would exceed
// a budget. RetryAt is when the oldest counted event leaves the window.
type BudgetError struct {
	Sender  bool // the Sender budget, not the Room budget
	Budget  Budget
	RetryAt time.Time
}

func (e *BudgetError) Error() string {
	who := "room"
	if e.Sender {
		who = "sender"
	}
	return fmt.Sprintf("store: %s budget of %d events per %s is spent", who, e.Budget.Max, e.Budget.Window)
}

// NewToken is a token to issue: whose, a name saying what it is for
// (such as "session" or "device:Ada's phone"), the client that asked
// for it, and when it expires (zero never).
type NewToken struct {
	PrincipalID string
	Name        string
	UserAgent   string
	Expires     time.Time
}

// Token is what is kept of an access token, never its secret.
type Token struct {
	ID         string
	Name       string
	UserAgent  string
	CreatedAt  time.Time
	ExpiresAt  time.Time // zero never
	LastUsedAt time.Time // zero unknown; it is recorded at most hourly
}

// Login is a username and password hash that signs a human principal in.
type Login struct {
	PrincipalID  string
	Username     string
	PasswordHash string
}

type Store interface {
	CreatePrincipal(ctx context.Context, p core.Principal) error
	GetPrincipal(ctx context.Context, id string) (core.Principal, error)
	// Principals returns every principal, oldest first.
	Principals(ctx context.Context) ([]core.Principal, error)

	// CreateUser writes a principal and its login in one transaction. It
	// returns ErrUsernameTaken if another login has l.Username.
	// l.PrincipalID is ignored.
	CreateUser(ctx context.Context, p core.Principal, l Login) error
	// LoginByUsername returns ErrNotFound for an unknown username.
	LoginByUsername(ctx context.Context, username string) (Login, error)

	// Setup creates the instance's first admin, once. It returns
	// ErrSetupDone if setup has already been done, also by another node.
	Setup(ctx context.Context, admin core.Principal, l Login) error
	SetupDone(ctx context.Context) (bool, error)

	// CreateToken issues a new access token for principalID and returns its
	// secret. Only a hash of the secret is stored, so it cannot be shown
	// again. A zero expires never expires. PrincipalByToken returns
	// ErrNotFound for an unknown, expired or revoked secret.
	CreateToken(ctx context.Context, principalID, name string, expires time.Time) (secret string, err error)
	PrincipalByToken(ctx context.Context, secret string) (core.Principal, error)
	// RevokeToken deletes the token; revoking an unknown secret is not an
	// error.
	RevokeToken(ctx context.Context, secret string) error
	// RevokeTokensNamed deletes principalID's tokens with that name.
	RevokeTokensNamed(ctx context.Context, principalID, name string) error
	// IssueToken is CreateToken with the client that asked for it, and
	// returns the token's record too.
	IssueToken(ctx context.Context, t NewToken) (secret string, _ Token, err error)
	// Authenticate is PrincipalByToken with the token's id. It records
	// when the token was last used, at most hourly.
	Authenticate(ctx context.Context, secret string) (_ core.Principal, tokenID string, err error)
	// Tokens lists principalID's tokens that haven't expired, newest
	// first (docs/adr/0018-devices.md).
	Tokens(ctx context.Context, principalID string) ([]Token, error)
	// RevokeTokenID deletes principalID's token id, or returns
	// ErrNotFound.
	RevokeTokenID(ctx context.Context, principalID, id string) error

	// CreatePairing makes a one-time code that signs in as principalID on
	// another device until expires, and returns it. RedeemPairing uses it
	// up and returns whose it was, or ErrNotFound for a wrong, used or
	// expired code. Codes are case-insensitive and may carry dashes.
	CreatePairing(ctx context.Context, principalID string, expires time.Time) (code string, err error)
	RedeemPairing(ctx context.Context, code string) (principalID string, err error)

	// CreateBridgeScreen makes a code, like a pairing code, that shows
	// bridgeID's login step on another screen until expires. BridgeScreen
	// returns the bridge a code is for, any number of times until then, or
	// ErrNotFound.
	CreateBridgeScreen(ctx context.Context, bridgeID string, expires time.Time) (code string, err error)
	BridgeScreen(ctx context.Context, code string) (bridgeID string, err error)

	// CreateRoom writes the room and its owner's membership in one
	// transaction.
	CreateRoom(ctx context.Context, r core.Room, owner core.Membership) error
	GetRoom(ctx context.Context, id string) (core.Room, error)
	// RoomsFor returns the rooms principalID is a member of, oldest first.
	RoomsFor(ctx context.Context, principalID string) ([]core.Room, error)
	// SetAgentPolicy replaces the room's agent policy.
	SetAgentPolicy(ctx context.Context, roomID string, p core.AgentPolicy) error

	AddMember(ctx context.Context, m core.Membership) error
	GetMember(ctx context.Context, roomID, principalID string) (core.Membership, error)
	Members(ctx context.Context, roomID string) ([]core.Membership, error)
	// SetMemberScopes replaces a membership's scopes.
	SetMemberScopes(ctx context.Context, roomID, principalID string, scopes []string) error

	// AppendEvent allocates the next seq for e.RoomID and writes the
	// event. An empty e.ID or e.CreatedAt
	// is filled in. The budgets are checked under the room's lock, so they
	// hold across the nodes of a cluster; a spent one is a *BudgetError.
	AppendEvent(ctx context.Context, e core.Event, opts AppendOptions) (core.Event, error)
	GetEvent(ctx context.Context, id string) (core.Event, error)

	// UpdateStreaming replaces the parts of senderID's message eventID in
	// place, without a new event, while the message is still streaming;
	// otherwise it returns ErrNotStreaming.
	UpdateStreaming(ctx context.Context, eventID, senderID string, parts []core.Part) error
	// AppendEdit appends edit e, which changes message e.Edits in
	// e.RoomID, and updates the message to the result in one transaction.
	// A nil e.Parts keeps the message's parts. An edit of a streaming
	// message ends its stream; an edit of a final message must have parts
	// and sets its EditedAt. Only the message's sender may edit it, else
	// ErrNotEditable; an unknown message is ErrNotFound. It returns the
	// edit as stored and the message as it now is.
	AppendEdit(ctx context.Context, e core.Event) (edit, message core.Event, err error)
	// StreamingFrom returns senderID's messages that are still streaming,
	// oldest first.
	StreamingFrom(ctx context.Context, senderID string) ([]core.Event, error)

	// EventsAfter returns up to limit events of room with seq > cursor, in
	// seq order.
	EventsAfter(ctx context.Context, roomID string, cursor int64, limit int) ([]core.Event, error)
	// EventsBefore returns the last limit events of room with seq < before,
	// in seq order.
	EventsBefore(ctx context.Context, roomID string, before int64, limit int) ([]core.Event, error)

	// CreateBuiltinAgent writes agent principal p and its settings in one
	// transaction; a.PrincipalID is ignored. UpdateBuiltinAgent replaces
	// the settings, apart from the last error and the creation time, and
	// renames the agent to displayName unless it is empty.
	CreateBuiltinAgent(ctx context.Context, p core.Principal, a BuiltinAgent) error
	UpdateBuiltinAgent(ctx context.Context, a BuiltinAgent, displayName string) error
	GetBuiltinAgent(ctx context.Context, principalID string) (BuiltinAgent, error)
	// BuiltinAgents returns every built-in agent, oldest first.
	BuiltinAgents(ctx context.Context) ([]BuiltinAgent, error)
	// SetBuiltinAgentError records the agent's latest failure; an empty msg
	// clears it.
	SetBuiltinAgentError(ctx context.Context, principalID, msg string) error
	// AddAgentUsage adds to the tokens and cost the agent used on the UTC
	// day of at, and AgentUsage reads them back.
	AddAgentUsage(ctx context.Context, principalID string, at time.Time, u Usage) error
	AgentUsage(ctx context.Context, principalID string, at time.Time) (Usage, error)
	// AgentCursor returns ErrNotFound when the agent has not read the room.
	// SetAgentCursor only moves a cursor forward.
	AgentCursor(ctx context.Context, principalID, roomID string) (int64, error)
	SetAgentCursor(ctx context.Context, principalID, roomID string, seq int64) error

	// SaveConnectorSession records a bot session, or renews its SeenAt
	// when it exists, and prunes sessions not seen for an hour.
	SaveConnectorSession(ctx context.Context, cs ConnectorSession) error
	DeleteConnectorSession(ctx context.Context, id string) error
	// ConnectorSessions returns the sessions seen since seenSince, oldest
	// first.
	ConnectorSessions(ctx context.Context, seenSince time.Time) ([]ConnectorSession, error)

	// CreateWidget and UpdateWidget write a widget; UpdateWidget keeps its
	// public key and creation time. WidgetByKey finds one by public key.
	CreateWidget(ctx context.Context, w Widget) error
	UpdateWidget(ctx context.Context, w Widget) error
	GetWidget(ctx context.Context, id string) (Widget, error)
	WidgetByKey(ctx context.Context, publicKey string) (Widget, error)
	// Widgets returns every widget, oldest first.
	Widgets(ctx context.Context) ([]Widget, error)
	// DeleteWidget deletes a widget and its visitors and signs them out.
	// Their guest principals and threads stay, so the team keeps the
	// conversations.
	DeleteWidget(ctx context.Context, id string) error
	// CreateVisitor writes a widget visitor's guest principal, its thread
	// with members and the visitor in one transaction. It returns
	// ErrVisitorExists if the widget already has a visitor with
	// v.Subject.
	CreateVisitor(ctx context.Context, guest core.Principal, room core.Room, members []core.Membership, v Visitor) error
	VisitorByPrincipal(ctx context.Context, principalID string) (Visitor, error)
	VisitorBySubject(ctx context.Context, widgetID, subject string) (Visitor, error)
	// VerifyVisitor gives anonymous visitor principalID the subject, so
	// it keeps its thread once the host site verifies it, and renames the
	// guest to displayName and its thread to roomName unless they are
	// empty. It returns ErrVisitorExists if the visitor is not anonymous
	// or its widget already has a visitor with that subject.
	VerifyVisitor(ctx context.Context, principalID, subject, displayName, roomName string) (Visitor, error)
	// LinkVisitor records that anonymous visitor principalID signed in as
	// verified visitor verifiedID, who already had a thread; neither
	// thread changes. A visitor that is not anonymous is ErrNotFound.
	LinkVisitor(ctx context.Context, principalID, verifiedID string) error
	// DeleteVisitor erases a visitor in one transaction: its thread with
	// every event and member, the messages it wrote in other rooms, its
	// memberships and tokens, and its guest principal. Anonymous visitors
	// linked to it are unlinked. An unknown visitor is ErrNotFound.
	DeleteVisitor(ctx context.Context, principalID string) error
	// WidgetThreads returns up to limit widget threads principalID is a
	// member of, latest activity first.
	WidgetThreads(ctx context.Context, principalID string, limit int) ([]WidgetThread, error)

	// CreateBridge and UpdateBridge write a bridge; UpdateBridge changes
	// only its name, config and enabled switch.
	CreateBridge(ctx context.Context, b Bridge) error
	UpdateBridge(ctx context.Context, b Bridge) error
	GetBridge(ctx context.Context, id string) (Bridge, error)
	// Bridges returns every bridge, oldest first.
	Bridges(ctx context.Context) ([]Bridge, error)
	// MarkBridgeDeleted asks for the bridge to be deleted, at at; asking
	// again keeps the first time. DeleteBridge removes it with its links,
	// their messages and its ghosts' mappings; portal rooms and ghost
	// principals stay. Both are ErrNotFound for an unknown bridge.
	MarkBridgeDeleted(ctx context.Context, id string, at time.Time) error
	DeleteBridge(ctx context.Context, id string) error
	// SetBridgeError records the bridge's latest failure; an empty msg
	// clears it. SetBridgeSeen records that its node runs it, or with a
	// zero at that none does.
	SetBridgeError(ctx context.Context, id, msg string) error
	SetBridgeSeen(ctx context.Context, id string, at time.Time) error
	// SetBridgeState replaces what the running node keeps; SetBridgePrompt
	// sets the login step waiting for the owner, or clears it when empty.
	SetBridgeState(ctx context.Context, id string, state json.RawMessage) error
	SetBridgePrompt(ctx context.Context, id string, prompt json.RawMessage) error

	// CreateBridgeLink returns ErrLinkExists if the bridge already links
	// l.RemoteID. DeleteBridgeLink deletes the link and its messages.
	CreateBridgeLink(ctx context.Context, l BridgeLink) error
	// CreatePortal writes a room, its members and the link that mirrors
	// it in one transaction; ErrLinkExists leaves nothing behind.
	CreatePortal(ctx context.Context, r core.Room, members []core.Membership, l BridgeLink) error
	DeleteBridgeLink(ctx context.Context, bridgeID, linkID string) error
	GetBridgeLink(ctx context.Context, id string) (BridgeLink, error)
	BridgeLinkByRemote(ctx context.Context, bridgeID, remoteID string) (BridgeLink, error)
	// BridgeLinks returns a bridge's links and RoomBridgeLinks a room's,
	// oldest first.
	BridgeLinks(ctx context.Context, bridgeID string) ([]BridgeLink, error)
	RoomBridgeLinks(ctx context.Context, roomID string) ([]BridgeLink, error)
	// SetBridgeLinkCursor only moves a link's cursor forward.
	SetBridgeLinkCursor(ctx context.Context, linkID string, seq int64) error

	// EnsureGhost returns the ghost principal of a remote user of a
	// bridge, made on first sight, and renames it to a changed non-empty
	// name.
	EnsureGhost(ctx context.Context, bridgeID, remoteUserID, name string) (core.Principal, error)
	BridgeGhostByPrincipal(ctx context.Context, principalID string) (BridgeGhost, error)

	// StartBridgeMessage records a message crossing a link. It returns
	// false, writing nothing, when the link already has a row for the
	// event or for m.RemoteID.
	StartBridgeMessage(ctx context.Context, m BridgeMessage) (bool, error)
	// FinishBridgeMessage sets the outcome of an outgoing message.
	FinishBridgeMessage(ctx context.Context, linkID, eventID, state, remoteID, errMsg string) error
	BridgeMessageByRemote(ctx context.Context, linkID, remoteID string) (BridgeMessage, error)
	GetBridgeMessage(ctx context.Context, linkID, eventID string) (BridgeMessage, error)
	// MarkBridgeSendsUnknown turns the bridge's sends still marked
	// sending, left by a node that stopped, to unknown.
	MarkBridgeSendsUnknown(ctx context.Context, bridgeID string) error
	// OutgoingBridgeMessages returns the outgoing rows of the room's
	// events with seq in [fromSeq, toSeq].
	OutgoingBridgeMessages(ctx context.Context, roomID string, fromSeq, toSeq int64) ([]BridgeMessage, error)

	// AcquireLease takes or renews the named lease for holder until ttl
	// from now. ok is false while another holder's lease is unexpired.
	// The generation goes up each time the lease changes hands.
	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (generation int64, ok bool, err error)
	// ReleaseLease gives up holder's lease; it is a no-op when holder does
	// not hold it.
	ReleaseLease(ctx context.Context, name, holder string) error

	Close() error
}

// Open opens and migrates the store named by dsn. A postgres:// or
// postgresql:// URL selects Postgres; anything else is a SQLite file path,
// optionally prefixed with sqlite:.
func Open(ctx context.Context, dsn string) (Store, error) {
	if IsPostgres(dsn) {
		return OpenPostgres(ctx, dsn)
	}
	return OpenSQLite(ctx, strings.TrimPrefix(dsn, "sqlite:"))
}

// IsPostgres reports whether Open would pick Postgres for dsn.
func IsPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// CompleteMessage returns the complete message e brings: e itself when it
// is a final message, or the message whose stream e ended. A streaming
// message waits for its end, and other edits bring nothing. Reading both a
// message that is already final and the edit that ended it gives the
// message twice; the edit commits with the message's last change, so a
// reader that sees both sees them in one pass.
func CompleteMessage(ctx context.Context, s Store, e core.Event) (core.Event, bool, error) {
	switch {
	case e.Type == core.EventMessage:
		return e, e.Stream != core.StreamStreaming, nil
	case e.Type == core.EventEdit && e.Stream == core.StreamFinal:
		msg, err := s.GetEvent(ctx, e.Edits)
		if errors.Is(err, ErrNotFound) {
			return core.Event{}, false, nil
		}
		return msg, err == nil, err
	}
	return core.Event{}, false, nil
}

// StartAgentCursor returns principalID's cursor in room. A room it has
// never read gets a cursor core.LookBack events back, which is stored.
func StartAgentCursor(ctx context.Context, s Store, principalID, roomID string) (int64, error) {
	seq, err := s.AgentCursor(ctx, principalID, roomID)
	if !errors.Is(err, ErrNotFound) {
		return seq, err
	}
	last, err := s.EventsBefore(ctx, roomID, math.MaxInt64, 1)
	if err != nil {
		return 0, err
	}
	if len(last) > 0 {
		seq = max(0, last[0].Seq-core.LookBack)
	}
	return seq, s.SetAgentCursor(ctx, principalID, roomID, seq)
}
