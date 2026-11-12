package session_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"fdrchat/internal/session"
)

func TestSetCookieUsesTheHardenedAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	expires := time.Now().Add(session.AbsoluteTimeout)

	session.SetCookie(rec, "the-token", session.Session{ExpiresAt: expires})

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want exactly one", cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-session" || c.Value != "the-token" {
		t.Fatalf("cookie = %s=%s", c.Name, c.Value)
	}
	// The __Host- prefix is only honoured with Secure, Path=/ and no Domain.
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("insecure cookie attributes: %+v", c)
	}
	maxAge := time.Duration(c.MaxAge) * time.Second
	if maxAge < session.AbsoluteTimeout-time.Minute || maxAge > session.AbsoluteTimeout {
		t.Fatalf("Max-Age = %v, want about %v", maxAge, session.AbsoluteTimeout)
	}
}

func TestClearCookieExpiresTheCookieInTheBrowser(t *testing.T) {
	rec := httptest.NewRecorder()

	session.ClearCookie(rec)

	header := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(header, session.CookieName+"=;") || !strings.Contains(header, "Max-Age=0") {
		t.Fatalf("Set-Cookie = %q, want an empty value with Max-Age=0", header)
	}
	// The browser only accepts a __Host- cookie update with these attributes.
	c := rec.Result().Cookies()[0]
	if !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Fatalf("clearing cookie must keep the __Host- attributes: %+v", c)
	}
}

func TestTokenFromRequest(t *testing.T) {
	tests := []struct {
		name    string
		cookies []*http.Cookie
		want    string
	}{
		{"no cookie", nil, ""},
		{"session cookie", []*http.Cookie{{Name: session.CookieName, Value: "abc"}}, "abc"},
		{"empty value", []*http.Cookie{{Name: session.CookieName, Value: ""}}, ""},
		{"other cookies are ignored", []*http.Cookie{{Name: "session", Value: "nope"}, {Name: "theme", Value: "dark"}}, ""},
		{"among others", []*http.Cookie{{Name: "theme", Value: "dark"}, {Name: session.CookieName, Value: "abc"}}, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for _, c := range tt.cookies {
				req.AddCookie(c)
			}
			if got := session.TokenFromRequest(req); got != tt.want {
				t.Fatalf("token = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionRoundTripsThroughTheContext(t *testing.T) {
	s := session.Session{ID: 7, UserID: uuid.New(), ExpiresAt: time.Now()}
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, ok := session.FromContext(req.Context()); ok {
		t.Fatal("an empty context must not report a session")
	}

	got, ok := session.FromContext(session.NewContext(req.Context(), s))
	if !ok || got != s {
		t.Fatalf("FromContext = %+v, %v, want %+v", got, ok, s)
	}
}
