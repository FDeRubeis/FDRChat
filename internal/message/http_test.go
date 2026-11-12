package message

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"fdrchat/internal/session"
	"fdrchat/internal/stream"
)

// init rather than TestMain: the integration tests of this package define
// their own TestMain, and a test binary can only have one.
func init() {
	slog.SetDefault(slog.New(slog.DiscardHandler))
}

type fakeSessions struct {
	ids map[uuid.UUID][]int64
	err error
}

func (f fakeSessions) IDsForUser(_ context.Context, userID uuid.UUID) ([]int64, error) {
	return f.ids[userID], f.err
}

type publication struct {
	sessionIDs []int64
	event      stream.Event
}

type recordingPublisher struct {
	calls []publication
}

func (p *recordingPublisher) Publish(sessionIDs []int64, e stream.Event) {
	p.calls = append(p.calls, publication{sessionIDs, e})
}

// fakeStore records its arguments and returns what the test configured.
type fakeStore struct {
	createErr error
	created   []createCall

	convMsgs    []Message
	convChanges []StatusChange
	convErr     error
	convCalls   []convCall

	deliveredChange  StatusChange
	deliveredChanged bool
	deliveredErr     error
	deliveredIDs     []uuid.UUID

	readChange  StatusChange
	readChanged bool
	readErr     error
	readCalls   []readCall
}

type createCall struct {
	sender, recipient uuid.UUID
	body              string
}

type convCall struct {
	user, peer uuid.UUID
	before     *uuid.UUID
	limit      int
}

type readCall struct{ id, recipient uuid.UUID }

func (f *fakeStore) Create(_ context.Context, senderID, recipientID uuid.UUID, body string) (Message, error) {
	f.created = append(f.created, createCall{senderID, recipientID, body})
	if f.createErr != nil {
		return Message{}, f.createErr
	}
	return Message{
		ID: uuid.New(), SenderID: senderID, RecipientID: recipientID,
		Body: body, Status: StatusSaved, CreatedAt: time.Now(),
	}, nil
}

func (f *fakeStore) Conversation(_ context.Context, userID, peerID uuid.UUID, before *uuid.UUID, limit int) ([]Message, []StatusChange, error) {
	f.convCalls = append(f.convCalls, convCall{userID, peerID, before, limit})
	return f.convMsgs, f.convChanges, f.convErr
}

func (f *fakeStore) MarkDelivered(_ context.Context, id uuid.UUID) (StatusChange, bool, error) {
	f.deliveredIDs = append(f.deliveredIDs, id)
	return f.deliveredChange, f.deliveredChanged, f.deliveredErr
}

func (f *fakeStore) MarkRead(_ context.Context, id, recipientID uuid.UUID) (StatusChange, bool, error) {
	f.readCalls = append(f.readCalls, readCall{id, recipientID})
	return f.readChange, f.readChanged, f.readErr
}

// env wires a Handler to fakes. alice is the authenticated caller.
type env struct {
	h        *Handler
	store    *fakeStore
	pub      *recordingPublisher
	alice    session.Session
	bob      uuid.UUID
	sessions map[uuid.UUID][]int64
}

func newEnv() *env {
	alice := session.Session{ID: 1, UserID: uuid.New()}
	bob := uuid.New()
	sessions := map[uuid.UUID][]int64{
		alice.UserID: {1, 2},
		bob:          {10, 11},
	}
	store, pub := &fakeStore{}, &recordingPublisher{}
	return &env{
		h:        NewHandler(store, fakeSessions{ids: sessions}, pub),
		store:    store,
		pub:      pub,
		alice:    alice,
		bob:      bob,
		sessions: sessions,
	}
}

func (e *env) request(method, target, body string, pathValues map[string]string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	return req.WithContext(session.NewContext(req.Context(), e.alice))
}

func (e *env) send(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.Send(rec, e.request(http.MethodPost, "/messages", body, nil))
	return rec
}

func (e *env) conversation(peer, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.Conversation(rec, e.request(http.MethodGet, "/conversations/"+peer+"/messages"+query, "", map[string]string{"userID": peer}))
	return rec
}

func (e *env) markRead(id string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.h.MarkRead(rec, e.request(http.MethodPost, "/messages/"+id+"/read", "", map[string]string{"id": id}))
	return rec
}

