package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"fdrchat/internal/server"
	"fdrchat/internal/session"
	"fdrchat/internal/stream"
)

// The router is wired with stores backed by a nil pool: these tests only
// exercise routing and the session middleware, which both answer before any
// handler reaches the database.
func newRouter(t *testing.T) http.Handler {
	t.Helper()
	hub := stream.NewHub()
	t.Cleanup(hub.Close)
	return server.NewRouter(server.Deps{
		Sessions: session.NewManager(nil, hub),
		Hub:      hub,
	})
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEveryProtectedRouteRequiresASession(t *testing.T) {
	h := newRouter(t)
	someone := uuid.New().String()

	routes := []struct{ method, path string }{
		{http.MethodPost, "/auth/logout"},
		{http.MethodGet, "/me"},
		{http.MethodGet, "/users"},
		{http.MethodPost, "/messages"},
		{http.MethodPost, "/messages/" + uuid.New().String() + "/read"},
		{http.MethodGet, "/conversations/" + someone + "/messages"},
		{http.MethodGet, "/events"},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			rec := do(t, h, r.method, r.path, `{"to":"x","body":"y"}`)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body)
			}
		})
	}
}

// Registering and logging in must work without a session; a 400 proves the
// request reached the handler rather than being routed elsewhere.
func TestAuthRoutesAreReachableWithoutASession(t *testing.T) {
	h := newRouter(t)

	for _, path := range []string{"/auth/register", "/auth/login"} {
		t.Run(path, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, path, `{`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 from the handler, body = %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestUnknownPathsAreNotFound(t *testing.T) {
	h := newRouter(t)

	for _, path := range []string{"/", "/nope", "/auth", "/messages/", "/conversations", "/me/extra"} {
		t.Run(path, func(t *testing.T) {
			if rec := do(t, h, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestRoutesAreBoundToOneMethod(t *testing.T) {
	h := newRouter(t)

	tests := []struct{ method, path string }{
		{http.MethodGet, "/auth/register"},
		{http.MethodGet, "/auth/login"},
		{http.MethodGet, "/auth/logout"},
		{http.MethodPost, "/me"},
		{http.MethodPost, "/users"},
		{http.MethodGet, "/messages"},
		{http.MethodDelete, "/events"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(t, h, tt.method, tt.path, "")
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", rec.Code)
			}
		})
	}
}
