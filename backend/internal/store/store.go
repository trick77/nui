// Package store opens the SQLite database (ncruces driver, no cgo), applies the embedded migrations and holds the
// writes: event batches, collector counters, actor classes, model results, retention. The dashboard reads with its
// own queries (internal/dash).
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/trick77/ship/kit/migrate"
	"github.com/trick77/ship/kit/sqlitex"

	// registers the "sqlite3" database/sql driver; the SQLite build comes with it.
	_ "github.com/ncruces/go-sqlite3/driver"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrations is the embedded migrations directory, files at its root as kit's runner reads them.
func migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded at build time; a programmer error
	}
	return sub
}

// Store is the database.
type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database at path, applies pending migrations and returns the store.
func Open(ctx context.Context, path string) (*Store, error) {
	// Migrations run on a pool of their own, closed before the app's opens (reworm's pattern): a failed
	// no-transaction file must not hand its connection state to the app.
	mdb, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	_, err = migrate.Up(ctx, mdb, migrations(), migrate.Options{Dialect: migrate.SQLite})
	if err == nil {
		err = incrementalVacuum(ctx, mdb)
	}
	_ = mdb.Close()
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &Store{DB: db}, nil
}

// Close closes the pool.
func (s *Store) Close() error { return s.DB.Close() }

// Ping is the readiness check.
func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

// dsn is kit's DSN plus _txlock=immediate (a write transaction takes the lock at BEGIN, so writers queue on the busy
// timeout instead of failing on upgrade) and temp_store in memory: the container's root is read-only, and a
// dashboard read's GROUP BY or DISTINCT that spills its sorter to a temp file would fail there.
func dsn(path string) string {
	return sqlitex.DSN(path, "_pragma=temp_store(memory)", "_txlock=immediate")
}

// incrementalVacuum turns on incremental auto-vacuum while the database holds no events, so the retention job can
// hand deleted pages back to the file system (PRAGMA incremental_vacuum) instead of a full VACUUM that locks
// everything. The mode only changes through a VACUUM, instant on an empty database and never run on a full one; the
// DSN cannot set it, since kit's journal_mode(wal) creates the file first.
func incrementalVacuum(ctx context.Context, db *sql.DB) error {
	var mode int
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode == 2 {
		return err
	}
	var has int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM event)`).Scan(&has); err != nil || has == 1 {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `VACUUM`)
	return err
}
