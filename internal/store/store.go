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
	"errors"
	"strings"
	"time"

	"github.com/vegard/say-v2/internal/core"
)

var (
	ErrNotFound      = errors.New("store: not found")
	ErrSetupDone     = errors.New("store: setup is already done")
	ErrUsernameTaken = errors.New("store: username is taken")
)

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

	// CreateRoom writes the room and its owner's membership in one
	// transaction.
	CreateRoom(ctx context.Context, r core.Room, owner core.Membership) error
	GetRoom(ctx context.Context, id string) (core.Room, error)
	// RoomsFor returns the rooms principalID is a member of, oldest first.
	RoomsFor(ctx context.Context, principalID string) ([]core.Room, error)

	AddMember(ctx context.Context, m core.Membership) error
	GetMember(ctx context.Context, roomID, principalID string) (core.Membership, error)
	Members(ctx context.Context, roomID string) ([]core.Membership, error)

	// AppendEvent allocates the next seq for e.RoomID and writes the event
	// and its outbox entry in one transaction. A repeated non-empty
	// idempotencyKey from the same sender in the same room returns the
	// originally stored event instead of writing a new one. An empty e.ID
	// or e.CreatedAt is filled in.
	AppendEvent(ctx context.Context, e core.Event, idempotencyKey string) (core.Event, error)
	GetEvent(ctx context.Context, id string) (core.Event, error)

	// EventsAfter returns up to limit events of room with seq > cursor, in
	// seq order.
	EventsAfter(ctx context.Context, roomID string, cursor int64, limit int) ([]core.Event, error)
	// EventsBefore returns the last limit events of room with seq < before,
	// in seq order.
	EventsBefore(ctx context.Context, roomID string, before int64, limit int) ([]core.Event, error)

	// PendingOutbox returns up to limit outbox entries, oldest first.
	// AckOutbox removes one once it has been delivered.
	PendingOutbox(ctx context.Context, limit int) ([]OutboxEntry, error)
	AckOutbox(ctx context.Context, eventID string) error

	Close() error
}

// OutboxEntry points at an event that connectors have not yet been handed.
type OutboxEntry struct {
	EventID string
	RoomID  string
	Seq     int64
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
