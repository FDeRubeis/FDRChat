//go:build integration

package session_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"fdrchat/internal/pgtest"
	"fdrchat/internal/session"
	"fdrchat/internal/stream"
	"fdrchat/internal/user"
)

// These tests run the session Manager against a real PostgreSQL (started in a
// container), because the timeouts and revocation live in the SQL.

var (
	pool *pgxpool.Pool
	ctx  = context.Background()
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(run(m))
}

func run(m *testing.M) int {
	p, stop, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	pool = p
	return m.Run()
}

func newManager() (*session.Manager, *stream.Hub) {
	hub := stream.NewHub()
	return session.NewManager(pool, hub), hub
}

func newUser(t *testing.T) user.User {
	t.Helper()
	return pgtest.NewUser(t, pool)
}

func login(t *testing.T, m *session.Manager, u user.User) (string, session.Session) {
	t.Helper()
	token, s, err := m.Create(ctx, u.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return token, s
}

func exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func countSessions(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func subscribe(t *testing.T, hub *stream.Hub, sessionID int64) *stream.Subscription {
	t.Helper()
	sub, ok := hub.Subscribe(ctx, sessionID)
	if !ok {
		t.Fatal("subscribe failed")
	}
	t.Cleanup(sub.Close)
	return sub
}

func closed(sub *stream.Subscription) bool {
	select {
	case <-sub.Context().Done():
		return true
	default:
		return false
	}
}

func assertNotFound(t *testing.T, m *session.Manager, token string) {
	t.Helper()
	if _, err := m.Authenticate(ctx, token); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Authenticate = %v, want ErrNotFound", err)
	}
}

// ---- Create / Authenticate ----

func TestCreateIssuesATokenThatAuthenticatesAndStoresOnlyItsHash(t *testing.T) {
	m, _ := newManager()
	u := newUser(t)

	token, s := login(t, m, u)

	if len(token) != 43 { // 32 random bytes, unpadded base64url
		t.Fatalf("token length = %d, want 43", len(token))
	}
	if s.ID == 0 || s.UserID != u.ID {
		t.Fatalf("session = %+v", s)
	}
	if d := time.Until(s.ExpiresAt); d < session.AbsoluteTimeout-time.Minute || d > session.AbsoluteTimeout {
		t.Fatalf("expires in %v, want about %v", d, session.AbsoluteTimeout)
	}

	got, err := m.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != s.ID || got.UserID != u.ID {
		t.Fatalf("Authenticate = %+v, want %+v", got, s)
	}

	var hash []byte
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE id = $1`, s.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if len(hash) != 32 || string(hash) == token {
		t.Fatalf("stored token_hash = %x, want a 32-byte SHA-256, not the token", hash)
	}
}

func TestEverySessionGetsItsOwnToken(t *testing.T) {
	m, _ := newManager()
	u := newUser(t)

	t1, s1 := login(t, m, u)
	t2, s2 := login(t, m, u)

	if t1 == t2 || s1.ID == s2.ID {
		t.Fatalf("sessions share token or id: %q/%q, %d/%d", t1, t2, s1.ID, s2.ID)
	}
}

func TestAuthenticateRejectsUnknownAndMalformedTokens(t *testing.T) {
	m, _ := newManager()
	for name, token := range map[string]string{
		"empty":                 "",
		"too short":             "short",
		"too long":              strings.Repeat("A", 44),
		"unknown, valid length": strings.Repeat("A", 43),
	} {
		t.Run(name, func(t *testing.T) { assertNotFound(t, m, token) })
	}
}

func TestAuthenticateSlidesTheIdleTimeout(t *testing.T) {
	m, _ := newManager()
	token, s := login(t, m, newUser(t))
	exec(t, `UPDATE sessions SET last_seen_at = now() - interval '9 hours' WHERE id = $1`, s.ID)

	if _, err := m.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}

	var refreshed bool
	if err := pool.QueryRow(ctx,
		`SELECT last_seen_at > now() - interval '1 minute' FROM sessions WHERE id = $1`, s.ID,
	).Scan(&refreshed); err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Fatal("last_seen_at was not refreshed")
	}
}

func TestAuthenticateDoesNotWriteOnEveryRequest(t *testing.T) {
	m, _ := newManager()
	token, s := login(t, m, newUser(t))
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM sessions WHERE id = $1`, s.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}

	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM sessions WHERE id = $1`, s.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("last_seen_at moved from %v to %v within the touch interval", before, after)
	}
}

// ---- Revoke ----

func TestRevokeEndsOnlyThatSessionAndItsStreams(t *testing.T) {
	m, hub := newManager()
	u := newUser(t)
	laptopToken, laptop := login(t, m, u)
	phoneToken, phone := login(t, m, u)
	laptopStream := subscribe(t, hub, laptop.ID)
	phoneStream := subscribe(t, hub, phone.ID)

	if err := m.Revoke(ctx, laptop.ID); err != nil {
		t.Fatal(err)
	}

	assertNotFound(t, m, laptopToken)
	if _, err := m.Authenticate(ctx, phoneToken); err != nil {
		t.Fatalf("other session: %v", err)
	}
	if !closed(laptopStream) {
		t.Fatal("the revoked session's stream must be closed")
	}
	if closed(phoneStream) {
		t.Fatal("another session's stream must stay open")
	}
	if n := countSessions(t, u.ID); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestRevokeOfAnUnknownSessionIsNotAnError(t *testing.T) {
	m, _ := newManager()

	if err := m.Revoke(ctx, -1); err != nil {
		t.Fatalf("Revoke = %v, want nil", err)
	}
}

// ---- DeleteExpired ----

func TestDeleteExpiredRemovesOnlyExpiredSessionsAndClosesTheirStreams(t *testing.T) {
	tests := []struct {
		name   string
		expire string
	}{
		{"absolute timeout", `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`},
		{"idle timeout", `UPDATE sessions SET last_seen_at = now() - interval '10 hours 1 second' WHERE id = $1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, hub := newManager()
			u := newUser(t)
			staleToken, stale := login(t, m, u)
			activeToken, active := login(t, m, u)
			staleStream := subscribe(t, hub, stale.ID)
			activeStream := subscribe(t, hub, active.ID)
			exec(t, tt.expire, stale.ID)

			n, err := m.DeleteExpired(ctx)
			if err != nil {
				t.Fatal(err)
			}

			if n < 1 {
				t.Fatalf("deleted = %d, want at least 1", n)
			}
			assertNotFound(t, m, staleToken)
			if _, err := m.Authenticate(ctx, activeToken); err != nil {
				t.Fatalf("active session: %v", err)
			}
			if !closed(staleStream) || closed(activeStream) {
				t.Fatalf("stale stream closed = %v, active stream closed = %v; want true, false",
					closed(staleStream), closed(activeStream))
			}
			if n := countSessions(t, u.ID); n != 1 {
				t.Fatalf("sessions = %d, want 1", n)
			}
		})
	}
}

