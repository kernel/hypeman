package authserver

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// SpentStore remembers the single-use identity tokens that were exchanged,
// for every instance of the server at once.
type SpentStore interface {
	// Spend records id and reports whether this call was the first to do so.
	// It reports true to exactly one caller, however many ask at the same time.
	Spend(ctx context.Context, id string, expires time.Time) (first bool, err error)
}

const (
	// spentRetention is how long a record outlives its identity token. A token
	// is refused once it has expired, so the record is no longer needed then;
	// keeping it a day longer leaves no room for a wrong clock to matter.
	spentRetention = 24 * time.Hour
	spentCleanup   = 10 * time.Minute

	// The table is named with its schema, so that every instance uses the
	// same one whatever its role's search path is.
	spentSchema = `CREATE TABLE IF NOT EXISTS public.hypeman_auth_spent_assertions (
	id         TEXT PRIMARY KEY,
	expires_at TIMESTAMPTZ NOT NULL
)`
	spentIndex  = `CREATE INDEX IF NOT EXISTS hypeman_auth_spent_assertions_expires_at ON public.hypeman_auth_spent_assertions (expires_at)`
	spentInsert = `INSERT INTO public.hypeman_auth_spent_assertions (id, expires_at) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`
	// The database's clock decides, so the instances agree on what is old.
	spentDelete = `DELETE FROM public.hypeman_auth_spent_assertions WHERE expires_at < now() - make_interval(secs => $1)`
	// spentSchemaLock serializes instances that start together: CREATE ...
	// IF NOT EXISTS alone can fail when two sessions create the table at once.
	spentSchemaLock = `SELECT pg_advisory_xact_lock(4801270353920264117)`
)

// PostgresStore keeps spent identity tokens in a PostgreSQL table. The insert
// is one statement on the primary key, so two instances exchanging the same
// identity token cannot both be first.
type PostgresStore struct {
	db *sql.DB
}

// OpenPostgresStore connects to url and creates the table if it is missing.
func OpenPostgresStore(ctx context.Context, url string) (*PostgresStore, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		// The URL holds a password, so the parse error is not passed on.
		return nil, fmt.Errorf("the replay store URL is not a PostgreSQL connection string")
	}
	// Plain statements work through a transaction pooler as well as directly.
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := createSpentTable(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare the replay store: %w", err)
	}
	return &PostgresStore{db: db}, nil
}

func createSpentTable(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{spentSchemaLock, spentSchema, spentIndex} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) Spend(ctx context.Context, id string, expires time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, spentInsert, id, expires.UTC())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// Cleanup deletes the records that are no longer needed, now and then until
// ctx ends. Every instance may run it.
func (s *PostgresStore) Cleanup(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(spentCleanup)
	defer ticker.Stop()
	for {
		if err := s.deleteOld(ctx); err != nil && ctx.Err() == nil {
			log.Warn("old records of spent identity tokens were not deleted", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *PostgresStore) deleteOld(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, spentDelete, spentRetention.Seconds())
	return err
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}