func sendBody(to, body string) string {
	b, _ := json.Marshal(map[string]string{"to": to, "body": body})
	return string(b)
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, want, rec.Body)
	}
}

func assertInternalError(t *testing.T, rec *httptest.ResponseRecorder, secret string) {
	t.Helper()
	assertStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("internal error details leaked to client")
	}
}

func assertNoPublish(t *testing.T, pub *recordingPublisher) {
	t.Helper()
	if len(pub.calls) != 0 {
		t.Fatalf("unexpected publications: %+v", pub.calls)
	}
}

func decodeEvent(t *testing.T, e stream.Event) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(e.Data, &got); err != nil {
		t.Fatalf("event data is not JSON: %v (%s)", err, e.Data)
	}
	return got
}

// ---- publish ----

func TestLivePushGoesToTheSessionsOfTheAddressedUserOnly(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	pub := &recordingPublisher{}
	h := NewHandler(nil, fakeSessions{ids: map[uuid.UUID][]int64{
		alice: {1, 2},
		bob:   {3},
	}}, pub)

	h.publish(alice, stream.Event{Name: EventStatus})

	if len(pub.calls) != 1 || !reflect.DeepEqual(pub.calls[0].sessionIDs, []int64{1, 2}) {
		t.Fatalf("published to %+v, want one call to [1 2]", pub.calls)
	}
}

// The message is stored before the push, so losing the lookup must cost only
// the live delivery, not the request.
func TestFailedSessionLookupSkipsTheLivePushWithoutPanicking(t *testing.T) {
	pub := &recordingPublisher{}
	h := NewHandler(nil, fakeSessions{err: errors.New("database down")}, pub)

	h.publish(uuid.New(), stream.Event{Name: EventMessage})

	assertNoPublish(t, pub)
}

func TestPublishStatusRoutesToTheSenderAndOmitsSenderIDFromPayload(t *testing.T) {
	e := newEnv()
	c := StatusChange{
		SenderID: e.alice.UserID, MessageID: uuid.New(), RecipientID: e.bob,
		Status: StatusDelivered, At: time.Now(),
	}

	e.h.publishStatus(c)

	if len(e.pub.calls) != 1 {
		t.Fatalf("publications = %d, want 1", len(e.pub.calls))
	}
	call := e.pub.calls[0]
	if !reflect.DeepEqual(call.sessionIDs, e.sessions[e.alice.UserID]) {
		t.Fatalf("routed to %v, want the sender's sessions %v", call.sessionIDs, e.sessions[e.alice.UserID])
	}
	if call.event.Name != EventStatus {
		t.Fatalf("event name = %q, want %q", call.event.Name, EventStatus)
	}
	if call.event.OnFlush != nil {
		t.Fatal("status events must not trigger delivery tracking")
	}
	got := decodeEvent(t, call.event)
	if got["message_id"] != c.MessageID.String() || got["recipient_id"] != e.bob.String() || got["status"] != "delivered" || got["at"] == nil {
		t.Fatalf("unexpected payload: %v", got)
	}
	if _, leaked := got["sender_id"]; leaked {
		t.Fatal("sender_id is routing information and must not be in the payload")
	}
}

func TestOnFlushMarksTheMessageDeliveredAndNotifiesTheSender(t *testing.T) {
	e := newEnv()
	m := Message{ID: uuid.New(), SenderID: e.alice.UserID, RecipientID: e.bob, Body: "hi", Status: StatusSaved}
	e.store.deliveredChanged = true
	e.store.deliveredChange = StatusChange{
		SenderID: e.alice.UserID, MessageID: m.ID, RecipientID: e.bob,
		Status: StatusDelivered, At: time.Now(),
	}

	e.h.publishMessage(m)
	if len(e.pub.calls) != 1 || e.pub.calls[0].event.OnFlush == nil {
		t.Fatalf("expected one message event with an OnFlush hook, got %+v", e.pub.calls)
	}
	if len(e.store.deliveredIDs) != 0 {
		t.Fatal("must not be marked delivered before a stream has flushed it")
	}

	e.pub.calls[0].event.OnFlush()

	if !reflect.DeepEqual(e.store.deliveredIDs, []uuid.UUID{m.ID}) {
		t.Fatalf("delivered ids = %v, want [%v]", e.store.deliveredIDs, m.ID)
	}
	if len(e.pub.calls) != 2 {
		t.Fatalf("publications = %d, want message + status", len(e.pub.calls))
	}
	status := e.pub.calls[1]
	if status.event.Name != EventStatus || !reflect.DeepEqual(status.sessionIDs, e.sessions[e.alice.UserID]) {
		t.Fatalf("status event = %+v, want a status event to the sender", status)
	}
}

