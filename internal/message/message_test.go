//go:build integration

package message_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"fdrchat/internal/message"
	"fdrchat/internal/pgtest"
	"fdrchat/internal/user"
)

// These tests run the Store against a real PostgreSQL (started in a
// container), because the behaviour worth testing lives in the SQL.

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	p, stop, err := pgtest.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	pool = p
	return m.Run()
}

var ctx = context.Background()

func newUser(t *testing.T) user.User {
	t.Helper()
	return pgtest.NewUser(t, pool)
}

func send(t *testing.T, s *message.Store, from, to user.User, body string) message.Message {
	t.Helper()
	m, err := s.Create(ctx, from.ID, to.ID, body)
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	return m
}

type rowState struct {
	status    string
	delivered *time.Time
	read      *time.Time
}

// state reads the message row directly, bypassing the Store.
func state(t *testing.T, id uuid.UUID) rowState {
	t.Helper()
	var r rowState
	err := pool.QueryRow(ctx,
		`SELECT status, delivered_at, read_at FROM messages WHERE id = $1`, id,
	).Scan(&r.status, &r.delivered, &r.read)
	if err != nil {
		t.Fatalf("read message row: %v", err)
	}
	return r
}

func ids(msgs []message.Message) []uuid.UUID {
	out := make([]uuid.UUID, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func assertIDs(t *testing.T, got []message.Message, want ...message.Message) {
	t.Helper()
	g, w := ids(got), ids(want)
	if len(g) != len(w) {
		t.Fatalf("got %d messages %v, want %d %v", len(g), g, len(w), w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("message %d = %v, want %v (full: got %v, want %v)", i, g[i], w[i], g, w)
		}
	}
}

// ---- Create ----

func TestCreateStoresASavedMessage(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)

	m := send(t, s, alice, bob, "hello")

	if m.ID == uuid.Nil || m.SenderID != alice.ID || m.RecipientID != bob.ID || m.Body != "hello" {
		t.Fatalf("unexpected message: %+v", m)
	}
	if m.Status != message.StatusSaved || m.DeliveredAt != nil || m.ReadAt != nil || m.CreatedAt.IsZero() {
		t.Fatalf("a new message must be saved without timestamps: %+v", m)
	}
	if got := state(t, m.ID); got.status != "saved" {
		t.Fatalf("row status = %q, want saved", got.status)
	}
}

func TestCreateWithUnknownRecipientReturnsErrNoRecipient(t *testing.T) {
	s := message.NewStore(pool)
	alice := newUser(t)

	_, err := s.Create(ctx, alice.ID, uuid.New(), "hello")

	if err != message.ErrNoRecipient {
		t.Fatalf("err = %v, want ErrNoRecipient", err)
	}
}

// The handler already rejects these; the schema is the last line of defence.
func TestCreateIsRejectedByTheSchemaConstraints(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)

	if _, err := s.Create(ctx, alice.ID, alice.ID, "to myself"); err == nil {
		t.Error("a message to oneself must be rejected")
	}
	if _, err := s.Create(ctx, alice.ID, bob.ID, ""); err == nil {
		t.Error("an empty body must be rejected")
	}
	if _, err := s.Create(ctx, alice.ID, bob.ID, strings.Repeat("a", message.MaxBodyLen+1)); err == nil {
		t.Error("a body over MaxBodyLen must be rejected")
	}
	// The limit is on characters, not bytes.
	if _, err := s.Create(ctx, alice.ID, bob.ID, strings.Repeat("é", message.MaxBodyLen)); err != nil {
		t.Errorf("MaxBodyLen multibyte characters must be accepted: %v", err)
	}
}

// ---- MarkDelivered ----

func TestMarkDeliveredAdvancesASavedMessageOnce(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	m := send(t, s, alice, bob, "hi")

	c, changed, err := s.MarkDelivered(ctx, m.ID)
	if err != nil || !changed {
		t.Fatalf("first call: changed=%v err=%v, want changed", changed, err)
	}
	if c.MessageID != m.ID || c.SenderID != alice.ID || c.RecipientID != bob.ID ||
		c.Status != message.StatusDelivered || c.At.IsZero() {
		t.Fatalf("unexpected change: %+v", c)
	}
	row := state(t, m.ID)
	if row.status != "delivered" || row.delivered == nil || !row.delivered.Equal(c.At) || row.read != nil {
		t.Fatalf("row = %+v, want delivered at %v", row, c.At)
	}

	_, changed, err = s.MarkDelivered(ctx, m.ID)
	if err != nil || changed {
		t.Fatalf("second call: changed=%v err=%v, want unchanged and no error", changed, err)
	}
	if again := state(t, m.ID); !again.delivered.Equal(*row.delivered) {
		t.Fatal("a repeated delivery must not move delivered_at")
	}
}

