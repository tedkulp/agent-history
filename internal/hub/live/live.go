// Package live tells open Transcript pages that their Session was parsed again
// from live data, and open home feeds which of their Sessions were (hub.md
// §4.5, §4.7). It is an in-process pub/sub.
package live

import "sync"

// Broadcaster fans a Session's "changed" events out to its subscribers.
// Publishing never blocks: each subscriber's channel holds one pending event,
// so events coalesce and a slow subscriber misses nothing but duplicates.
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[int64]map[chan struct{}]struct{}
	feeds  map[*FeedSub]struct{}
	closed bool
}

// New returns an empty Broadcaster.
func New() *Broadcaster {
	return &Broadcaster{subs: map[int64]map[chan struct{}]struct{}{}, feeds: map[*FeedSub]struct{}{}}
}

// FeedSession is a Session the home feed lists, just parsed from live data,
// with what the feed's chips match on (hub.md §4.7).
type FeedSession struct {
	ID       int64
	Machine  string
	Source   string
	Project  string // "" = No project
	Warnings bool   // the "has warnings" chip matches it
}

// FeedSub is one open feed's subscription. Published Sessions wait in it,
// once each, until taken, so a slow reader loses none and publishing never
// blocks.
type FeedSub struct {
	ready   chan struct{}
	mu      sync.Mutex
	pending map[int64]FeedSession
}

// Ready receives when Sessions are waiting to be taken. It is closed by the
// Broadcaster's Close.
func (f *FeedSub) Ready() <-chan struct{} { return f.ready }

// Take returns the waiting Sessions and forgets them.
func (f *FeedSub) Take() []FeedSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FeedSession, 0, len(f.pending))
	for _, s := range f.pending {
		out = append(out, s)
	}
	clear(f.pending)
	return out
}

// SubscribeFeed returns a subscription to every published FeedSession, and a
// function that unsubscribes. After Close, its Ready channel is closed.
func (b *Broadcaster) SubscribeFeed() (*FeedSub, func()) {
	f := &FeedSub{ready: make(chan struct{}, 1), pending: map[int64]FeedSession{}}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(f.ready)
		return f, func() {}
	}
	b.feeds[f] = struct{}{}
	return f, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.feeds, f)
	}
}

// PublishFeed hands s to every feed subscriber.
func (b *Broadcaster) PublishFeed(s FeedSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for f := range b.feeds {
		f.mu.Lock()
		f.pending[s.ID] = s
		f.mu.Unlock()
		select {
		case f.ready <- struct{}{}:
		default:
		}
	}
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
	for f := range b.feeds {
		close(f.ready)
	}
	b.subs, b.feeds = nil, nil
}
