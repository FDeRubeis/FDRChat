// Package events serves the GET /events server-sent event stream.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"fdrchat/internal/httpx"
	"fdrchat/internal/session"
	"fdrchat/internal/stream"
)

// heartbeatInterval keeps proxies from closing an idle stream.
const heartbeatInterval = 25 * time.Second

// Authenticator is implemented by *session.Manager.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (session.Session, error)
}

type Handler struct {
	hub       *stream.Hub
	sessions  Authenticator
	heartbeat time.Duration
}

func NewHandler(hub *stream.Hub, sessions Authenticator) *Handler {
	return &Handler{hub: hub, sessions: sessions, heartbeat: heartbeatInterval}
}

// Stream serves GET /events. It must run behind session.Manager.Require.
//
// The stream ends when the client disconnects or when its session is deleted.
func (h *Handler) Stream(w http.ResponseWriter, r *http.Request) {
	s, ok := session.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	token := session.TokenFromRequest(r)

	sub, ok := h.hub.Subscribe(r.Context(), s.ID)
	if !ok {
		httpx.WriteError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}
	defer sub.Close()

	// The deadline ends the stream when the session expires, so the end of
	// its lifetime does not have to wait for the cleanup job.
	ctx, cancel := context.WithDeadline(sub.Context(), s.ExpiresAt)
	defer cancel()

	// Check if the session is still valid and it hasn't been deleted
	// before the stream was created.
	if _, err := h.sessions.Authenticate(ctx, token); err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			slog.Error("events: authenticate", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		httpx.WriteError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // ask reverse proxies not to buffer
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return // no streaming support, nothing useful to do
	}

	heartbeat := time.NewTicker(h.heartbeat)
	defer heartbeat.Stop() // stop the ticker when the stream ends

	for {
		select {
		case <-ctx.Done():
			return

		case e := <-sub.Events():
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Name, e.Data); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			if e.OnFlush != nil {
				go e.OnFlush()
			}

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
