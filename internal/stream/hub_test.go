package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newSub builds a registered-looking subscription without going through HTTP,
// so the registry can be tested on its own.
func newSub(sessionID int64, buffer int) (*Subscription, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Subscription{
		sessionID: sessionID,
		out:       make(chan Event, buffer),
		cancel:    cancel,
	}, ctx
}

func cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func TestPublishReachesEveryStreamOfTheGivenSessionsOnly(t *testing.T) {
	h := NewHub()

	laptopTab1, _ := newSub(1, 1)
	laptopTab2, _ := newSub(1, 1) // second tab, same session
	phone, _ := newSub(2, 1)
	stranger, _ := newSub(3, 1)
	for _, c := range []*Subscription{laptopTab1, laptopTab2, phone, stranger} {
		if !h.add(c) {
			t.Fatal("add returned false")
		}
	}

	h.Publish([]int64{1, 2}, Event{Name: "message", Data: []byte(`{"id":1}`)})

	for i, c := range []*Subscription{laptopTab1, laptopTab2, phone} {
		select {
		case e := <-c.out:
			if e.Name != "message" {
				t.Fatalf("stream %d: event = %q", i, e.Name)
			}
		default:
			t.Fatalf("stream %d received nothing", i)
		}
	}
	if len(stranger.out) != 0 {
		t.Fatal("event leaked to a session that was not addressed")
	}
}

func TestPublishToSessionsWithNoStreamIsHarmless(t *testing.T) {
	h := NewHub()
	h.Publish([]int64{404}, Event{Name: "message"})
	h.Publish(nil, Event{Name: "message"})
}

func TestCloseSessionClosesOnlyThatSession(t *testing.T) {
	h := NewHub()

	laptopTab1, laptop1Ctx := newSub(1, 1)
	laptopTab2, laptop2Ctx := newSub(1, 1) // second tab, same session
	phone, phoneCtx := newSub(2, 1)
	for _, c := range []*Subscription{laptopTab1, laptopTab2, phone} {
		h.add(c)
	}

	h.CloseSession(1)

	if !cancelled(laptop1Ctx) || !cancelled(laptop2Ctx) {
		t.Fatal("both tabs of the revoked session must be closed")
	}
	if cancelled(phoneCtx) {
		t.Fatal("a different session of the same user must stay open")
	}
}

func TestPublishDropsAStreamThatCannotKeepUp(t *testing.T) {
	h := NewHub()
	slow, slowCtx := newSub(1, 1)
	h.add(slow)

	h.Publish([]int64{1}, Event{Name: "message"}) // fills the buffer
	if cancelled(slowCtx) {
		t.Fatal("a stream with room must not be dropped")
	}

	h.Publish([]int64{1}, Event{Name: "message"}) // overflows it
	if !cancelled(slowCtx) {
		t.Fatal("a stream that cannot keep up must be dropped")
	}
}

func TestRemoveLeavesNoEmptySetsBehind(t *testing.T) {
	h := NewHub()
	c, _ := newSub(1, 1)

	h.add(c)
	h.remove(c)

	if len(h.bySession) != 0 {
		t.Fatalf("index not empty: %v", h.bySession)
	}
}

func TestRemovingOneTabKeepsTheSessionsOtherTabs(t *testing.T) {
	h := NewHub()
	first, _ := newSub(1, 1)
	second, _ := newSub(1, 1)
	h.add(first)
	h.add(second)

	h.remove(first)

	h.Publish([]int64{1}, Event{Name: "message"})
	if len(second.out) != 1 {
		t.Fatal("the remaining tab must still receive events")
	}
	if len(first.out) != 0 {
		t.Fatal("a removed tab must not receive events")
	}
}

func TestCloseEndsEveryStreamAndRefusesNewOnes(t *testing.T) {
	h := NewHub()
	first, ok := h.Subscribe(context.Background(), 1)
	if !ok {
		t.Fatal("Subscribe refused on an open hub")
	}
	second, _ := h.Subscribe(context.Background(), 2)

	h.Close()

	if !cancelled(first.Context()) || !cancelled(second.Context()) {
		t.Fatal("Close must end every stream")
	}
	if late, ok := h.Subscribe(context.Background(), 3); ok || late != nil {
		t.Fatal("a stream arriving during shutdown must be refused")
	}
}