func TestDeleteExpiredLeavesLiveSessionsAlone(t *testing.T) {
	m, _ := newManager()
	u := newUser(t)
	token, _ := login(t, m, u)

	if _, err := m.DeleteExpired(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Authenticate(ctx, token); err != nil {
		t.Fatalf("live session was removed: %v", err)
	}
}

// Documents current behaviour: expiry is enforced by the cleanup job, not by
// the lookup, so a session stays usable until the next cleanup tick.
func TestAuthenticateAcceptsAnExpiredSessionUntilItIsCleanedUp(t *testing.T) {
	m, _ := newManager()
	token, s := login(t, m, newUser(t))
	exec(t, `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.ID)

	if _, err := m.Authenticate(ctx, token); err != nil {
		t.Fatalf("Authenticate = %v, want the session to still resolve", err)
	}

	if _, err := m.DeleteExpired(ctx); err != nil {
		t.Fatal(err)
	}
	assertNotFound(t, m, token)
}

// ---- RunCleanup ----

func TestRunCleanupRemovesExpiredSessionsUntilTheContextEnds(t *testing.T) {
	m, hub := newManager()
	token, s := login(t, m, newUser(t))
	sub := subscribe(t, hub, s.ID)
	exec(t, `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.ID)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.RunCleanup(runCtx, time.Millisecond)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if _, err := m.Authenticate(ctx, token); errors.Is(err, session.ErrNotFound) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the expired session was not cleaned up")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !closed(sub) {
		t.Fatal("the cleaned up session's stream must be closed")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCleanup must return when its context is cancelled")
	}
}

