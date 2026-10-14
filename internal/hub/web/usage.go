package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

// usageRange is one of the Usage page's fixed windows (hub.md §4.7).
type usageRange struct {
	Param string
	Label string
	Days  int // 0 = all time
}

var usageRanges = []usageRange{{"7d", "7 days", 7}, {"30d", "30 days", 30}, {"90d", "90 days", 90}, {"all", "All time", 0}}

// usageBys are the Usage page's breakdowns with their tab labels.
var usageBys = []struct{ Param, Label string }{
	{store.UsageBySource, "Source"}, {store.UsageByMachine, "Machine"}, {store.UsageByProject, "Project"}, {store.UsageByModel, "Model"},
}

const (
	defaultUsageRange = "30d"
	defaultUsageBy    = store.UsageBySource
)

// usageFields are the Usage fields in tile, chart and table order. Class
// colours a chart segment and its legend swatch.
var usageFields = []struct {
	Label, Class string
	Of           func(parser.Usage) int64
}{
	{"Input", "in", func(u parser.Usage) int64 { return u.Input }},
	{"Output", "out", func(u parser.Usage) int64 { return u.Output }},
	{"Cache read", "cr", func(u parser.Usage) int64 { return u.CacheRead }},
	{"Cache write", "cw", func(u parser.Usage) int64 { return u.CacheWrite }},
	{"Reasoning", "re", func(u parser.Usage) int64 { return u.Reasoning }},
}

// usageLink is one range or breakdown tab.
type usageLink struct {
	Label string
	Href  string
	On    bool
}

// usageNum is a count shown compactly, with its exact value on hover.
type usageNum struct {
	Short string
	Full  string
}

// usageLegend is one field's legend entry; Class is its swatch colour.
type usageLegend struct{ Label, Class string }

type usageTile struct {
	Label string
	usageNum
}

// usageRow is one breakdown row: its five Usage fields, then Sessions and
// Messages.
type usageRow struct {
	Label   string
	Title   string // hover text: a Project's full path
	Machine string // a Project's Machine
	Nums    []usageNum
}

// usageSeg is one field's slice of a day's bar, in chart units.
type usageSeg struct {
	Class string
	Y, H  string
}

type usageBar struct {
	X, W  string
	Title string
	Segs  []usageSeg
}

// usageChart is the daily stacked bar chart, drawn in a viewBox
// usageSlot units per day wide and usageChartH high.
type usageChart struct {
	ViewBox     string
	Bars        []usageBar
	First, Last string // the first and last day's labels
	Max         string // the tallest bar's total
}

const (
	usageSlot   = 10
	usageBarW   = 8
	usageChartH = 100
)

type usageView struct {
	Ranges  []usageLink
	Bys     []usageLink
	ByLabel string
	Refresh string
	Tiles   []usageTile
	NoUsage string // "12 Sessions in range reported no Usage"; "" when none
	Chart   usageChart
	Legend  []usageLegend
	Columns []string
	Rows    []usageRow
}

// usageURL is the Usage page for a range and breakdown.
func usageURL(rng, by string) string {
	return "/usage?" + url.Values{"range": {rng}, "by": {by}}.Encode()
}

