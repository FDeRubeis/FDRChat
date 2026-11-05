package message

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxBodyLen = 2000

const (
	DefaultLimit = 50
	MaxLimit     = 200
)

const pgForeignKeyViolation = "23503"

var (
	ErrNotFound    = errors.New("message: not found")
	ErrNoRecipient = errors.New("message: recipient does not exist")
)

// Status is the lifecycle of a message. It only ever moves forward.
type Status string

const (
	StatusSaved     Status = "saved"
	StatusDelivered Status = "delivered"
	StatusRead      Status = "read"
)

type Message struct {
	ID          uuid.UUID  `json:"id"`
	SenderID    uuid.UUID  `json:"sender_id"`
	RecipientID uuid.UUID  `json:"recipient_id"`
	Body        string     `json:"body"`
	Status      Status     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
	ReadAt      *time.Time `json:"read_at"`
}

// StatusChange is pushed to the sender's own stream when the status of one of their
// messages advances. SenderID is the routing target, not part of the payload.
type StatusChange struct {
	SenderID    uuid.UUID `json:"-"`
	MessageID   uuid.UUID `json:"message_id"`
	RecipientID uuid.UUID `json:"recipient_id"`
	Status      Status    `json:"status"`
	At          time.Time `json:"at"`
}

const messageColumns = `id, sender_id, recipient_id, body, status, created_at, delivered_at, read_at`

// scanMessage reads the columns of messageColumns, in that order.
func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.SenderID, &m.RecipientID, &m.Body,
		&m.Status, &m.CreatedAt, &m.DeliveredAt, &m.ReadAt)
	return m, err
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create stores a new message as saved.
func (s *Store) Create(ctx context.Context, senderID, recipientID uuid.UUID, body string) (Message, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx,
		`INSERT INTO messages (sender_id, recipient_id, body) VALUES ($1, $2, $3)
		 RETURNING `+messageColumns,
		senderID, recipientID, body,
	))

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation { // check if the recipient exists
		return Message{}, ErrNoRecipient
	}
	if err != nil {
		return Message{}, fmt.Errorf("message: create: %w", err)
	}
	return m, nil
}

// Conversation returns the exchange between userID and peerID, oldest first.
// It uses pagination, where "before" is the cursor and "limit" is the page size.
func (s *Store) Conversation(ctx context.Context, userID, peerID uuid.UUID, before *uuid.UUID, limit int) ([]Message, []StatusChange, error) {
	if limit <= 0 || limit > MaxLimit {
		limit = DefaultLimit
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("message: conversation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`UPDATE messages SET status = 'delivered', delivered_at = now()
		 WHERE recipient_id = $1 AND sender_id = $2 AND status = 'saved'
		 RETURNING id, delivered_at`,
		userID, peerID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("message: conversation: mark delivered: %w", err)
	}
	changes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatusChange, error) {
		c := StatusChange{SenderID: peerID, RecipientID: userID, Status: StatusDelivered}
		return c, row.Scan(&c.MessageID, &c.At)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("message: conversation: mark delivered: %w", err)
	}

	// Get the messages before the cursor.
	rows, err = tx.Query(ctx,
		`WITH cursor AS (
		     SELECT created_at, id FROM messages
		     WHERE id = $3::uuid
		       AND ((sender_id = $1 AND recipient_id = $2) OR (sender_id = $2 AND recipient_id = $1))
		 )
		 SELECT `+messageColumns+`
		 FROM messages
		 WHERE ((sender_id = $1 AND recipient_id = $2) OR (sender_id = $2 AND recipient_id = $1))
		   AND ($3::uuid IS NULL OR (created_at, id) < (SELECT created_at, id FROM cursor))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $4`,
		userID, peerID, before, limit,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("message: conversation: %w", err)
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		return scanMessage(row)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("message: conversation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("message: conversation: commit: %w", err)
	}

	slices.Reverse(msgs) // oldest first, the order a client renders
	if msgs == nil {
		msgs = []Message{}
	}
	return msgs, changes, nil
}

// MarkDelivered advances a saved message to delivered and reports whether the
// status actually changed.
func (s *Store) MarkDelivered(ctx context.Context, id uuid.UUID) (StatusChange, bool, error) {
	var c StatusChange
	err := s.pool.QueryRow(ctx,
		`UPDATE messages SET status = 'delivered', delivered_at = now()
		 WHERE id = $1 AND status = 'saved'
		 RETURNING id, sender_id, recipient_id, status, delivered_at`,
		id,
	).Scan(&c.MessageID, &c.SenderID, &c.RecipientID, &c.Status, &c.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusChange{}, false, nil // already delivered or read
	}
	if err != nil {
		return StatusChange{}, false, fmt.Errorf("message: mark delivered: %w", err)
	}
	return c, true, nil
}

// MarkRead marks a message as read by the recipient.
func (s *Store) MarkRead(ctx context.Context, id uuid.UUID, recipientID uuid.UUID) (StatusChange, bool, error) {

	var (
		c      StatusChange
		readAt *time.Time
	)
	// the "target" table restricts everything to the caller's own inbox, because
	// the caller can only read messages that were sent to their inbox.
	err := s.pool.QueryRow(ctx,
		`WITH target AS (
		     SELECT id, sender_id, status FROM messages WHERE id = $1 AND recipient_id = $2
		 ), updated AS (
		     UPDATE messages m
		     SET status = 'read', read_at = now(), delivered_at = COALESCE(m.delivered_at, now())
		     FROM target t
		     WHERE m.id = t.id AND t.status <> 'read'
		     RETURNING m.read_at
		 )
		 SELECT t.sender_id, u.read_at
		 FROM target t LEFT JOIN updated u ON true`,
		id, recipientID,
	).Scan(&c.SenderID, &readAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusChange{}, false, ErrNotFound
	}
	if err != nil {
		return StatusChange{}, false, fmt.Errorf("message: mark read: %w", err)
	}
	if readAt == nil {
		return StatusChange{}, false, nil // already read
	}

	c.MessageID = id
	c.RecipientID = recipientID
	c.Status = StatusRead
	c.At = *readAt
	return c, true, nil
}