func TestRunCleanupReturnsImmediatelyOnACancelledContext(t *testing.T) {
	m, _ := newManager()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.RunCleanup(cancelled, time.Hour)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCleanup must return on a cancelled context")
	}
}

// ---- IDsForUser ----

func TestIDsForUserReturnsEverySessionOfThatUserOnly(t *testing.T) {
	m, _ := newManager()
	alice, bob, nobody := newUser(t), newUser(t), newUser(t)
	_, a1 := login(t, m, alice)
	_, a2 := login(t, m, alice)
	_, b1 := login(t, m, bob)

	got, err := m.IDsForUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []int64{a1.ID, a2.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("alice ids = %v, want %v (bob has %d)", got, want, b1.ID)
	}

	if none, err := m.IDsForUser(ctx, nobody.ID); err != nil || len(none) != 0 {
		t.Fatalf("user without sessions: ids = %v, err = %v", none, err)
	}
}

// ---- Require ----

type nextCall struct {
	called  bool
	session session.Session
	ok      bool
}

func (n *nextCall) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.called = true
		n.session, n.ok = session.FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
}

func requireRequest(ctx context.Context, token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	}
	return req
}

func TestRequireLetsAValidSessionThroughAndExposesIt(t *testing.T) {
	m, _ := newManager()
	token, s := login(t, m, newUser(t))
	next := &nextCall{}

	rec := httptest.NewRecorder()
	m.Require(next.handler()).ServeHTTP(rec, requireRequest(ctx, token))

	if rec.Code != http.StatusNoContent || !next.called {
		t.Fatalf("status = %d, next called = %v", rec.Code, next.called)
	}
	if !next.ok || next.session.ID != s.ID || next.session.UserID != s.UserID {
		t.Fatalf("session in context = %+v (ok=%v), want %+v", next.session, next.ok, s)
	}
}

func TestRequireRejectsMissingUnknownAndMalformedTokens(t *testing.T) {
	m, _ := newManager()
	tests := []struct {
		name         string
		token        string
		clearsCookie bool
	}{
		{"no cookie", "", false},
		{"unknown token", strings.Repeat("A", 43), true},
		{"malformed token", "short", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &nextCall{}
			rec := httptest.NewRecorder()

			m.Require(next.handler()).ServeHTTP(rec, requireRequest(ctx, tt.token))

			if rec.Code != http.StatusUnauthorized || next.called {
				t.Fatalf("status = %d, next called = %v, want 401 and not called", rec.Code, next.called)
			}
			cleared := strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0")
			if cleared != tt.clearsCookie {
				t.Fatalf("cookie cleared = %v, want %v", cleared, tt.clearsCookie)
			}
		})
	}
}

func TestRequireRejectsARevokedSessionAndClearsTheCookie(t *testing.T) {
	m, _ := newManager()
	token, s := login(t, m, newUser(t))
	if err := m.Revoke(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	next := &nextCall{}

	rec := httptest.NewRecorder()
	m.Require(next.handler()).ServeHTTP(rec, requireRequest(ctx, token))

	if rec.Code != http.StatusUnauthorized || next.called {
		t.Fatalf("status = %d, next called = %v", rec.Code, next.called)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatal("a rejected token's cookie should be cleared")
	}
}

func TestRequireStoreFailureReturns500WithoutDetails(t *testing.T) {
	m, _ := newManager()
	token, _ := login(t, m, newUser(t))
	cancelled, cancel := context.WithCancel(ctx)
	cancel() // the lookup fails with a context error, not "not found"
	next := &nextCall{}

	rec := httptest.NewRecorder()
	m.Require(next.handler()).ServeHTTP(rec, requireRequest(cancelled, token))

	if rec.Code != http.StatusInternalServerError || next.called {
		t.Fatalf("status = %d, next called = %v, want 500 and not called", rec.Code, next.called)
	}
	if strings.Contains(rec.Body.String(), "context") {
		t.Fatalf("internal error details leaked: %s", rec.Body)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("a transient failure must not clear the user's cookie")
	}
}
