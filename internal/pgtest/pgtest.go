//go:build integration

// Package pgtest starts a throwaway PostgreSQL for integration tests.
package pgtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"fdrchat/internal/db"
	"fdrchat/internal/user"
)

// NewUser inserts a user with a unique name, so tests sharing one database do
// not interfere with each other.
func NewUser(t *testing.T, pool *pgxpool.Pool) user.User {
	t.Helper()
	u, err := user.NewStore(pool).Create(context.Background(), "u_"+uuid.NewString(), "not-a-real-hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// Start runs Postgres in a container, applies the schema and returns a
// connection pool. Call stop when the tests are done; it closes the pool and
// removes the container.
func Start(ctx context.Context) (pool *pgxpool.Pool, stop func(), err error) {
	ctr, err := postgres.Run(ctx, "postgres:18-alpine",
		postgres.WithDatabase("fdrchat"),
		postgres.WithUsername("fdrchat"),
		postgres.WithPassword("fdrchat"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("start postgres container (is Docker running?): %w", err)
	}
	terminate := func() { _ = testcontainers.TerminateContainer(ctr) }

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		terminate()
		return nil, nil, fmt.Errorf("connection string: %w", err)
	}
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		terminate()
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	if err := db.ApplySchema(ctx, pool); err != nil {
		pool.Close()
		terminate()
		return nil, nil, err
	}
	return pool, func() { pool.Close(); terminate() }, nil
}
