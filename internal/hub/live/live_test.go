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