func TestMarkDeliveredStaysQuietWhenNothingChangedOrTheStoreFails(t *testing.T) {
	tests := []struct {
		name    string
		changed bool
		err     error
	}{
		{"already delivered or read", false, nil},
		{"store failure", false, errors.New("database down")},
		{"store failure with stale change flag", true, errors.New("database down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			e.store.deliveredChanged = tt.changed
			e.store.deliveredErr = tt.err

			e.h.markDelivered(uuid.New())

			assertNoPublish(t, e.pub)
		})
	}
}

// ---- Send ----

func TestSendStoresTrimmedBodyAndPushesToTheRecipient(t *testing.T) {
	e := newEnv()
	rec := e.send(sendBody(e.bob.String(), "  hello bob \n"))

	assertStatus(t, rec, http.StatusCreated)

	if len(e.store.created) != 1 {
		t.Fatalf("store.Create calls = %d, want 1", len(e.store.created))
	}
	want := createCall{sender: e.alice.UserID, recipient: e.bob, body: "hello bob"}
	if e.store.created[0] != want {
		t.Fatalf("Create(%+v), want %+v", e.store.created[0], want)
	}

	var resp Message
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Body != "hello bob" || resp.SenderID != e.alice.UserID || resp.RecipientID != e.bob || resp.Status != StatusSaved {
		t.Fatalf("unexpected response: %+v", resp)
	}

	if len(e.pub.calls) != 1 {
		t.Fatalf("publications = %d, want 1", len(e.pub.calls))
	}
	call := e.pub.calls[0]
	if !reflect.DeepEqual(call.sessionIDs, e.sessions[e.bob]) {
		t.Fatalf("pushed to %v, want the recipient's sessions %v", call.sessionIDs, e.sessions[e.bob])
	}
	if call.event.Name != EventMessage || call.event.OnFlush == nil {
		t.Fatalf("event = %+v, want a message event with an OnFlush hook", call.event)
	}
	if got := decodeEvent(t, call.event); got["id"] != resp.ID.String() || got["body"] != "hello bob" {
		t.Fatalf("event payload = %v", got)
	}
}

func TestSendLimitCountsCharactersNotBytes(t *testing.T) {
	e := newEnv()
	if rec := e.send(sendBody(e.bob.String(), strings.Repeat("é", MaxBodyLen))); rec.Code != http.StatusCreated {
		t.Fatalf("exactly MaxBodyLen multibyte characters: status = %d, want 201", rec.Code)
	}
	if rec := e.send(sendBody(e.bob.String(), strings.Repeat("é", MaxBodyLen+1))); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("MaxBodyLen+1 multibyte characters: status = %d, want 422", rec.Code)
	}
}

func TestSendRejectsInvalidRequestsWithoutTouchingTheStore(t *testing.T) {
	e := newEnv()
	tests := []struct {
		name string
		body string
		want int
	}{
		{"malformed json", `{"to":`, http.StatusBadRequest},
		{"unknown field", `{"to":"` + e.bob.String() + `","body":"hi","extra":1}`, http.StatusBadRequest},
		{"recipient is not a uuid", sendBody("bob", "hi"), http.StatusUnprocessableEntity},
		{"recipient missing", `{"body":"hi"}`, http.StatusUnprocessableEntity},
		{"message to self", sendBody(e.alice.UserID.String(), "hi"), http.StatusUnprocessableEntity},
		{"empty body", sendBody(e.bob.String(), ""), http.StatusUnprocessableEntity},
		{"whitespace-only body", sendBody(e.bob.String(), " \t\n "), http.StatusUnprocessableEntity},
		{"body too long", sendBody(e.bob.String(), strings.Repeat("a", MaxBodyLen+1)), http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertStatus(t, e.send(tt.body), tt.want)
		})
	}
	if len(e.store.created) != 0 {
		t.Fatalf("store.Create called %d times for invalid requests", len(e.store.created))
	}
	assertNoPublish(t, e.pub)
}

