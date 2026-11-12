package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"fdrchat/internal/session"
	"fdrchat/internal/user"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// userCreate records one call to UserStore.Create.
type userCreate struct {
	name         string
	passwordHash string
}

type fakeUsers struct {
	createErr error
	created   []userCreate

	getByName func(name string) (user.User, string, error)
	getByID   func(id uuid.UUID) (user.User, error)
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{}
}

// Create records the arguments and returns a user echoing the name.
func (f *fakeUsers) Create(_ context.Context, name, passwordHash string) (user.User, error) {
	f.created = append(f.created, userCreate{name: name, passwordHash: passwordHash})
	if f.createErr != nil {
		return user.User{}, f.createErr
	}
	return user.User{ID: uuid.New(), Name: name, CreatedAt: time.Now()}, nil
}

func (f *fakeUsers) GetByName(_ context.Context, name string) (user.User, string, error) {
	if f.getByName != nil {
		return f.getByName(name)
	}
	return user.User{}, "", user.ErrNotFound
}

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (user.User, error) {
	if f.getByID != nil {
		return f.getByID(id)
	}
	return user.User{}, user.ErrNotFound
}

type fakeSessions struct {
	createErr   error
	authSession session.Session
	authErr     error
	revokeErr   error

	created []uuid.UUID
	revoked []int64
}

