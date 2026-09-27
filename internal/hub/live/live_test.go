package live

import (
	"testing"
	"time"
)

func received(c <-chan struct{}) bool {
	select {
	case _, ok := <-c:
		return ok
	case <-time.After(time.Second):
		return false
	}
}

func empty(c <-chan struct{}) bool {
	select {
	case <-c:
		return false
	default:
		return true
	}
}

func TestPublishReachesThatSessionsSubscribers(t *testing.T) {
	b := New()
	a1, stop1 := b.Subscribe(1)
	defer stop1()
	a2, stop2 := b.Subscribe(1)
	defer stop2()
	other, stop3 := b.Subscribe(2)
	defer stop3()

	b.Publish(1)
	if !received(a1) || !received(a2) {
		t.Fatal("a subscriber of Session 1 got no event")
	}
	if !empty(other) {
		t.Error("a subscriber of Session 2 got Session 1's event")
	}
}

func TestPublishNeverBlocksAndCoalesces(t *testing.T) {
	b := New()
	c, stop := b.Subscribe(1) // never read until the end
	defer stop()
	done := make(chan struct{})
	go func() {
		for range 1000 {
			b.Publish(1)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}
	if !received(c) {
		t.Fatal("no event after 1000 publishes")
	}
	if !empty(c) {
		t.Error("events weren't coalesced into one")
	}
}

func TestUnsubscribeAndClose(t *testing.T) {
	b := New()
	c, stop := b.Subscribe(1)
	stop()
	stop() // idempotent
	b.Publish(1)
	if !empty(c) {
		t.Error("an unsubscribed channel got an event")
	}

	c2, stop2 := b.Subscribe(1)
	defer stop2()
	b.Close()
	if _, ok := <-c2; ok {
		t.Error("Close didn't close a subscriber's channel")
	}
	c3, stop3 := b.Subscribe(1)
	defer stop3()
	if _, ok := <-c3; ok {
		t.Error("Subscribe after Close returned an open channel")
	}
	b.Publish(1) // no panic on a closed broadcaster
}

func TestFeedSubscribersGetEachSessionOnceWithoutBlocking(t *testing.T) {
	b := New()
	f, stop := b.SubscribeFeed() // not read while the publishes run
	defer stop()
	f2, stop2 := b.SubscribeFeed()
	defer stop2()
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			b.PublishFeed(FeedSession{ID: int64(i%3 + 1), Source: "codex"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PublishFeed blocked on a subscriber that never reads")
	}
	if !received(f.Ready()) {
		t.Fatal("no feed event")
	}
	if !empty(f.Ready()) {
		t.Error("ready signals weren't coalesced into one")
	}
	got := map[int64]bool{}
	for _, s := range f.Take() {
		if got[s.ID] || s.Source != "codex" {
			t.Errorf("Take repeated or garbled %+v", s)
		}
		got[s.ID] = true
	}
	if len(got) != 3 {
		t.Errorf("Take = %v, want Sessions 1, 2 and 3", got)
	}
	if s := f.Take(); len(s) != 0 {
		t.Errorf("a second Take = %v", s)
	}
	if !received(f2.Ready()) || len(f2.Take()) != 3 {
		t.Error("the second subscriber missed Sessions")
	}

	// Transcript subscribers don't hear feed events, and vice versa.
	c, stopC := b.Subscribe(1)
	defer stopC()
	b.PublishFeed(FeedSession{ID: 1})
	if !empty(c) {
		t.Error("a Transcript subscriber got a feed event")
	}
	if !received(f.Ready()) || len(f.Take()) != 1 {
		t.Fatal("the feed event didn't arrive")
	}
	b.Publish(1)
	if !empty(f.Ready()) {
		t.Error("a feed subscriber got a Transcript event")
	}
}

func TestFeedUnsubscribeAndClose(t *testing.T) {
	b := New()
	f, stop := b.SubscribeFeed()
	stop()
	stop()
	b.PublishFeed(FeedSession{ID: 1})
	if !empty(f.Ready()) {
		t.Error("an unsubscribed feed got an event")
	}

	f2, stop2 := b.SubscribeFeed()
	defer stop2()
	b.Close()
	if _, ok := <-f2.Ready(); ok {
		t.Error("Close didn't close a feed subscriber")
	}
	f3, stop3 := b.SubscribeFeed()
	defer stop3()
	if _, ok := <-f3.Ready(); ok {
		t.Error("SubscribeFeed after Close returned an open subscription")
	}
	b.PublishFeed(FeedSession{ID: 1})
}