func TestSendToUnknownRecipientReturns404(t *testing.T) {
	e := newEnv()
	e.store.createErr = ErrNoRecipient

	assertStatus(t, e.send(sendBody(e.bob.String(), "hi")), http.StatusNotFound)
	assertNoPublish(t, e.pub)
}

func TestSendStoreFailureReturns500WithoutDetails(t *testing.T) {
	e := newEnv()
	e.store.createErr = errors.New("connection refused to 10.0.0.5")

	assertInternalError(t, e.send(sendBody(e.bob.String(), "hi")), "10.0.0.5")
	assertNoPublish(t, e.pub)
}

func TestSendSucceedsEvenIfTheLivePushCannotBeDone(t *testing.T) {
	e := newEnv()
	e.h = NewHandler(e.store, fakeSessions{err: errors.New("database down")}, e.pub)

	assertStatus(t, e.send(sendBody(e.bob.String(), "hi")), http.StatusCreated)
	assertNoPublish(t, e.pub)
}

// ---- Conversation ----

func TestConversationRejectsInvalidRequestsWithoutTouchingTheStore(t *testing.T) {
	e := newEnv()
	someMsg := uuid.New().String()
	tests := []struct {
		name  string
		peer  string
		query string
	}{
		{"peer is not a uuid", "bob", ""},
		{"peer is the caller", e.alice.UserID.String(), ""},
		{"before is not a uuid", e.bob.String(), "?before=nope"},
		{"limit is not a number", e.bob.String(), "?limit=abc"},
		{"limit is negative", e.bob.String(), "?limit=-1"},
		{"limit overflows", e.bob.String(), "?limit=99999999999999999999"},
		{"valid before, bad limit", e.bob.String(), "?before=" + someMsg + "&limit=x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertStatus(t, e.conversation(tt.peer, tt.query), http.StatusUnprocessableEntity)
		})
	}
	if len(e.store.convCalls) != 0 {
		t.Fatalf("store.Conversation called %d times for invalid requests", len(e.store.convCalls))
	}
	assertNoPublish(t, e.pub)
}

func TestConversationPassesParametersToTheStore(t *testing.T) {
	e := newEnv()
	before := uuid.New()

	assertStatus(t, e.conversation(e.bob.String(), "?before="+before.String()+"&limit=25"), http.StatusOK)
	assertStatus(t, e.conversation(e.bob.String(), ""), http.StatusOK)

	if len(e.store.convCalls) != 2 {
		t.Fatalf("store calls = %d, want 2", len(e.store.convCalls))
	}
	first, second := e.store.convCalls[0], e.store.convCalls[1]
	if first.user != e.alice.UserID || first.peer != e.bob || first.before == nil || *first.before != before || first.limit != 25 {
		t.Fatalf("first call = %+v", first)
	}
	if second.before != nil || second.limit != 0 {
		t.Fatalf("second call = %+v, want no cursor and limit 0 (store applies the default)", second)
	}
}

func TestConversationReturnsMessagesAndNotifiesSendersOfNewlyDelivered(t *testing.T) {
	e := newEnv()
	m1 := Message{ID: uuid.New(), SenderID: e.bob, RecipientID: e.alice.UserID, Body: "one", Status: StatusDelivered}
	m2 := Message{ID: uuid.New(), SenderID: e.bob, RecipientID: e.alice.UserID, Body: "two", Status: StatusDelivered}
	e.store.convMsgs = []Message{m1, m2}
	e.store.convChanges = []StatusChange{
		{SenderID: e.bob, MessageID: m1.ID, RecipientID: e.alice.UserID, Status: StatusDelivered, At: time.Now()},
		{SenderID: e.bob, MessageID: m2.ID, RecipientID: e.alice.UserID, Status: StatusDelivered, At: time.Now()},
	}

	rec := e.conversation(e.bob.String(), "")

	assertStatus(t, rec, http.StatusOK)
	var got []Message
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 || got[0].ID != m1.ID || got[1].ID != m2.ID {
		t.Fatalf("response = %+v, want the store's messages in order", got)
	}

	if len(e.pub.calls) != 2 {
		t.Fatalf("publications = %d, want one status event per change", len(e.pub.calls))
	}
	for i, call := range e.pub.calls {
		if call.event.Name != EventStatus || !reflect.DeepEqual(call.sessionIDs, e.sessions[e.bob]) {
			t.Fatalf("publication %d = %+v, want a status event to bob's sessions", i, call)
		}
	}
	if decodeEvent(t, e.pub.calls[0].event)["message_id"] != m1.ID.String() ||
		decodeEvent(t, e.pub.calls[1].event)["message_id"] != m2.ID.String() {
		t.Fatal("status events must be published for each changed message")
	}
}

