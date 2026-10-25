package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lum1n/say-v2/internal/store/db"
	_ "modernc.org/sqlite"
)

// OpenSQLite opens (creating if needed) and migrates the SQLite database at
// path, in WAL mode. Write transactions take the write lock when they begin,
// so concurrent writers wait on the busy timeout instead of failing to
// upgrade a read lock.
func OpenSQLite(ctx context.Context, path string) (Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	params := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(10000)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		params.Add("_pragma", p)
	}
	params.Set("_txlock", "immediate")
	conn, err := sql.Open("sqlite", "file:"+path+"?"+params.Encode())
	if err != nil {
		return nil, err
	}
	s := &sqlStore{db: conn, wrap: func(d db.DBTX) db.DBTX { return sqliteDBTX{d} }}
	if err := s.migrate(ctx, ""); err != nil {
		conn.Close()
		return nil, fmt.Errorf("store: open sqlite %s: %w", path, err)
	}
	return s, nil
}

// sqliteDBTX rewrites Postgres placeholders ($1) to SQLite's numbered ones
// (?1), which bind the same argument.
type sqliteDBTX struct{ db.DBTX }

func (d sqliteDBTX) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.DBTX.ExecContext(ctx, sqlitePlaceholders(query), args...)
}

func (d sqliteDBTX) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return d.DBTX.PrepareContext(ctx, sqlitePlaceholders(query))
}

func (d sqliteDBTX) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DBTX.QueryContext(ctx, sqlitePlaceholders(query), args...)
}

func (d sqliteDBTX) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DBTX.QueryRowContext(ctx, sqlitePlaceholders(query), args...)
}

var rewritten sync.Map // query string -> rewritten string

// sqlitePlaceholders replaces $N with ?N outside quoted strings,
// identifiers and comments.
func sqlitePlaceholders(query string) string {
	if !strings.Contains(query, "$") {
		return query
	}
	if r, ok := rewritten.Load(query); ok {
		return r.(string)
	}
	var b strings.Builder
	b.Grow(len(query))
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' || c == '"':
			end := strings.IndexByte(query[i+1:], c)
			if end < 0 {
				b.WriteString(query[i:])
				i = len(query)
				continue
			}
			b.WriteString(query[i : i+end+2])
			i += end + 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query) - i
			}
			b.WriteString(query[i : i+end])
			i += end - 1
		case c == '$' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9':
			b.WriteByte('?')
		default:
			b.WriteByte(c)
		}
	}
	r := b.String()
	rewritten.Store(query, r)
	return r
}