func (f *fakeSessions) Create(_ context.Context, userID uuid.UUID) (string, session.Session, error) {
	f.created = append(f.created, userID)
	if f.createErr != nil {
		return "", session.Session{}, f.createErr
	}
	return "new-token", session.Session{ID: 100, UserID: userID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *fakeSessions) Authenticate(context.Context, string) (session.Session, error) {
	return f.authSession, f.authErr
}

func (f *fakeSessions) Revoke(_ context.Context, id int64) error {
	f.revoked = append(f.revoked, id)
	return f.revokeErr
}

func register(t *testing.T, h *Handler, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.Register(rec, req)
	return rec
}

func newTestHandler(users UserStore) *Handler {
	return NewHandler(users, nil)
}

func TestRegisterSuccess(t *testing.T) {
	users := newFakeUsers()
	h := newTestHandler(users)

	rec := register(t, h, "application/json", `{"name":"alice","password":"s3cret-pass"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "alice" {
		t.Fatalf("name = %v, want alice", got["name"])
	}
	if _, leaked := got["password_hash"]; leaked {
		t.Fatal("response must not contain the password hash")
	}

	if len(users.created) != 1 {
		t.Fatalf("created %d users, want 1", len(users.created))
	}
	createdUser := users.created[0]
	if createdUser.name != "alice" {
		t.Fatalf("created name = %q, want alice", createdUser.name)
	}
	ok, err := VerifyPassword("s3cret-pass", createdUser.passwordHash)
	if err != nil || !ok {
		t.Fatalf("passed hash does not verify: ok=%v err=%v", ok, err)
	}
}

func TestRegisterNameTakenReturnsConflict(t *testing.T) {
	users := newFakeUsers()
	users.createErr = user.ErrNameTaken

	rec := register(t, newTestHandler(users), "application/json", `{"name":"Alice","password":"other-pass"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if len(users.created) != 1 || users.created[0].name != "Alice" {
		t.Fatalf("created = %+v, want the submitted name forwarded unchanged", users.created)
	}
}

func TestRegisterRequestValidate(t *testing.T) {
	const (
		nameErr     = "name must be 3-32 characters: letters, digits, '_' or '-'"
		passwordErr = "password must be 8-128 characters"
	)
	tests := []struct {
		name     string
		userName string
		password string
		wantErr  string
	}{
		{"name too short", "al", "s3cret-pass", nameErr},
		{"name at minimum length", "ali", "s3cret-pass", ""},
		{"name with space", "al ice", "s3cret-pass", nameErr},
		{"name at maximum length", strings.Repeat("a", 32), "s3cret-pass", ""},
		{"name too long", strings.Repeat("a", 33), "s3cret-pass", nameErr},
		{"password too short", "alice", strings.Repeat("a", 7), passwordErr},
		{"password at minimum length", "alice", strings.Repeat("a", 8), ""},
		{"password at maximum length", "alice", strings.Repeat("a", 128), ""},
		{"password too long", "alice", strings.Repeat("a", 129), passwordErr},
		{"password counted in runes", "alice", strings.Repeat("é", 8), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := registerRequest{Name: tt.userName, Password: tt.password}.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("validate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{"malformed json", "application/json", `{"name":`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"name":"alice","password":"s3cret-pass","admin":true}`, http.StatusBadRequest},
		{"trailing data", "application/json", `{"name":"alice","password":"s3cret-pass"}{}`, http.StatusBadRequest},
		{"name too short", "application/json", `{"name":"al","password":"s3cret-pass"}`, http.StatusUnprocessableEntity},
		{"body too large", "application/json", `{"name":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users := newFakeUsers()
			rec := register(t, newTestHandler(users), tt.contentType, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.want, rec.Body)
			}
			if len(users.created) != 0 {
				t.Fatalf("created = %+v, want no call", users.created)
			}
		})
	}
}

func TestRegisterStoreFailureReturns500WithoutDetails(t *testing.T) {
	users := newFakeUsers()
	users.createErr = errors.New("connection refused to 10.0.0.5")
	rec := register(t, newTestHandler(users), "application/json", `{"name":"alice","password":"s3cret-pass"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatal("internal error details leaked to client")
	}
}

func login(h *Handler, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.Login(rec, req)
	return rec
}

// usersWithPassword returns a store in which "alice" exists with password s3cret-pass.
func usersWithPassword(t *testing.T) *fakeUsers {
	t.Helper()
	hash, err := HashPassword("s3cret-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u := user.User{ID: uuid.New(), Name: "alice", CreatedAt: time.Now()}
	users := newFakeUsers()
	users.getByName = func(string) (user.User, string, error) { return u, hash, nil }
	return users
}

const loginBody = `{"name":"alice","password":"s3cret-pass"}`

func assertInternalError(t *testing.T, rec *httptest.ResponseRecorder, secret string) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("internal error details leaked to client")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("no cookie must be set on failure, got %v", rec.Result().Cookies())
	}
}

func TestLoginRejectsMalformedJSON(t *testing.T) {
	h := NewHandler(newFakeUsers(), &fakeSessions{})
	if rec := login(h, `{"name":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestLoginUserLookupFailureReturns500WithoutDetails(t *testing.T) {
	users := newFakeUsers()
	users.getByName = func(string) (user.User, string, error) {
		return user.User{}, "", errors.New("connection refused to 10.0.0.5")
	}
	sessions := &fakeSessions{}
	rec := login(NewHandler(users, sessions), loginBody)

	assertInternalError(t, rec, "10.0.0.5")
	if len(sessions.created) != 0 {
		t.Fatal("no session must be created when the lookup fails")
	}
}

func TestLoginCorruptStoredHashReturns500(t *testing.T) {
	users := newFakeUsers()
	users.getByName = func(string) (user.User, string, error) {
		return user.User{ID: uuid.New(), Name: "alice"}, "not-a-phc-hash", nil
	}
	sessions := &fakeSessions{}
	rec := login(NewHandler(users, sessions), loginBody)

	assertInternalError(t, rec, ErrInvalidHash.Error())
	if len(sessions.created) != 0 {
		t.Fatal("no session must be created when verification errors")
	}
}

func TestLoginSessionCreateFailureReturns500WithoutDetails(t *testing.T) {
	users := usersWithPassword(t)
	sessions := &fakeSessions{createErr: errors.New("connection refused to 10.0.0.5")}
	rec := login(NewHandler(users, sessions), loginBody)

	assertInternalError(t, rec, "10.0.0.5")
	u, _, err := users.GetByName(context.Background(), "alice")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if len(sessions.created) != 1 || sessions.created[0] != u.ID {
		t.Fatalf("created = %v, want [%v]", sessions.created, u.ID)
	}
}

func TestLoginSucceedsEvenIfRevokingPreviousSessionFails(t *testing.T) {
	sessions := &fakeSessions{
		authSession: session.Session{ID: 7},
		revokeErr:   errors.New("db down"),
	}
	old := &http.Cookie{Name: session.CookieName, Value: "old-token"}
	rec := login(NewHandler(usersWithPassword(t), sessions), loginBody, old)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != 7 {
		t.Fatalf("revoked = %v, want [7]", sessions.revoked)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != session.CookieName || cookies[0].Value != "new-token" {
		t.Fatalf("cookies = %v, want fresh session cookie", cookies)
	}
}

func TestLoginDoesNotRevokeWhenPreviousSessionIsInvalid(t *testing.T) {
	sessions := &fakeSessions{authErr: session.ErrNotFound}
	old := &http.Cookie{Name: session.CookieName, Value: "stale-token"}
	rec := login(NewHandler(usersWithPassword(t), sessions), loginBody, old)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	if len(sessions.revoked) != 0 {
		t.Fatalf("revoked = %v, want none", sessions.revoked)
	}
}

func TestLogoutRevokeFailureReturns500AndKeepsCookie(t *testing.T) {
	sessions := &fakeSessions{revokeErr: errors.New("connection refused to 10.0.0.5")}
	h := NewHandler(newFakeUsers(), sessions)

	rec := httptest.NewRecorder()
	h.Logout(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))

	assertInternalError(t, rec, "10.0.0.5")
	if len(sessions.revoked) != 1 {
		t.Fatalf("revoke calls = %d, want 1", len(sessions.revoked))
	}
}

func TestMeErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"user deleted", user.ErrNotFound, http.StatusUnauthorized},
		{"store failure", errors.New("connection refused to 10.0.0.5"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users := newFakeUsers()
			users.getByID = func(uuid.UUID) (user.User, error) { return user.User{}, tt.err }
			h := NewHandler(users, &fakeSessions{})

			rec := httptest.NewRecorder()
			h.Me(rec, httptest.NewRequest(http.MethodGet, "/me", nil))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// ---- success paths and credential handling ----

func withSession(r *http.Request, s session.Session) *http.Request {
	return r.WithContext(session.NewContext(r.Context(), s))
}

func TestLoginSetsTheSessionCookieAndReturnsTheUser(t *testing.T) {
	users := usersWithPassword(t)
	sessions := &fakeSessions{}

	rec := login(NewHandler(users, sessions), loginBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want exactly one", cookies)
	}
	c := cookies[0]
	if c.Name != session.CookieName {
		t.Fatalf("unexpected cookie: %+v", c)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "alice" {
		t.Fatalf("unexpected body: %v", got)
	}

	if len(sessions.created) != 1 || sessions.created[0].String() != got["id"] {
		t.Fatalf("session created for %v, want the logged-in user %v", sessions.created, got["id"])
	}
}

// A client must not be able to tell a wrong password from an unknown name.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	wrongPassword := login(NewHandler(usersWithPassword(t), &fakeSessions{}),
		`{"name":"alice","password":"wrong-password"}`)
	sessions := &fakeSessions{}
	unknownUser := login(NewHandler(newFakeUsers(), sessions), loginBody)

	for name, rec := range map[string]*httptest.ResponseRecorder{"wrong password": wrongPassword, "unknown user": unknownUser} {
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: a failed login must not set a cookie", name)
		}
	}
	if wrongPassword.Body.String() != unknownUser.Body.String() {
		t.Fatalf("bodies differ: %q vs %q", wrongPassword.Body, unknownUser.Body)
	}
	if len(sessions.created) != 0 {
		t.Fatal("no session may be created for an unknown user")
	}
}

func TestLoginRevokesThePreviousSessionOfTheClient(t *testing.T) {
	sessions := &fakeSessions{authSession: session.Session{ID: 7}}
	old := &http.Cookie{Name: session.CookieName, Value: "old-token"}

	rec := login(NewHandler(usersWithPassword(t), sessions), loginBody, old)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != 7 {
		t.Fatalf("revoked = %v, want [7]", sessions.revoked)
	}
}

func TestLogoutRevokesTheCurrentSessionAndClearsTheCookie(t *testing.T) {
	sessions := &fakeSessions{}
	h := NewHandler(newFakeUsers(), sessions)

	rec := httptest.NewRecorder()
	h.Logout(rec, withSession(httptest.NewRequest(http.MethodPost, "/auth/logout", nil), session.Session{ID: 5}))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != 5 {
		t.Fatalf("revoked = %v, want [5]", sessions.revoked)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout must clear the cookie, got %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestMeReturnsTheProfileOfTheSessionUser(t *testing.T) {
	sessionUser := uuid.New()
	want := user.User{ID: sessionUser, Name: "alice", CreatedAt: time.Now()}
	users := newFakeUsers()
	users.getByID = func(uuid.UUID) (user.User, error) { return want, nil }
	h := NewHandler(users, &fakeSessions{})

	rec := httptest.NewRecorder()
	h.Me(rec, withSession(httptest.NewRequest(http.MethodGet, "/me", nil), session.Session{ID: 1, UserID: sessionUser}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != sessionUser.String() || got["name"] != "alice" {
		t.Fatalf("unexpected body: %v", got)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatal("/me must not expose the password hash")
	}
}
