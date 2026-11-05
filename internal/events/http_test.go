package events

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"fdrchat/internal/session"
	"fdrchat/internal/stream"
)

const (
	testToken   = "test-token"
	testSession = int64(42)
	wait        = 3 * time.Second
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

type fakeAuth struct {
	err      error
	gotToken string
}

func (f *fakeAuth) Authenticate(_ context.Context, token string) (session.Session, error) {
	f.gotToken = token
	return session.Session{}, f.err
}

func newRequest(withCookie bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	if withCookie {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: testToken})
	}
	return req
}

// withSession stands in for session.Manager.Require.
func withSession(r *http.Request, expires time.Time) *http.Request {
	s := session.Session{ID: testSession, ExpiresAt: expires}
	return r.WithContext(session.NewContext(r.Context(), s))
}

// serve runs h behind a stand-in for Require and returns the open response.
// The stream is closed when the test ends.
func serve(t *testing.T, h *Handler, expires time.Time) *http.Response {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Stream(w, withSession(r, expires))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: testToken})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp
}

// lines reads body line by line in the background; the channel is closed at EOF.
func lines(body io.Reader) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

func expectLine(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("stream ended, want line %q", want)
		}
		if got != want {
			t.Fatalf("line = %q, want %q", got, want)
		}
	case <-time.After(wait):
		t.Fatalf("timed out waiting for line %q", want)
	}
}

func expectEnded(t *testing.T, ch <-chan string) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream should have been closed by the server")
		}
	}
}

func TestStreamWithoutSessionInContextReturns401(t *testing.T) {
	h := NewHandler(stream.NewHub(), &fakeAuth{})
	rec := httptest.NewRecorder()
	h.Stream(rec, newRequest(true))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestStreamReturns503WhenHubIsClosed(t *testing.T) {
	hub := stream.NewHub()
	hub.Close()
	auth := &fakeAuth{}
	h := NewHandler(hub, auth)

	rec := httptest.NewRecorder()
	h.Stream(rec, withSession(newRequest(true), time.Now().Add(time.Hour)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if auth.gotToken != "" {
		t.Fatal("must not authenticate when the hub rejects the subscription")
	}
}

func TestStreamAuthenticationFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"session deleted before the stream started", session.ErrNotFound, http.StatusUnauthorized},
		{"store failure", errors.New("connection refused to 10.0.0.5"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &fakeAuth{err: tt.err}
			h := NewHandler(stream.NewHub(), auth)

			rec := httptest.NewRecorder()
			h.Stream(rec, withSession(newRequest(true), time.Now().Add(time.Hour)))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.want, rec.Body)
			}
			if auth.gotToken != testToken {
				t.Fatalf("authenticated token = %q, want %q", auth.gotToken, testToken)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.5") {
				t.Fatal("internal error details leaked to client")
			}
			if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
				t.Fatal("must not start an event stream on failure")
			}
		})
	}
}

func TestStreamSendsHeadersAndFramesEvents(t *testing.T) {
	hub := stream.NewHub()
	h := NewHandler(hub, &fakeAuth{})
	resp := serve(t, h, time.Now().Add(time.Hour))

	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	flushed := make(chan struct{})
	// The response arrives only after the handler subscribed, so this reaches the stream.
	hub.Publish([]int64{testSession}, stream.Event{
		Name:    "message",
		Data:    []byte(`{"id":1}`),
		OnFlush: func() { close(flushed) },
	})
	// Events for other sessions must not show up on this stream.
	hub.Publish([]int64{testSession + 1}, stream.Event{Name: "other", Data: []byte(`{}`)})
	hub.Publish([]int64{testSession}, stream.Event{Name: "status", Data: []byte(`{"id":2}`)})

	ch := lines(resp.Body)
	expectLine(t, ch, "event: message")
	expectLine(t, ch, `data: {"id":1}`)
	expectLine(t, ch, "")
	expectLine(t, ch, "event: status")
	expectLine(t, ch, `data: {"id":2}`)
	expectLine(t, ch, "")

	select {
	case <-flushed:
	case <-time.After(wait):
		t.Fatal("OnFlush was not called after the event was written")
	}
}

func TestStreamSendsHeartbeats(t *testing.T) {
	h := NewHandler(stream.NewHub(), &fakeAuth{})
	h.heartbeat = 10 * time.Millisecond
	resp := serve(t, h, time.Now().Add(time.Hour))

	ch := lines(resp.Body)
	expectLine(t, ch, ": heartbeat")
	expectLine(t, ch, "")
	expectLine(t, ch, ": heartbeat")
}

func TestStreamEndsWhenSessionExpires(t *testing.T) {
	h := NewHandler(stream.NewHub(), &fakeAuth{})
	resp := serve(t, h, time.Now().Add(150*time.Millisecond))

	expectEnded(t, lines(resp.Body))
}

func TestStreamEndsWhenItsSessionIsClosedInTheHub(t *testing.T) {
	hub := stream.NewHub()
	h := NewHandler(hub, &fakeAuth{})
	resp := serve(t, h, time.Now().Add(time.Hour))

	ch := lines(resp.Body)
	hub.CloseSession(testSession)
	expectEnded(t, ch)
}

func TestStreamEndsWhenTheHubShutsDown(t *testing.T) {
	hub := stream.NewHub()
	h := NewHandler(hub, &fakeAuth{})
	resp := serve(t, h, time.Now().Add(time.Hour))

	ch := lines(resp.Body)
	hub.Close()
	expectEnded(t, ch)
}

// noFlush hides the Flush method of the underlying writer.
type noFlush struct{ http.ResponseWriter }

func TestStreamReturnsWhenFlushingIsUnsupported(t *testing.T) {
	h := NewHandler(stream.NewHub(), &fakeAuth{})
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Stream(noFlush{rec}, withSession(newRequest(true), time.Now().Add(time.Hour)))
	}()

	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("handler must return instead of blocking when it cannot flush")
	}
}

// brokenWriter simulates a client that went away: writes fail.
type brokenWriter struct {
	header http.Header
	ready  chan struct{} // closed on the first flush, i.e. once the stream is open
}

func (w *brokenWriter) Header() http.Header       { return w.header }
func (w *brokenWriter) WriteHeader(int)           {}
func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (w *brokenWriter) Flush() {
	select {
	case <-w.ready:
	default:
		close(w.ready)
	}
}

func TestStreamEndsOnWriteError(t *testing.T) {
	hub := stream.NewHub()
	h := NewHandler(hub, &fakeAuth{})
	w := &brokenWriter{header: http.Header{}, ready: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Stream(w, withSession(newRequest(true), time.Now().Add(time.Hour)))
	}()

	select {
	case <-w.ready:
	case <-time.After(wait):
		t.Fatal("stream did not open")
	}
	hub.Publish([]int64{testSession}, stream.Event{Name: "message", Data: []byte(`{}`)})

	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("handler must return when writing to the client fails")
	}
}
