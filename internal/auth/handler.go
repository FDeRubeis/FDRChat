package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"

	"fdrchat/internal/httpx"
	"fdrchat/internal/session"
	"fdrchat/internal/user"
)

const (
	minPasswordLen = 8 // not conforming to OWASP recommendations - keeping it small for testing convenience
	maxPasswordLen = 128
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

type UserStore interface {
	Create(ctx context.Context, name, passwordHash string) (user.User, error)
	GetByName(ctx context.Context, name string) (user.User, string, error)
	GetByID(ctx context.Context, id uuid.UUID) (user.User, error)
}

type SessionManager interface {
	Create(ctx context.Context, userID uuid.UUID) (string, session.Session, error)
	Authenticate(ctx context.Context, token string) (session.Session, error)
	Revoke(ctx context.Context, sessionID int64) error
}

type Handler struct {
	users    UserStore
	sessions SessionManager
}

func NewHandler(users UserStore, sessions SessionManager) *Handler {
	return &Handler{users: users, sessions: sessions}
}

// dummyHash is verified against when the user does not exist, so that login
// takes the same time for unknown names and wrong passwords.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("dummy password for timing equalization")
	if err != nil {
		panic(err)
	}
	return h
})

type registerRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (req registerRequest) validate() error {
	if !namePattern.MatchString(req.Name) {
		return errors.New("name must be 3-32 characters: letters, digits, '_' or '-'")
	}
	n := utf8.RuneCountInString(req.Password)
	if n < minPasswordLen || n > maxPasswordLen {
		return errors.New("password must be 8-128 characters")
	}
	// no special character enforcement in passwords
	// as per OWASP recommendations
	// https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html
	return nil
}

// Register handles POST /auth/register.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if herr := httpx.DecodeJSON(w, r, &req); herr != nil {
		httpx.WriteError(w, herr.Status, herr.Message)
		return
	}
	if err := req.validate(); err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		slog.Error("hash password", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	u, err := h.users.Create(r.Context(), req.Name, hash)
	switch {
	case errors.Is(err, user.ErrNameTaken):
		httpx.WriteError(w, http.StatusConflict, "name already taken")
		return
	case err != nil:
		slog.Error("create user", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, u)
}

type loginRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Login handles POST /auth/login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if herr := httpx.DecodeJSON(w, r, &req); herr != nil {
		httpx.WriteError(w, herr.Status, herr.Message)
		return
	}

	u, hash, err := h.users.GetByName(r.Context(), req.Name)
	found := err == nil
	switch {
	case errors.Is(err, user.ErrNotFound):
		hash = dummyHash() // defense against timing attacks
	case err != nil:
		slog.Error("get user by name", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ok, err := VerifyPassword(req.Password, hash)
	if err != nil {
		slog.Error("verify password", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if !ok || !found {
		httpx.WriteError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// A fresh token on every login prevents session fixation; a session the
	// client still carries is replaced rather than left valid in the database.
	if old := session.TokenFromRequest(r); old != "" {
		if s, err := h.sessions.Authenticate(r.Context(), old); err == nil {
			if err := h.sessions.Revoke(r.Context(), s.ID); err != nil {
				slog.Error("failed to revoke previous session", "err", err)
			}
		}
	}

	token, s, err := h.sessions.Create(r.Context(), u.ID)
	if err != nil {
		slog.Error("create session", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	session.SetCookie(w, token, s)
	httpx.WriteJSON(w, http.StatusOK, u)
}

// Logout handles POST /auth/logout. It ends the current session.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	s, _ := session.FromContext(r.Context())
	if err := h.sessions.Revoke(r.Context(), s.ID); err != nil {
		slog.Error("revoke session", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	session.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /me.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	s, _ := session.FromContext(r.Context())
	u, err := h.users.GetByID(r.Context(), s.UserID)
	switch {
	case errors.Is(err, user.ErrNotFound):
		httpx.WriteError(w, http.StatusUnauthorized, "authentication required")
		return
	case err != nil:
		slog.Error("get user by id", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}
