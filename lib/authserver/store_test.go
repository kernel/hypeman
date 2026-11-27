package authserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postgresTestURL is the URL of a PostgreSQL server on which the test may
// create a database of its own, for example
// postgres://postgres:postgres@localhost:5432/postgres. Nothing that is
// already there is touched.
const postgresTestURL = "HYPEMAN_AUTH_TEST_POSTGRES_URL"

func TestPostgresStore(t *testing.T) {
	server := os.Getenv(postgresTestURL)
	if server == "" {
		t.Skipf("%s is not set", postgresTestURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	newID := func() string {
		var id [32]byte
		_, _ = rand.Read(id[:])
		return hex.EncodeToString(id[:])
	}

	// A new, empty database for this run.
	admin, err := sql.Open("pgx", server)
	require.NoError(t, err)
	database := "hypeman_auth_test_" + newID()[:16]
	_, err = admin.ExecContext(ctx, `CREATE DATABASE `+database)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP DATABASE ` + database + ` WITH (FORCE)`)
		assert.NoError(t, err)
		admin.Close()
	})
	// The stores go to that database whatever the URL said: pgx lets the
	// dbname and database parameters override the path.
	location, err := url.Parse(server)
	require.NoError(t, err)
	query := location.Query()
	query.Del("dbname")
	query.Del("database")
	location.Path, location.RawQuery = "/"+database, query.Encode()

	// Instances that start together on a database without the table all come up.
	stores := make(chan *PostgresStore, 8)
	for range cap(stores) {
		go func() {
			store, err := OpenPostgresStore(ctx, location.String())
			assert.NoError(t, err)
			stores <- store
		}()
	}
	var opened []*PostgresStore
	for range cap(stores) {
		if store := <-stores; store != nil {
			defer store.Close()
			opened = append(opened, store)
		}
	}
	require.Len(t, opened, cap(stores))
	first, second := opened[0], opened[1]
	var connected string
	require.NoError(t, first.db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&connected))
	require.Equal(t, database, connected)

	id := newID()
	spent, err := first.Spend(ctx, id, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	require.True(t, spent)
	for _, store := range []*PostgresStore{first, second} {
		spent, err = store.Spend(ctx, id, time.Now().Add(5*time.Minute))
		require.NoError(t, err)
		require.False(t, spent)
	}

	// Raced across both instances, one caller is first.
	raced := newID()
	results := make(chan bool, 32)
	for i := range cap(results) {
		go func() {
			spent, err := []*PostgresStore{first, second}[i%2].Spend(ctx, raced, time.Now().Add(5*time.Minute))
			results <- spent && err == nil
		}()
	}
	winners := 0
	for range cap(results) {
		if <-results {
			winners++
		}
	}
	require.Equal(t, 1, winners)

	// A record outlives its identity token by the retention, then goes.
	recent, old := newID(), newID()
	_, err = first.Spend(ctx, recent, time.Now().Add(-spentRetention+time.Hour))
	require.NoError(t, err)
	_, err = first.Spend(ctx, old, time.Now().Add(-spentRetention-time.Hour))
	require.NoError(t, err)
	require.NoError(t, second.deleteOld(ctx))
	spent, err = first.Spend(ctx, recent, time.Now())
	require.NoError(t, err)
	require.False(t, spent, "still on record")
	spent, err = first.Spend(ctx, old, time.Now())
	require.NoError(t, err)
	require.True(t, spent, "deleted, so recorded anew")
	spent, err = first.Spend(ctx, id, time.Now())
	require.NoError(t, err)
	require.False(t, spent)

	_, err = OpenPostgresStore(ctx, "postgres://user:secret-password@%zz")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-password")
}
