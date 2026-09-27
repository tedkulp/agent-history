package web

import (
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/live"
)

func TestFeedPageListensOnlyOnItsFirstPage(t *testing.T) {
	ls := newLiveSite(t, line("u1", "", "user", `"first prompt"`))

	_, page := get(t, ls.srv.URL+"/?machine=m1&source=claude-code")
	for _, want := range []string{
		`data-events="/events?machine=m1&amp;source=claude-code"`,
		`data-reload="/?machine=m1&amp;source=claude-code"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("first page lacks %s", want)
		}
	}
	// Input with no search terms shows the feed; the pill reloads it without.
	if _, page := get(t, ls.srv.URL+"/?q=%22%22"); !strings.Contains(page, `data-events="/events"`) || !strings.Contains(page, `data-reload="/"`) {
		t.Error("a feed from an empty search doesn't listen at /events or reload to /")
	}
	for name, u := range map[string]string{
		"a later page": "/?before=9999999999999&before_id=9",
		"search":       "/?q=prompt",
	} {
		if _, page := get(t, ls.srv.URL+u); strings.Contains(page, "data-events") {
			t.Errorf("%s listens for live Sessions", name)
		}
	}
	if _, frag := getHX(t, ls.srv.URL+"/?before=9999999999999&before_id=9"); strings.Contains(frag, "data-events") {
		t.Error("a Load more page listens for live Sessions")
	}
}

func TestFeedEventsCarryLiveSessionsDebouncedWithoutReadingTheStore(t *testing.T) {
	oldBeat, oldDebounce := heartbeatEvery, feedDebounce
	heartbeatEvery, feedDebounce = 50*time.Millisecond, 300*time.Millisecond
	defer func() { heartbeatEvery, feedDebounce = oldBeat, oldDebounce }()

	ls := newLiveSite(t, line("u1", "", "user", `"first prompt"`))
	resp, r := stream(t, ls.srv.URL+"/events?machine=m1")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for k, want := range map[string]string{"Content-Type": "text/event-stream", "X-Accel-Buffering": "no", "Cache-Control": "no-cache"} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	nextEvent(t, r, ": heartbeat")

	// A live parse reaches the open feed, named by its Session.
	ls.append(line("a1", "u1", "assistant", `[{"type":"text","text":"hi"}]`))
	ls.parse()
	nextEvent(t, r, "event: changed")
	if got := nextEvent(t, r, "data:"); got != "data: 1" {
		t.Fatalf("event data = %q", got)
	}

	// The idle stream reads nothing from the store. Sessions published within
	// the debounce arrive in one event, each once, and ones the chips hide are
	// left out.
	ls.store.Close()
	nextEvent(t, r, ": heartbeat")
	start := time.Now()
	ls.live.PublishFeed(live.FeedSession{ID: 9, Machine: "m1"})
	ls.live.PublishFeed(live.FeedSession{ID: 3, Machine: "m1"})
	ls.live.PublishFeed(live.FeedSession{ID: 4, Machine: "m2"})
	ls.live.PublishFeed(live.FeedSession{ID: 9, Machine: "m1"})
	if got := nextEvent(t, r, "data:"); got != "data: 3,9" {
		t.Fatalf("debounced event data = %q", got)
	}
	if d := time.Since(start); d < feedDebounce/2 {
		t.Errorf("the second event came %v after the first; want about %v", d, feedDebounce)
	}
}

func TestFeedParamsShowMatchesTheChips(t *testing.T) {
	s := live.FeedSession{ID: 1, Machine: "m1", Source: "codex", Project: "/src/app"}
	noProj := live.FeedSession{ID: 2, Machine: "m1", Source: "codex"}
	warn := live.FeedSession{ID: 3, Machine: "m1", Source: "codex", Warnings: true}
	cases := []struct {
		p    feedParams
		s    live.FeedSession
		want bool
	}{
		{feedParams{}, s, true},
		{feedParams{Machine: "m1"}, s, true},
		{feedParams{Machine: "m2"}, s, false},
		{feedParams{Source: "codex"}, s, true},
		{feedParams{Source: "opencode"}, s, false},
		{feedParams{Machine: "m1", Project: "/src/app"}, s, true},
		{feedParams{Machine: "m1", Project: "/src/other"}, s, false},
		{feedParams{Machine: "m1", Project: noProject}, s, false},
		{feedParams{Machine: "m1", Project: noProject}, noProj, true},
		{feedParams{Warnings: true}, s, false},
		{feedParams{Warnings: true}, warn, true},
	}
	for _, c := range cases {
		if got := c.p.shows(c.s); got != c.want {
			t.Errorf("%+v shows %+v = %v, want %v", c.p, c.s, got, c.want)
		}
	}
}