func TestMarkDeliveredDoesNotDowngradeARead(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	m := send(t, s, alice, bob, "hi")
	if _, _, err := s.MarkRead(ctx, m.ID, bob.ID); err != nil {
		t.Fatal(err)
	}

	_, changed, err := s.MarkDelivered(ctx, m.ID)

	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want unchanged and no error", changed, err)
	}
	if got := state(t, m.ID); got.status != "read" || got.read == nil {
		t.Fatalf("row = %+v, want it to stay read", got)
	}
}

func TestMarkDeliveredOfUnknownMessageIsANoOp(t *testing.T) {
	s := message.NewStore(pool)

	_, changed, err := s.MarkDelivered(ctx, uuid.New())

	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want unchanged and no error", changed, err)
	}
}

// ---- MarkRead ----

func TestMarkReadByTheRecipientBackfillsDelivery(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	m := send(t, s, alice, bob, "hi")

	c, changed, err := s.MarkRead(ctx, m.ID, bob.ID)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want changed", changed, err)
	}
	if c.MessageID != m.ID || c.SenderID != alice.ID || c.RecipientID != bob.ID ||
		c.Status != message.StatusRead || c.At.IsZero() {
		t.Fatalf("unexpected change: %+v", c)
	}
	row := state(t, m.ID)
	if row.status != "read" || row.read == nil || !row.read.Equal(c.At) || row.delivered == nil {
		t.Fatalf("row = %+v, want read at %v with delivered_at backfilled", row, c.At)
	}
}

func TestMarkReadKeepsTheOriginalDeliveryTime(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	m := send(t, s, alice, bob, "hi")
	d, _, err := s.MarkDelivered(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, changed, err := s.MarkRead(ctx, m.ID, bob.ID); err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want changed", changed, err)
	}

	if row := state(t, m.ID); row.delivered == nil || !row.delivered.Equal(d.At) {
		t.Fatalf("delivered_at = %v, want the original %v", row.delivered, d.At)
	}
}

func TestMarkReadTwiceReportsNoChangeTheSecondTime(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	m := send(t, s, alice, bob, "hi")
	if _, _, err := s.MarkRead(ctx, m.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	first := state(t, m.ID)

	_, changed, err := s.MarkRead(ctx, m.ID, bob.ID)

	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want unchanged and no error", changed, err)
	}
	if second := state(t, m.ID); !second.read.Equal(*first.read) {
		t.Fatal("read_at must not move on a repeated read")
	}
}

func TestMarkReadIsOnlyPossibleForTheRecipient(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	m := send(t, s, alice, bob, "hi")

	for name, caller := range map[string]user.User{"sender": alice, "third party": carol} {
		if _, changed, err := s.MarkRead(ctx, m.ID, caller.ID); err != message.ErrNotFound || changed {
			t.Errorf("%s: changed=%v err=%v, want ErrNotFound", name, changed, err)
		}
	}
	if got := state(t, m.ID); got.status != "saved" {
		t.Fatalf("status = %q, want the message untouched", got.status)
	}
}

func TestMarkReadOfUnknownMessageReturnsErrNotFound(t *testing.T) {
	s := message.NewStore(pool)
	bob := newUser(t)

	if _, _, err := s.MarkRead(ctx, uuid.New(), bob.ID); err != message.ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---- Conversation ----

func TestConversationOfStrangersIsEmptyButNotNil(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)

	msgs, changes, err := s.Conversation(ctx, alice.ID, bob.ID, nil, 0)

	if err != nil {
		t.Fatal(err)
	}
	if msgs == nil || len(msgs) != 0 {
		t.Fatalf("msgs = %#v, want an empty non-nil slice (it is encoded as [])", msgs)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %v, want none", changes)
	}
}

func TestConversationIsOldestFirstAndScopedToThePair(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	m1 := send(t, s, alice, bob, "1")
	m2 := send(t, s, bob, alice, "2")
	send(t, s, alice, carol, "other conversation")
	send(t, s, carol, bob, "another conversation")
	m3 := send(t, s, alice, bob, "3")

	fromAlice, _, err := s.Conversation(ctx, alice.ID, bob.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, fromAlice, m1, m2, m3)

	fromBob, _, err := s.Conversation(ctx, bob.ID, alice.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, fromBob, m1, m2, m3)
}