func TestConversationWithoutChangesPublishesNothing(t *testing.T) {
	e := newEnv()
	e.store.convMsgs = []Message{}

	rec := e.conversation(e.bob.String(), "")

	assertStatus(t, rec, http.StatusOK)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("body = %q, want []", rec.Body)
	}
	assertNoPublish(t, e.pub)
}

func TestConversationStoreFailureReturns500WithoutDetails(t *testing.T) {
	e := newEnv()
	e.store.convErr = errors.New("connection refused to 10.0.0.5")

	assertInternalError(t, e.conversation(e.bob.String(), ""), "10.0.0.5")
	assertNoPublish(t, e.pub)
}

// ---- MarkRead ----

func TestMarkReadNotifiesTheSender(t *testing.T) {
	e := newEnv()
	id := uuid.New()
	e.store.readChanged = true
	e.store.readChange = StatusChange{
		SenderID: e.bob, MessageID: id, RecipientID: e.alice.UserID,
		Status: StatusRead, At: time.Now(),
	}

	assertStatus(t, e.markRead(id.String()), http.StatusNoContent)

	want := []readCall{{id: id, recipient: e.alice.UserID}}
	if !reflect.DeepEqual(e.store.readCalls, want) {
		t.Fatalf("MarkRead calls = %+v, want %+v (the caller must be the recipient)", e.store.readCalls, want)
	}
	if len(e.pub.calls) != 1 {
		t.Fatalf("publications = %d, want 1", len(e.pub.calls))
	}
	call := e.pub.calls[0]
	if call.event.Name != EventStatus || !reflect.DeepEqual(call.sessionIDs, e.sessions[e.bob]) {
		t.Fatalf("publication = %+v, want a status event to the sender", call)
	}
	if got := decodeEvent(t, call.event); got["status"] != "read" || got["message_id"] != id.String() {
		t.Fatalf("payload = %v", got)
	}
}

func TestMarkReadOfAnAlreadyReadMessageIsANoOp(t *testing.T) {
	e := newEnv()
	e.store.readChanged = false

	assertStatus(t, e.markRead(uuid.New().String()), http.StatusNoContent)
	assertNoPublish(t, e.pub)
}

func TestMarkReadWithInvalidIDReturns404WithoutTouchingTheStore(t *testing.T) {
	e := newEnv()

	assertStatus(t, e.markRead("not-a-uuid"), http.StatusNotFound)
	if len(e.store.readCalls) != 0 {
		t.Fatal("store must not be called for an invalid id")
	}
}

func TestMarkReadNotFoundReturns404(t *testing.T) {
	e := newEnv()
	e.store.readErr = ErrNotFound

	assertStatus(t, e.markRead(uuid.New().String()), http.StatusNotFound)
	assertNoPublish(t, e.pub)
}

func TestMarkReadStoreFailureReturns500WithoutDetails(t *testing.T) {
	e := newEnv()
	e.store.readErr = errors.New("connection refused to 10.0.0.5")

	assertInternalError(t, e.markRead(uuid.New().String()), "10.0.0.5")
	assertNoPublish(t, e.pub)
}

// ---- intParam ----

func TestIntParam(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    int64
		wantErr bool
	}{
		{"absent", "", 0, false},
		{"empty value", "limit=", 0, false},
		{"zero", "limit=0", 0, false},
		{"positive", "limit=42", 42, false},
		{"negative", "limit=-1", 0, true},
		{"not a number", "limit=abc", 0, true},
		{"decimal", "limit=1.5", 0, true},
		{"overflow", "limit=99999999999999999999", 0, true},
		{"other parameter only", "offset=5", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.URL.RawQuery = tt.query
			got, err := intParam(r, "limit")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}
