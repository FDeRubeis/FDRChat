package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNameTaken is returned when a user with the same name (case-insensitive) exists.
	ErrNameTaken = errors.New("user: name already taken")
	ErrNotFound  = errors.New("user: not found")
)

const pgUniqueViolation = "23505"

type User struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Summary is the public view of a user.
type Summary struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts a new user. It returns ErrNameTaken if the name is in use.
func (s *Store) Create(ctx context.Context, name, passwordHash string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (name, password_hash) VALUES ($1, $2)
		 RETURNING id, name, created_at`,
		name, passwordHash,
	).Scan(&u.ID, &u.Name, &u.CreatedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return User{}, ErrNameTaken
	}
	if err != nil {
		return User{}, fmt.Errorf("user: create: %w", err)
	}
	return u, nil
}

// GetByName looks up a user by name (case-insensitive) and returns it with its password hash.
func (s *Store) GetByName(ctx context.Context, name string) (User, string, error) {
	var (
		u    User
		hash string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, created_at, password_hash FROM users WHERE lower(name) = lower($1)`,
		name,
	).Scan(&u.ID, &u.Name, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("user: get by name: %w", err)
	}
	return u, hash, nil
}

// List returns every registered user, ordered by name.
func (s *Store) List(ctx context.Context) ([]Summary, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM users ORDER BY lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("user: list: %w", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Summary])
	if err != nil {
		return nil, fmt.Errorf("user: list: %w", err)
	}
	if list == nil {
		list = []Summary{} // encode as [] rather than null
	}
	return list, nil
}

func (s *Store) GetByID(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, created_at FROM users WHERE id = $1`,
		id,
	).Scan(&u.ID, &u.Name, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("user: get by id: %w", err)
	}
	return u, nil
}
