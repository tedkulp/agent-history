package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
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

// chartBars counts the bars of the page's i-th chart.
func chartBars(page string, i int) int {
	charts := strings.Split(page, `<figure class="chart">`)
	if i+1 >= len(charts) {
		return -1
	}
	return strings.Count(strings.Split(charts[i+1], "</figure>")[0], `<g class="bar">`)
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
	if n := chartBars(page, 0); n != 30 {
		t.Errorf("30d chart has %d bars, want 30", n)
	}
	// The group chart has the same days, one series for the one Source.
	if n := chartBars(page, 1); n != 30 {
		t.Errorf("30d group chart has %d bars, want 30", n)
	}
	if !strings.Contains(page, "Tokens per day by source") || !strings.Contains(page, `<i class="g1"></i>hermes</span>`) {
		t.Errorf("group chart missing its title or legend")
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
	if n := chartBars(page, 0); n != 7 {
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
	started := time.Now().Add(-time.Second)
	srv := usageSite(t, record{"m1", "20261010_120000_aaaaaa", hmExport(t, "20261010_120000_aaaaaa", started, 5, 5)})
	_, page := get(t, srv.URL+"/usage?range=all&by=project")
	if strings.Contains(page, "reported no Usage") {
		t.Errorf("no-Usage line shown with none")
	}
	if !strings.Contains(page, `<td title="/Users/ted/src/app">app <span class="dim">laptop</span></td>`) {
		t.Errorf("project row missing")
	}
	// One bar, unless the Session started yesterday, just before midnight.
	want := 1
	if started.Day() != time.Now().Day() {
		want = 2
	}
	if n := chartBars(page, 0); n != want {
		t.Errorf("all-time chart has %d bars, want %d", n, want)
	}
}

// The group chart stacks the top groups on their own and the rest as
// "Other"; groups with no tokens are left out.
func TestGroupChartMergesTheRestIntoOther(t *testing.T) {
	loc := time.FixedZone("X", 0)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, loc)
	rep := store.UsageReport{GroupDays: []store.UsageGroupDay{{Day: day, Tokens: map[string]int64{}}}}
	var rows []usageRow
	for i := range 8 {
		k := fmt.Sprint("k", i)
		tokens := int64(70 - 10*i) // k7 has none
		rep.Groups = append(rep.Groups, store.UsageGroup{Key: k, Label: k, UsageCounts: store.UsageCounts{Usage: parser.Usage{Input: tokens}}})
		rep.GroupDays[0].Tokens[k] = tokens
		rows = append(rows, usageRow{Label: k})
	}
	c := groupChart(rep, rows, "Model", day, day)
	var labels []string
	for _, l := range c.Legend {
		labels = append(labels, l.Label+"/"+l.Class)
	}
	if got, want := strings.Join(labels, " "), "k0/g1 k1/g2 k2/g3 k3/g4 k4/g5 Other/g0"; got != want {
		t.Errorf("legend = %s, want %s", got, want)
	}
	if len(c.Bars) != 1 || len(c.Bars[0].Segs) != 6 {
		t.Fatalf("bars = %+v", c.Bars)
	}
	// k5 and k6 make Other: 20 + 10 of the tallest bar's 280.
	if !strings.Contains(c.Bars[0].Title, "Other 30") || c.Title != "Tokens per day by model" {
		t.Errorf("title = %q, bar = %q", c.Title, c.Bars[0].Title)
	}
	if other := c.Bars[0].Segs[5]; other.Class != "g0" || other.H != coord(30.0/280*usageChartH) {
		t.Errorf("Other segment = %+v", other)
	}
}
