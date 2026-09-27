// Package live tells open Transcript pages that their Session was parsed again
// from live data (hub.md §4.5, §4.7). It is an in-process pub/sub keyed by
// Session id.
package live

import "sync"

// Broadcaster fans a Session's "changed" events out to its subscribers.
// Publishing never blocks: each subscriber's channel holds one pending event,
// so events coalesce and a slow subscriber misses nothing but duplicates.
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[int64]map[chan struct{}]struct{}
	closed bool
}

// New returns an empty Broadcaster.
func New() *Broadcaster {
	return &Broadcaster{subs: map[int64]map[chan struct{}]struct{}{}}
}

// Subscribe returns a channel that receives an event each time the Session is
// published, and a function that unsubscribes. The channel is closed by Close;
// after Close, Subscribe returns a closed channel.
func (b *Broadcaster) Subscribe(sessionID int64) (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(c)
		return c, func() {}
	}
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[chan struct{}]struct{}{}
	}
	b.subs[sessionID][c] = struct{}{}
	return c, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if set := b.subs[sessionID]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(b.subs, sessionID)
			}
		}
	}
}

// Publish sends the Session's subscribers an event, dropping it for any that
// already has one pending.
func (b *Broadcaster) Publish(sessionID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs[sessionID] {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Close closes every subscriber's channel, ending their streams, for
// shutdown.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, set := range b.subs {
		for c := range set {
			close(c)
		}
	}
	b.subs = nil
}