// usage renders the Usage page (hub.md §4.7): tiles, a daily chart and a
// breakdown for one fixed window, read afresh on each load.
func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var rng usageRange
	for _, c := range usageRanges {
		if c.Param == q.Get("range") || rng.Param == "" && c.Param == defaultUsageRange {
			rng = c
		}
	}
	var by, byLabel string
	for _, c := range usageBys {
		if c.Param == q.Get("by") || by == "" && c.Param == defaultUsageBy {
			by, byLabel = c.Param, c.Label
		}
	}

	now := s.now()
	loc := now.Location()
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	var since int64
	if rng.Days > 0 {
		since = today.AddDate(0, 0, 1-rng.Days).UnixMilli()
	}
	rep, err := s.store.UsageReport(r.Context(), since, loc, by)
	if err != nil {
		s.internal(w, r, err)
		return
	}

	v := usageView{ByLabel: byLabel, Refresh: usageURL(rng.Param, by)}
	for _, c := range usageRanges {
		v.Ranges = append(v.Ranges, usageLink{Label: c.Label, Href: usageURL(c.Param, by), On: c == rng})
	}
	for _, c := range usageBys {
		v.Bys = append(v.Bys, usageLink{Label: c.Label, Href: usageURL(rng.Param, c.Param), On: c.Param == by})
	}
	for _, f := range usageFields {
		v.Tiles = append(v.Tiles, usageTile{f.Label, num(f.Of(rep.Total.Usage))})
		v.Legend = append(v.Legend, usageLegend{f.Label, f.Class})
		v.Columns = append(v.Columns, f.Label)
	}
	v.Tiles = append(v.Tiles, usageTile{"Sessions", num(int64(rep.Total.Sessions))}, usageTile{"Messages", num(int64(rep.Total.Messages))})
	v.Columns = append(v.Columns, "Sessions", "Messages")
	switch rep.NoUsage {
	case 0:
	case 1:
		v.NoUsage = "1 Session in range reported no Usage"
	default:
		v.NoUsage = strconv.Itoa(rep.NoUsage) + " Sessions in range reported no Usage"
	}

	first := today.AddDate(0, 0, 1-rng.Days)
	if rng.Days == 0 {
		first = today
		if len(rep.Days) > 0 && rep.Days[0].Day.Before(today) {
			first = rep.Days[0].Day
		}
	}
	v.Chart = usageChartFor(rep.Days, first, today)

	for _, g := range rep.Groups {
		row := usageRow{Label: g.Label}
		switch by {
		case store.UsageByProject:
			row.Label, row.Title = projectName(g.Key)
			row.Machine = g.Machine
		case store.UsageByMachine:
			row.Title = g.Key
		}
		for _, f := range usageFields {
			row.Nums = append(row.Nums, num(f.Of(g.Usage)))
		}
		row.Nums = append(row.Nums, num(int64(g.Sessions)), num(int64(g.Messages)))
		v.Rows = append(v.Rows, row)
	}
	s.render(w, r, http.StatusOK, usagePage(v))
}

// usageChartFor draws one stacked bar per day from first to last, both
// local midnights; a day with no Usage is an empty slot.
func usageChartFor(days []store.UsageDay, first, last time.Time) usageChart {
	const key = "2006-01-02"
	byDay := map[string]parser.Usage{}
	var top int64
	for _, d := range days {
		byDay[d.Day.Format(key)] = d.Usage
		top = max(top, d.Sum())
	}
	c := usageChart{First: first.Format("Jan 2"), Last: last.Format("Jan 2"), Max: num(top).Short}
	n := 0
	// AddDate, not Add, so a DST change keeps each step on a midnight.
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		u := byDay[day.Format(key)]
		b := usageBar{X: coord(float64(n*usageSlot + (usageSlot-usageBarW)/2)), W: coord(usageBarW)}
		b.Title = day.Format("Mon, Jan 2")
		y := float64(usageChartH)
		for _, f := range usageFields {
			v := f.Of(u)
			b.Title += fmt.Sprintf(" · %s %s", f.Label, num(v).Full)
			if v == 0 || top == 0 {
				continue
			}
			h := float64(v) / float64(top) * usageChartH
			y -= h
			b.Segs = append(b.Segs, usageSeg{Class: f.Class, Y: coord(y), H: coord(h)})
		}
		c.Bars = append(c.Bars, b)
		n++
	}
	c.ViewBox = fmt.Sprintf("0 0 %d %d", max(n, 1)*usageSlot, usageChartH)
	return c
}

func coord(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// num formats a count as e.g. "1.2M", with the exact "1,234,567".
func num(n int64) usageNum {
	var short string
	switch {
	case n >= 1e9:
		short = fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		short = fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		short = fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		short = strconv.FormatInt(n, 10)
	}
	return usageNum{Short: short, Full: thousands(n)}
}

// thousands writes n with comma separators.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if n < 0 {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
