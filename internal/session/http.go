package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"fdrchat/internal/httpx"
)

// The __Host- prefix makes browsers enforce Secure, Path=/ and no Domain,
// so the cookie cannot be set or overwritten from a subdomain.
const CookieName = "__Host-session"

func SetCookie(w http.ResponseWriter, token string, s Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(time.Until(s.ExpiresAt).Seconds()),
		HttpOnly: true,
		Secure:   true, // the cookie is only sent over secure connections (e.g. HTTPS, localhost)
		SameSite: http.SameSiteLaxMode,
	})
}

func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   CookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1, // Indicate to the browser that the cookie should be deleted.
		Secure: true,
	})
}

// TokenFromRequest returns the session token from the cookie, or "" if absent.
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

type ctxKey struct{}

// NewContext returns a context carrying s, retrievable with FromContext.
func NewContext(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the session stored by Require.
func FromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(Session)
	return s, ok
}

// Require rejects requests without a valid session with 401 and otherwise
// makes the session available via FromContext.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := TokenFromRequest(r)
		if token == "" {
			httpx.WriteError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		s, err := m.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, ErrNotFound):
			ClearCookie(w)
			httpx.WriteError(w, http.StatusUnauthorized, "authentication required")
			return
		case err != nil:
			slog.Error("authenticate session", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), s)))
	})
}
