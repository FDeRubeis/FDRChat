package stream

import (
	"context"
	"sync"
)

// outBuffer absorbs bursts. Overflowing it drops the stream.
const outBuffer = 64

// Event is one server-sent event.
type Event struct {
	Name string
	Data []byte

	// OnFlush runs after the event is written to a stream, once per stream.
	OnFlush func()
}

// Subscription is one open stream registered with a Hub.
type Subscription struct {
	hub       *Hub
	sessionID int64
	ctx       context.Context
	out       chan Event
	cancel    context.CancelFunc
}

// Context is done when the subscription is dropped or its parent context ends.
func (s *Subscription) Context() context.Context { return s.ctx }

// Events yields what was published to the subscription's session.
func (s *Subscription) Events() <-chan Event { return s.out }

// Close unregisters the subscription.
func (s *Subscription) Close() {
	s.hub.remove(s)
	s.cancel()
}

// Hub routes events to the open streams, indexed by session.
type Hub struct {
	mu        sync.Mutex // guards bySession and closed
	bySession map[int64]map[*Subscription]struct{}
	closed    bool
}

func NewHub() *Hub {
	return &Hub{bySession: make(map[int64]map[*Subscription]struct{})}
}

// Subscribe registers a stream for a sessionID.
func (h *Hub) Subscribe(ctx context.Context, sessionID int64) (*Subscription, bool) {
	ctx, cancel := context.WithCancel(ctx)
	s := &Subscription{
		hub:       h,
		sessionID: sessionID,
		ctx:       ctx,
		out:       make(chan Event, outBuffer),
		cancel:    cancel,
	}
	if !h.add(s) {
		cancel()
		return nil, false
	}
	return s, true
}

// Publish queues an event for every stream of the given sessions.
// A stream whose buffer is full is dropped.
func (h *Hub) Publish(sessionIDs []int64, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range sessionIDs {
		for s := range h.bySession[id] {
			select {
			case s.out <- e:
			default:
				s.cancel()
			}
		}
	}
}

// CloseSession ends every stream of the session.
func (h *Hub) CloseSession(sessionID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.bySession[sessionID] {
		s.cancel()
	}
}

// Close ends every stream and rejects new subscriptions.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, set := range h.bySession {
		for s := range set {
			s.cancel()
		}
	}
}

func (h *Hub) add(s *Subscription) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	set, ok := h.bySession[s.sessionID]
	if !ok {
		set = make(map[*Subscription]struct{})
		h.bySession[s.sessionID] = set
	}
	set[s] = struct{}{}
	return true
}

func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.bySession[s.sessionID]
	delete(set, s)
	if len(set) == 0 {
		delete(h.bySession, s.sessionID) // do not keep empty sets for ended sessions
	}
}
