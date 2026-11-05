//go:build integration

package user_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"fdrchat/internal/pgtest"
	"fdrchat/internal/user"
)

// These tests run the user Store and Handler against a real PostgreSQL
// (started in a container), because name handling lives in the SQL.

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

func uniqueName() string { return "u_" + uuid.NewString() }

// ---- Store ----

func TestCreateReturnsTheStoredUser(t *testing.T) {
	s := user.NewStore(pool)
	name := uniqueName()

	u, err := s.Create(ctx, name, "hash-1")

	if err != nil {
		t.Fatal(err)
	}
	if u.ID == uuid.Nil || u.Name != name || u.CreatedAt.IsZero() {
		t.Fatalf("unexpected user: %+v", u)
	}
}

func TestCreateRejectsTheSameNameRegardlessOfCase(t *testing.T) {
	s := user.NewStore(pool)
	name := "Alice_" + uuid.NewString()
	if _, err := s.Create(ctx, name, "hash-1"); err != nil {
		t.Fatal(err)
	}

	for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
		if _, err := s.Create(ctx, variant, "hash-2"); err != user.ErrNameTaken {
			t.Errorf("Create(%q) = %v, want ErrNameTaken", variant, err)
		}
	}
}

func TestGetByNameIsCaseInsensitiveAndReturnsTheHash(t *testing.T) {
	s := user.NewStore(pool)
	name := "Bob_" + uuid.NewString()
	created, err := s.Create(ctx, name, "bobs-hash")
	if err != nil {
		t.Fatal(err)
	}

	for _, lookup := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
		u, hash, err := s.GetByName(ctx, lookup)
		if err != nil {
			t.Fatalf("GetByName(%q): %v", lookup, err)
		}
		if u.ID != created.ID || u.Name != name || hash != "bobs-hash" {
			t.Fatalf("GetByName(%q) = %+v, %q", lookup, u, hash)
		}
	}
}

func TestGetByNameOfAnUnknownUserReturnsErrNotFound(t *testing.T) {
	s := user.NewStore(pool)

	if _, _, err := s.GetByName(ctx, uniqueName()); err != user.ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetByID(t *testing.T) {
	s := user.NewStore(pool)
	created, err := s.Create(ctx, uniqueName(), "hash")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetByID(ctx, created.ID)
	if err != nil || got.ID != created.ID || got.Name != created.Name {
		t.Fatalf("GetByID = %+v, %v, want %+v", got, err, created)
	}
	if _, err := s.GetByID(ctx, uuid.New()); err != user.ErrNotFound {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestListIsOrderedByNameIgnoringCase(t *testing.T) {
	s := user.NewStore(pool)
	suffix := uuid.NewString()
	// Created out of order, with mixed case: "B_" sorts before "c_" only
	// when case is ignored.
	for _, name := range []string{"c_" + suffix, "B_" + suffix, "a_" + suffix} {
		if _, err := s.Create(ctx, name, "hash"); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var ours []string
	for _, u := range list {
		if strings.HasSuffix(u.Name, suffix) {
			ours = append(ours, u.Name)
		}
	}
	want := []string{"a_" + suffix, "B_" + suffix, "c_" + suffix}
	if strings.Join(ours, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", ours, want)
	}
}

// ---- Handler ----

func TestListEndpointExposesOnlyIDAndName(t *testing.T) {
	s := user.NewStore(pool)
	created, err := s.Create(ctx, uniqueName(), "secret-hash")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	user.NewHandler(s).List(rec, httptest.NewRequest(http.MethodGet, "/users", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, entry := range list {
		if len(entry) != 2 || entry["id"] == nil || entry["name"] == nil {
			t.Fatalf("entry %v: want exactly id and name", entry)
		}
		if entry["id"] == created.ID.String() && entry["name"] == created.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("created user %v missing from %v", created.ID, list)
	}
	if strings.Contains(rec.Body.String(), "secret-hash") {
		t.Fatal("the password hash must never be listed")
	}
}

func TestListEndpointStoreFailureReturns500WithoutDetails(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel() // the query fails with a context error
	req := httptest.NewRequest(http.MethodGet, "/users", nil).WithContext(cancelled)

	rec := httptest.NewRecorder()
	user.NewHandler(user.NewStore(pool)).List(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "context") {
		t.Fatalf("internal error details leaked: %s", rec.Body)
	}
}
