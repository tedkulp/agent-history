package web

import (
	"regexp"
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
		`data-rows="/feed/rows?machine=m1&amp;source=claude-code"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("first page lacks %s", want)
		}
	}
	if strings.Contains(page, "pill-updated") || strings.Contains(page, "Sessions updated") {
		t.Error("the feed still has the Sessions updated pill")
	}
	// Input with no search terms shows the feed; it listens and fetches without.
	if _, page := get(t, ls.srv.URL+"/?q=%22%22"); !strings.Contains(page, `data-events="/events" data-rows="/feed/rows"`) {
		t.Error("a feed from an empty search doesn't listen at /events and fetch from /feed/rows")
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
	ls.live.PublishFeed(live.FeedSession{ID: 5, MachineID: "m1"})
	if got := nextEvent(t, r, "data:"); got != "data: 5" {
		t.Fatalf("event data = %q", got)
	}
	start := time.Now()
	ls.live.PublishFeed(live.FeedSession{ID: 9, MachineID: "m1"})
	ls.live.PublishFeed(live.FeedSession{ID: 3, MachineID: "m1"})
	ls.live.PublishFeed(live.FeedSession{ID: 4, MachineID: "m2"})
	ls.live.PublishFeed(live.FeedSession{ID: 9, MachineID: "m1"})
	if got := nextEvent(t, r, "data:"); got != "data: 3,9" {
		t.Fatalf("debounced event data = %q", got)
	}
	// The previous event went out just before start, so the debounce holds
	// this one back for nearly all of feedDebounce.
	if d := time.Since(start); d < feedDebounce*3/4 {
		t.Errorf("the next event came %v after the last; want at least about %v", d, feedDebounce)
	}
}

func TestFeedParamsShowMatchesTheChips(t *testing.T) {
	s := live.FeedSession{ID: 1, MachineID: "m1", Source: "codex", ProjectCwd: "/src/app"}
	noProj := live.FeedSession{ID: 2, MachineID: "m1", Source: "codex"}
	warn := live.FeedSession{ID: 3, MachineID: "m1", Source: "codex", Warnings: true}
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

func TestFeedRowsServesTheNamedSessionsByTheChips(t *testing.T) {
	ls := newLiveSite(t, line("u1", "", "user", `"first prompt"`))
	at := regexp.MustCompile(`data-at="(\d+)"`)

	code, frag := getHX(t, ls.srv.URL+"/feed/rows?ids=1,7&machine=m1")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`href="/sessions/1"`, `data-id="1"`, `data-day="`, "first prompt"} {
		if !strings.Contains(frag, want) {
			t.Errorf("rows lack %s:\n%s", want, frag)
		}
	}
	for _, not := range []string{"<html", `class="more"`, "data-events"} {
		if strings.Contains(frag, not) {
			t.Errorf("rows hold %s", not)
		}
	}
	first := at.FindStringSubmatch(frag)
	if first == nil {
		t.Fatal("row lacks data-at")
	}

	// Sessions the chips hide never come back.
	for _, u := range []string{"/feed/rows?ids=1&machine=m2", "/feed/rows?ids=1&warnings=1", "/feed/rows?ids="} {
		if _, frag := getHX(t, ls.srv.URL+u); strings.Contains(frag, "data-id") {
			t.Errorf("%s lists a row", u)
		}
	}

	// A live parse moves the row's time; without ids it's the first page.
	ls.append(line("a1", "u1", "assistant", `[{"type":"text","text":"hi"}]`))
	ls.parse()
	_, frag = getHX(t, ls.srv.URL+"/feed/rows")
	if m := at.FindStringSubmatch(frag); m == nil || m[1] == first[1] {
		t.Errorf("first page rows after a live parse: %s", frag)
	}

	if code, _ := getHX(t, ls.srv.URL+"/feed/rows?ids=1,x"); code != 400 {
		t.Errorf("bad ids: status %d, want 400", code)
	}
}
