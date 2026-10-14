package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/protocol"
)

// hmExport is a Hermes export of one Session started at, with a user and
// an assistant row and the given token totals.
func hmExport(t *testing.T, id string, at time.Time, input, output int) string {
	t.Helper()
	secs := float64(at.UnixMilli()) / 1000
	var b strings.Builder
	for _, l := range []map[string]any{
		{"table": "sessions", "row": map[string]any{"id": id, "source": "cli", "model": "gpt-5.5", "started_at": secs, "cwd": "/Users/ted/src/app",
			"input_tokens": input, "output_tokens": output, "cache_read_tokens": 0, "cache_write_tokens": 0, "reasoning_tokens": 0}},
		{"table": "messages", "row": map[string]any{"id": 1, "role": "user", "content": "hi " + id, "timestamp": secs, "active": 1}},
		{"table": "messages", "row": map[string]any{"id": 2, "role": "assistant", "content": "hello", "timestamp": secs, "active": 1}},
	} {
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func usageSite(t *testing.T, recs ...record) *httptest.Server {
	t.Helper()
	s, _ := newHubSource(t, protocol.SourceHermes, recs)
	srv := httptest.NewServer(New(s, nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

func TestUsagePage(t *testing.T) {
	now := time.Now()
	srv := usageSite(t,
		record{"m1", "20261010_120000_aaaaaa", hmExport(t, "20261010_120000_aaaaaa", now.Add(-time.Hour), 1234567, 890)},
		record{"m1", "20261010_120000_bbbbbb", hmExport(t, "20261010_120000_bbbbbb", now.Add(-time.Hour), 0, 0)},
	)

	code, page := get(t, srv.URL+"/usage")
	if code != http.StatusOK {
		t.Fatalf("GET /usage = %d", code)
	}
	for _, want := range []string{
		`<a class="nav" href="/usage">Usage</a>`,
		`<a class="on" href="/usage?by=source&amp;range=30d">30 days</a>`,
		`href="/usage?by=model&amp;range=30d"`,
		`<a class="refresh" href="/usage?by=source&amp;range=30d">`,
		`title="1,234,567">1.2M</div>`, // the input tile
		"1 Session in range reported no Usage",
		`<td>hermes</td>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/usage lacks %q", want)
		}
	}
	if n := strings.Count(page, `<g class="bar">`); n != 30 {
		t.Errorf("30d chart has %d bars, want 30", n)
	}
	// Session count: both are top-level and active; Message count: both rows.
	if !strings.Contains(page, `<div class="lab">Sessions</div><div class="v" title="2">2</div>`) {
		t.Errorf("Sessions tile wrong")
	}

	_, page = get(t, srv.URL+"/usage?range=7d&by=model")
	for _, want := range []string{
		`<a class="on" href="/usage?by=model&amp;range=7d">7 days</a>`,
		`<a class="on" href="/usage?by=model&amp;range=7d">Model</a>`,
		`href="/usage?by=model&amp;range=90d"`,
		`<td>gpt-5.5</td>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/usage?range=7d&by=model lacks %q", want)
		}
	}
	if n := strings.Count(page, `<g class="bar">`); n != 7 {
		t.Errorf("7d chart has %d bars, want 7", n)
	}

	// Unknown values fall back to the defaults.
	_, page = get(t, srv.URL+"/usage?range=1y&by=nope")
	if !strings.Contains(page, `<a class="on" href="/usage?by=source&amp;range=30d">30 days</a>`) {
		t.Errorf("bad range/by didn't fall back")
	}

	// Every page links to /usage.
	_, feed := get(t, srv.URL+"/")
	if !strings.Contains(feed, `<a class="nav" href="/usage">Usage</a>`) {
		t.Errorf("feed lacks the Usage link")
	}
}

func TestUsagePageWithoutNoUsageLine(t *testing.T) {
	srv := usageSite(t, record{"m1", "20261010_120000_aaaaaa", hmExport(t, "20261010_120000_aaaaaa", time.Now().Add(-time.Second), 5, 5)})
	_, page := get(t, srv.URL+"/usage?range=all&by=project")
	if strings.Contains(page, "reported no Usage") {
		t.Errorf("no-Usage line shown with none")
	}
	if !strings.Contains(page, `<td title="/Users/ted/src/app">app <span class="dim">laptop</span></td>`) {
		t.Errorf("project row missing")
	}
	if n := strings.Count(page, `<g class="bar">`); n != 1 {
		t.Errorf("all-time chart has %d bars, want 1 (today)", n)
	}
}
