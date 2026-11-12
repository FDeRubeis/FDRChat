package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fdrchat/internal/stream"
)

const (
	// These timeouts have been chosen assuming that the chat is commonly
	// used in workplaces, where users login in the morning and need the
	// session to be valid for the entire working day.
	IdleTimeout     = 10 * time.Hour
	AbsoluteTimeout = 20 * time.Hour

	// last_seen_at is refreshed at most this often
	touchInterval = time.Minute

	tokenBytes = 32
)

var tokenLen = base64.RawURLEncoding.EncodedLen(tokenBytes)

// ErrNotFound is returned for unknown, revoked or expired sessions.
var ErrNotFound = errors.New("session: not found or expired")

type Session struct {
	ID        int64
	UserID    uuid.UUID
	ExpiresAt time.Time // absolute expiry; the idle timeout may end the session earlier
}

type Manager struct {
	pool *pgxpool.Pool
	hub  *stream.Hub // streams are cut the moment their session goes away
}

func NewManager(pool *pgxpool.Pool, hub *stream.Hub) *Manager {
	return &Manager{pool: pool, hub: hub}
}

// Create starts a new session for userID and returns the token for the cookie.
// Only the token's SHA-256 hash is stored.
func (m *Manager) Create(ctx context.Context, userID uuid.UUID) (string, Session, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Session{}, fmt.Errorf("session: generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	var s Session
	err := m.pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, token_hash, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3))
		 RETURNING id, user_id, expires_at`,
		userID, hashToken(token), AbsoluteTimeout.Seconds(),
	).Scan(&s.ID, &s.UserID, &s.ExpiresAt)
	if err != nil {
		return "", Session{}, fmt.Errorf("session: create: %w", err)
	}
	return token, s, nil
}

// Authenticate returns the session corresponding to the given token  and
// slides the idle timeout forward.
func (m *Manager) Authenticate(ctx context.Context, token string) (Session, error) {
	if len(token) != tokenLen {
		return Session{}, ErrNotFound
	}

	var s Session
	err := m.pool.QueryRow(ctx,
		`WITH found AS (
		     SELECT id, user_id, expires_at, last_seen_at
		     FROM sessions
		     WHERE token_hash = $1
		 ), touched AS (
		     UPDATE sessions SET last_seen_at = now()
		     FROM found
		     WHERE sessions.id = found.id
		       AND found.last_seen_at < now() - make_interval(secs => $2)
		 )
		 SELECT id, user_id, expires_at FROM found`,
		hashToken(token), touchInterval.Seconds(),
	).Scan(&s.ID, &s.UserID, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("session: authenticate: %w", err)
	}
	return s, nil
}

// IDsForUser returns the ids of every session row the user has.
func (m *Manager) IDsForUser(ctx context.Context, userID uuid.UUID) ([]int64, error) {
	rows, err := m.pool.Query(ctx, `SELECT id FROM sessions WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("session: ids for user: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("session: ids for user: %w", err)
	}
	return ids, nil
}

// Revoke deletes the session and closes its streams.
func (m *Manager) Revoke(ctx context.Context, sessionID int64) error {
	if _, err := m.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
		return fmt.Errorf("session: revoke: %w", err)
	}
	m.hub.CloseSession(sessionID)
	return nil
}

// DeleteExpired removes sessions past their absolute or idle timeout and
// closes the streams of each one, so that an expiring session is torn down
// the same way an explicit logout is.
func (m *Manager) DeleteExpired(ctx context.Context) (int64, error) {
	rows, err := m.pool.Query(ctx,
		`DELETE FROM sessions
		 WHERE expires_at <= now() OR last_seen_at <= now() - make_interval(secs => $1)
		 RETURNING id`,
		IdleTimeout.Seconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("session: delete expired: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, fmt.Errorf("session: delete expired: %w", err)
	}

	for _, id := range ids {
		m.hub.CloseSession(id)
	}
	return int64(len(ids)), nil
}

// RunCleanup calls DeleteExpired every interval until ctx is cancelled.
func (m *Manager) RunCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := m.DeleteExpired(ctx)
			if err != nil {
				slog.Error("session cleanup", "err", err)
				continue
			}
			if n > 0 {
				slog.Info("session cleanup", "deleted", n)
			}
		}
	}
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
