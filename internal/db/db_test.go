//go:build integration

package db_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"fdrchat/internal/db"
	"fdrchat/internal/pgtest"
)

var (
	pool *pgxpool.Pool
	ctx  = context.Background()
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	p, stop, err := pgtest.Start(ctx) // applies the schema to an empty database
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	pool = p
	return m.Run()
}

func newUser(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (name, password_hash) VALUES ($1, 'hash') RETURNING id`, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert user %q: %v", name, err)
	}
	return id
}

func TestSchemaCreatesTheTables(t *testing.T) {
	for _, table := range []string{"users", "sessions", "messages"} {
		t.Run(table, func(t *testing.T) {
			var exists bool
			if err := pool.QueryRow(ctx,
				`SELECT to_regclass('public.' || $1) IS NOT NULL`, table,
			).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Fatalf("table %q was not created", table)
			}
		})
	}
}

func TestSchemaCreatesTheIndexes(t *testing.T) {
	indexes := []string{
		"users_name_lower_idx",
		"sessions_user_id_idx",
		"messages_sender_recipient_idx",
		"messages_recipient_sender_idx",
	}
	for _, name := range indexes {
		t.Run(name, func(t *testing.T) {
			var exists bool
			if err := pool.QueryRow(ctx,
				`SELECT count(*) > 0 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name,
			).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Fatalf("index %q was not created", name)
			}
		})
	}
}

func TestSchemaFillsInTheDefaults(t *testing.T) {
	sender, recipient := newUser(t, uuid.NewString()), newUser(t, uuid.NewString())

	var (
		id                  uuid.UUID
		status              string
		createdAt           time.Time
		deliveredAt, readAt *time.Time
	)
	if err := pool.QueryRow(ctx,
		`INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, 'hi')
		 RETURNING id, status, created_at, delivered_at, read_at`,
		sender, recipient,
	).Scan(&id, &status, &createdAt, &deliveredAt, &readAt); err != nil {
		t.Fatal(err)
	}

	if id.Version() != 7 {
		t.Errorf("message id is a v%d uuid, want v7", id.Version())
	}
	if status != "saved" || deliveredAt != nil || readAt != nil {
		t.Errorf("defaults = %q, %v, %v; want saved without timestamps", status, deliveredAt, readAt)
	}
	if time.Since(createdAt) > time.Minute {
		t.Errorf("created_at = %v, want about now", createdAt)
	}
}

func TestSchemaEnforcesCaseInsensitivelyUniqueNames(t *testing.T) {
	name := "Alice_" + uuid.NewString()
	newUser(t, name)

	for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (name, password_hash) VALUES ($1, 'hash')`, variant,
		); err == nil {
			t.Errorf("inserting %q a second time was allowed", variant)
		}
	}
}

func TestSchemaEnforcesTheMessageConstraints(t *testing.T) {
	sender, recipient := newUser(t, uuid.NewString()), newUser(t, uuid.NewString())

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{"message to oneself", `INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $1, 'hi')`, []any{sender}},
		{"empty body", `INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, '')`, []any{sender, recipient}},
		{"body over 2000 characters", `INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, $3)`, []any{sender, recipient, strings.Repeat("a", 2001)}},
		{"unknown sender", `INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, 'hi')`, []any{uuid.New(), recipient}},
		{"unknown recipient", `INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, 'hi')`, []any{sender, uuid.New()}},
		{"delivered without a timestamp", `INSERT INTO messages (sender_id, recipient_id, body, status) VALUES ($1, $2, 'hi', 'delivered')`, []any{sender, recipient}},
		{"read without a delivery", `INSERT INTO messages (sender_id, recipient_id, body, status, read_at) VALUES ($1, $2, 'hi', 'read', now())`, []any{sender, recipient}},
		{"saved with a timestamp", `INSERT INTO messages (sender_id, recipient_id, body, delivered_at) VALUES ($1, $2, 'hi', now())`, []any{sender, recipient}},
		{"unknown status", `INSERT INTO messages (sender_id, recipient_id, body, status) VALUES ($1, $2, 'hi', 'pending')`, []any{sender, recipient}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tt.sql, tt.args...); err == nil {
				t.Fatal("the insert was allowed")
			}
		})
	}

	// The length limit counts characters, not bytes.
	if _, err := pool.Exec(ctx,
		`INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, $3)`,
		sender, recipient, strings.Repeat("é", 2000),
	); err != nil {
		t.Fatalf("2000 multibyte characters must be allowed: %v", err)
	}
}

func TestDeletingAUserCascadesToSessionsAndMessages(t *testing.T) {
	alice, bob := newUser(t, uuid.NewString()), newUser(t, uuid.NewString())
	if _, err := pool.Exec(ctx,
		`INSERT INTO sessions (user_id, token_hash, expires_at)
		 VALUES ($1, $2, now() + interval '1 hour')`, alice, []byte(uuid.NewString()),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, 'hi')`, alice, bob,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, alice); err != nil {
		t.Fatalf("deleting a user must not be blocked by its rows: %v", err)
	}

	for table, query := range map[string]string{
		"sessions": `SELECT count(*) FROM sessions WHERE user_id = $1`,
		"messages": `SELECT count(*) FROM messages WHERE sender_id = $1 OR recipient_id = $1`,
	} {
		var n int
		if err := pool.QueryRow(ctx, query, alice).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s left %d rows behind", table, n)
		}
	}
}

// pgtest.Start has already applied the schema to an empty database, so
// applying it again is what every restart does.
func TestApplyingTheSchemaToAnExistingDatabaseIsHarmless(t *testing.T) {
	id := newUser(t, uuid.NewString())

	for range 2 {
		if err := db.ApplySchema(ctx, pool); err != nil {
			t.Fatalf("restart on an existing schema: %v", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("users with the inserted id = %d, want 1: reapplying must keep data", n)
	}
}