func TestClosingASubscriptionUnregistersItAndEndsItsContext(t *testing.T) {
	h := NewHub()
	s, _ := h.Subscribe(context.Background(), 1)

	s.Close()

	if !cancelled(s.Context()) {
		t.Fatal("Close must cancel the subscription's context")
	}
	if len(h.bySession) != 0 {
		t.Fatalf("index not empty: %v", h.bySession)
	}
}

func TestSubscriptionEndsWhenItsParentContextEnds(t *testing.T) {
	h := NewHub()
	parent, cancel := context.WithCancel(context.Background())
	s, _ := h.Subscribe(parent, 1)
	defer s.Close()

	cancel()

	if !cancelled(s.Context()) {
		t.Fatal("a client disconnect must end the subscription")
	}
}

// Revoking a session nobody is streaming on, or one that was already cleaned
// up, is normal: the cleanup job reports every deleted session.
func TestRevokingAnUnknownSessionIsHarmless(t *testing.T) {
	h := NewHub()
	h.CloseSession(404)
}

func TestEventsAreDeliveredInOrderWithTheirPayload(t *testing.T) {
	h := NewHub()
	s, _ := h.Subscribe(context.Background(), 1)
	defer s.Close()
	flushed := make(chan struct{})

	h.Publish([]int64{1}, Event{Name: "message", Data: []byte(`{"id":1}`), OnFlush: func() { close(flushed) }})
	h.Publish([]int64{1}, Event{Name: "status", Data: []byte(`{"id":2}`)})

	first, second := <-s.Events(), <-s.Events()
	if first.Name != "message" || string(first.Data) != `{"id":1}` {
		t.Fatalf("first event = %+v", first)
	}
	if second.Name != "status" || string(second.Data) != `{"id":2}` {
		t.Fatalf("second event = %+v", second)
	}
	if first.OnFlush == nil || second.OnFlush != nil {
		t.Fatal("the OnFlush hook must be carried through unchanged")
	}
	first.OnFlush()
	<-flushed
}

func TestAStreamAbsorbsABurstUpToItsBuffer(t *testing.T) {
	h := NewHub()
	s, _ := h.Subscribe(context.Background(), 1)
	defer s.Close()

	for range outBuffer {
		h.Publish([]int64{1}, Event{Name: "message"})
	}

	if cancelled(s.Context()) {
		t.Fatalf("a burst of %d events must fit in the buffer", outBuffer)
	}
	if len(s.Events()) != outBuffer {
		t.Fatalf("buffered = %d, want %d", len(s.Events()), outBuffer)
	}
}

func TestClosingTwiceIsHarmless(t *testing.T) {
	h := NewHub()
	s, _ := h.Subscribe(context.Background(), 1)

	s.Close()
	s.Close()
	h.Close()
	h.Close()

	if len(h.bySession) != 0 {
		t.Fatalf("index not empty: %v", h.bySession)
	}
}

// The hub is reached from request handlers, the cleanup job and the publisher
// at the same time, so every exported method has to be safe to call
// concurrently. Run with -race for this to mean anything.
func TestConcurrentUseIsSafe(t *testing.T) {
	h := NewHub()
	const workers = 8

	done := make(chan struct{})
	var started sync.WaitGroup
	started.Add(3 * workers)

	for i := range workers {
		sessionID := int64(i%3 + 1)
		go func() {
			started.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if s, ok := h.Subscribe(context.Background(), sessionID); ok {
					select { // drain one event if there is one, then leave
					case <-s.Events():
					case <-s.Context().Done():
					case <-done:
					}
					s.Close()
				}
			}
		}()
		go func() {
			started.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				h.Publish([]int64{1, 2, 3}, Event{Name: "message"})
			}
		}()
		go func() {
			started.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				h.CloseSession(sessionID)
			}
		}()
	}

	started.Wait()
	time.Sleep(50 * time.Millisecond)
	h.Close()
	close(done)
}
