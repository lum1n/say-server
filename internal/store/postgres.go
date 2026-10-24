package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/vegard/say-v2/internal/store/db"
)

// migrationLock is the Postgres advisory lock key that serializes
// migrations across nodes starting at the same time.
const migrationLock = 0x5a7_0001

// OpenPostgres connects to and migrates the Postgres database at dsn.
func OpenPostgres(ctx context.Context, dsn string) (Store, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	s := &sqlStore{db: conn, wrap: func(d db.DBTX) db.DBTX { return d }}
	if err := s.migrate(ctx, fmt.Sprintf("SELECT pg_advisory_xact_lock(%d)", migrationLock)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: open postgres: %w", err)
	}
	return s, nil
}
