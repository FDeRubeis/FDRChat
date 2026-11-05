package message

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"fdrchat/internal/httpx"
	"fdrchat/internal/session"
	"fdrchat/internal/stream"
)

// Event names on the SSE stream.
const (
	EventMessage = "message"
	EventStatus  = "status"
)

// hookTimeout bounds the work that happens outside a request's lifetime: the
// session lookup before a live push, and the status writes after a delivery.
const hookTimeout = 5 * time.Second

// SessionLister resolves a user to the sessions their streams belong to.
type SessionLister interface {
	IDsForUser(ctx context.Context, userID uuid.UUID) ([]int64, error)
}

// Publisher routes events to the live streams of the given sessions.
type Publisher interface {
	Publish(sessionIDs []int64, e stream.Event)
}

// MessageStore is the persistence the handlers need; *Store implements it.
type MessageStore interface {
	Create(ctx context.Context, senderID, recipientID uuid.UUID, body string) (Message, error)
	Conversation(ctx context.Context, userID, peerID uuid.UUID, before *uuid.UUID, limit int) ([]Message, []StatusChange, error)
	MarkDelivered(ctx context.Context, id uuid.UUID) (StatusChange, bool, error)
	MarkRead(ctx context.Context, id, recipientID uuid.UUID) (StatusChange, bool, error)
}

type Handler struct {
	store     MessageStore
	sessions  SessionLister
	publisher Publisher
}

func NewHandler(store MessageStore, sessions SessionLister, publisher Publisher) *Handler {
	return &Handler{store: store, sessions: sessions, publisher: publisher}
}

type sendRequest struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

// Send handles POST /messages.
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	s, _ := session.FromContext(r.Context())

	var req sendRequest
	if herr := httpx.DecodeJSON(w, r, &req); herr != nil {
		httpx.WriteError(w, herr.Status, herr.Message)
		return
	}

	to, err := uuid.Parse(req.To)
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "to must be a user id")
		return
	}
	if to == s.UserID {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "cannot send a message to yourself")
		return
	}

	body := strings.TrimSpace(req.Body)
	switch {
	case body == "":
		httpx.WriteError(w, http.StatusUnprocessableEntity, "body must not be empty")
		return
	case utf8.RuneCountInString(body) > MaxBodyLen:
		httpx.WriteError(w, http.StatusUnprocessableEntity,
			"body must be at most "+strconv.Itoa(MaxBodyLen)+" characters")
		return
	}

	m, err := h.store.Create(r.Context(), s.UserID, to, body)
	switch {
	case errors.Is(err, ErrNoRecipient):
		httpx.WriteError(w, http.StatusNotFound, "recipient not found")
		return
	case err != nil:
		slog.Error("create message", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.publishMessage(m)
	httpx.WriteJSON(w, http.StatusCreated, m)
}

// Conversation handles GET /conversations/{userID}/messages.
func (h *Handler) Conversation(w http.ResponseWriter, r *http.Request) {
	s, _ := session.FromContext(r.Context())

	peerID, err := uuid.Parse(r.PathValue("userID"))
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid user id")
		return
	}
	if peerID == s.UserID {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "cannot converse with yourself")
		return
	}

	// Note: if "before" is not provided, the conversation will be returned from the newest message.
	var before *uuid.UUID
	if raw := r.URL.Query().Get("before"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "before must be a message id")
			return
		}
		before = &id
	}
	limit, err := intParam(r, "limit")
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "limit must be a number")
		return
	}

	msgs, changes, err := h.store.Conversation(r.Context(), s.UserID, peerID, before, int(limit))
	if err != nil {
		slog.Error("load conversation", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	for _, c := range changes {
		h.publishStatus(c)
	}
	httpx.WriteJSON(w, http.StatusOK, msgs)
}

// MarkRead handles POST /messages/{id}/read. Only the recipient may call it.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	s, _ := session.FromContext(r.Context())

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "message not found")
		return
	}

	c, changed, err := h.store.MarkRead(r.Context(), id, s.UserID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "message not found")
		return
	case err != nil:
		slog.Error("mark message read", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if changed {
		h.publishStatus(c)
	}
	w.WriteHeader(http.StatusNoContent)
}

// publishMessage pushes a new message to the recipient's streams. The
// message only becomes delivered once one of them has actually flushed it.
func (h *Handler) publishMessage(m Message) {
	data, err := json.Marshal(m)
	if err != nil {
		slog.Error("encode message event", "err", err)
		return
	}
	h.publish(m.RecipientID, stream.Event{
		Name:    EventMessage,
		Data:    data,
		OnFlush: func() { h.markDelivered(m.ID) },
	})
}

// publish pushes e to every stream of userID.
func (h *Handler) publish(userID uuid.UUID, e stream.Event) {
	// The IDsForUser lookup has its own context so that a sender
	// who disconnects right after the insert does not cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	ids, err := h.sessions.IDsForUser(ctx, userID)
	if err != nil {
		slog.Error("look up sessions for live push", "err", err, "event", e.Name)
		return
	}
	h.publisher.Publish(ids, e)
}

// markDelivered marks a message as delivered.
func (h *Handler) markDelivered(id uuid.UUID) {
	// The MarkDelivered operation has its own context because it happens
	// after the request has returned.
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	c, changed, err := h.store.MarkDelivered(ctx, id)
	if err != nil {
		slog.Error("mark message delivered", "err", err, "message_id", id)
		return
	}
	if changed {
		h.publishStatus(c)
	}
}

func (h *Handler) publishStatus(c StatusChange) {
	data, err := json.Marshal(c)
	if err != nil {
		slog.Error("encode status event", "err", err)
		return
	}
	h.publish(c.SenderID, stream.Event{Name: EventStatus, Data: data})
}

// intParam reads an optional non-negative integer query parameter.
func intParam(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, errors.New("invalid " + name)
	}
	return v, nil
}