// The query always puts the caller on one side of the pair, so naming two
// other users reveals nothing.
func TestConversationIsInvisibleToThirdParties(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	m := send(t, s, alice, bob, "just between us")

	for name, peer := range map[string]user.User{"alice": alice, "bob": bob} {
		msgs, changes, err := s.Conversation(ctx, carol.ID, peer.ID, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(msgs) != 0 || len(changes) != 0 {
			t.Fatalf("carol naming %s got msgs=%v changes=%v", name, ids(msgs), changes)
		}
	}
	if got := state(t, m.ID); got.status != "saved" {
		t.Fatalf("status = %q, a third party must not trigger delivery", got.status)
	}
}

func TestConversationWithAnUnknownUserIsEmpty(t *testing.T) {
	s := message.NewStore(pool)
	alice := newUser(t)

	msgs, changes, err := s.Conversation(ctx, alice.ID, uuid.New(), nil, 0)

	if err != nil || len(msgs) != 0 || len(changes) != 0 {
		t.Fatalf("msgs=%v changes=%v err=%v, want an empty conversation and no error", msgs, changes, err)
	}
}

func TestConversationMarksOnlyIncomingSavedMessagesAsDelivered(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	incoming1 := send(t, s, bob, alice, "to alice 1")
	incoming2 := send(t, s, bob, alice, "to alice 2")
	outgoing := send(t, s, alice, bob, "to bob")
	alreadyDelivered := send(t, s, bob, alice, "already delivered")
	if _, _, err := s.MarkDelivered(ctx, alreadyDelivered.ID); err != nil {
		t.Fatal(err)
	}

	msgs, changes, err := s.Conversation(ctx, alice.ID, bob.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	got := map[uuid.UUID]message.StatusChange{}
	for _, c := range changes {
		got[c.MessageID] = c
	}
	if len(got) != 2 {
		t.Fatalf("changes = %+v, want exactly the two newly delivered messages", changes)
	}
	for _, m := range []message.Message{incoming1, incoming2} {
		c, ok := got[m.ID]
		if !ok || c.SenderID != bob.ID || c.RecipientID != alice.ID || c.Status != message.StatusDelivered || c.At.IsZero() {
			t.Fatalf("change for %v = %+v (found=%v)", m.ID, c, ok)
		}
		if row := state(t, m.ID); row.status != "delivered" {
			t.Fatalf("row status = %q, want delivered", row.status)
		}
	}
	if row := state(t, outgoing.ID); row.status != "saved" {
		t.Fatalf("a message the caller sent must stay saved, got %q", row.status)
	}

	// The returned messages already reflect the new status.
	status := map[uuid.UUID]message.Status{}
	for _, m := range msgs {
		status[m.ID] = m.Status
	}
	if status[incoming1.ID] != message.StatusDelivered || status[outgoing.ID] != message.StatusSaved {
		t.Fatalf("returned statuses = %v", status)
	}

	// Loading it again has nothing left to deliver.
	_, again, err := s.Conversation(ctx, alice.ID, bob.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second load reported changes: %+v", again)
	}
}

func TestConversationDoesNotDowngradeReadMessages(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	m := send(t, s, bob, alice, "hi")
	if _, _, err := s.MarkRead(ctx, m.ID, alice.ID); err != nil {
		t.Fatal(err)
	}

	msgs, changes, err := s.Conversation(ctx, alice.ID, bob.ID, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || len(msgs) != 1 || msgs[0].Status != message.StatusRead {
		t.Fatalf("msgs = %+v, changes = %+v, want the message to stay read", msgs, changes)
	}
}

func TestConversationPagination(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	var sent []message.Message
	for i := range 5 {
		from, to := alice, bob
		if i%2 == 1 {
			from, to = bob, alice
		}
		sent = append(sent, send(t, s, from, to, fmt.Sprintf("m%d", i)))
	}

	newest, _, err := s.Conversation(ctx, alice.ID, bob.ID, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, newest, sent[3], sent[4])

	older, _, err := s.Conversation(ctx, alice.ID, bob.ID, &newest[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, older, sent[1], sent[2])

	oldest, _, err := s.Conversation(ctx, alice.ID, bob.ID, &older[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, oldest, sent[0])

	none, _, err := s.Conversation(ctx, alice.ID, bob.ID, &oldest[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, none)
}

func TestConversationCursorFromAnotherConversationYieldsNothing(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	send(t, s, alice, bob, "ours")
	foreign := send(t, s, alice, carol, "not ours")

	for name, cursor := range map[string]uuid.UUID{"someone else's message": foreign.ID, "unknown id": uuid.New()} {
		msgs, _, err := s.Conversation(ctx, alice.ID, bob.ID, &cursor, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(msgs) != 0 {
			t.Errorf("%s: got %d messages, want an empty page", name, len(msgs))
		}
	}
}

func TestConversationPageSizeDefaults(t *testing.T) {
	s := message.NewStore(pool)
	alice, bob := newUser(t), newUser(t)
	for i := 0; i < message.DefaultLimit+1; i++ {
		send(t, s, alice, bob, "m")
	}

	for name, limit := range map[string]int{
		"zero":           0,
		"negative":       -5,
		"above the max":  message.MaxLimit + 1,
		"explicit small": 3,
	} {
		msgs, _, err := s.Conversation(ctx, alice.ID, bob.ID, nil, limit)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := message.DefaultLimit
		if name == "explicit small" {
			want = 3
		}
		if len(msgs) != want {
			t.Errorf("%s: got %d messages, want %d", name, len(msgs), want)
		}
	}
}
