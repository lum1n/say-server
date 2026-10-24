package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrate applies the migrations that are not yet recorded in
// schema_migrations, in file name order, in one transaction. lock is a
// statement run first so that nodes starting together migrate one at a
// time; it may be empty when the transaction itself is exclusive.
func (s *sqlStore) migrate(ctx context.Context, lock string) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.wrap(tx)
	if lock != "" {
		if _, err := q.ExecContext(ctx, lock); err != nil {
			return fmt.Errorf("store: migration lock: %w", err)
		}
	}
	_, err = q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    BIGINT PRIMARY KEY,
		applied_at BIGINT NOT NULL
	)`)
	if err != nil {
		return err
	}
	applied := map[int64]bool{}
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}
		body, err := fs.ReadFile(migrations, name)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		_, err = q.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`,
			version, time.Now().UnixMilli())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrationVersion reads the number before the first underscore of
// migrations/0001_init.sql.
func migrationVersion(name string) (int64, error) {
	base := strings.TrimPrefix(name, "migrations/")
	num, _, _ := strings.Cut(base, "_")
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: migration %s has no numeric prefix", name)
	}
	return v, nil
}
