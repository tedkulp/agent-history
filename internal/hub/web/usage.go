package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// usageLegend is one series of a chart: a Usage field or a breakdown group.
// Class colours its segments and legend swatch; Title is hover text.
type usageLegend struct{ Label, Title, Class string }

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

// usageSeg is one series' slice of a day's bar, in chart units.
type usageSeg struct {
	Class string
	Y, H  string
}

type usageBar struct {
	X, W  string
	Title string
	Segs  []usageSeg
}

// usageChart is a daily stacked bar chart, drawn in a viewBox usageSlot
// units per day wide and usageChartH high.
type usageChart struct {
	Title       string
	ViewBox     string
	Bars        []usageBar
	First, Last string // the first and last day's labels
	Max         string // the tallest bar's total
	Legend      []usageLegend
}

const (
	usageSlot   = 10
	usageBarW   = 8
	usageChartH = 100
)

// usageTopGroups is how many breakdown groups the group chart shows on
// their own; the rest stack as one "Other".
const usageTopGroups = 5

type usageView struct {
	Ranges  []usageLink
	Bys     []usageLink
	ByLabel string
	Refresh string
	Tiles   []usageTile
	NoUsage string     // "12 Sessions in range reported no Usage"; "" when none
	Chart   usageChart // by Usage field
	ByChart usageChart // by the breakdown's groups
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
	v.Chart = fieldChart(rep.Days, first, today)

	for _, g := range rep.Groups {
		row := usageRow{Label: g.Label}
		switch by {
		case store.UsageByProject:
			row.Label, row.Title = projectName(g.Label)
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
	v.ByChart = groupChart(rep, v.Rows, byLabel, first, today)
	s.render(w, r, http.StatusOK, usagePage(v))
}

// dayKey is a day's key in a chart's per-day values.
const dayKey = "2006-01-02"

// fieldChart stacks each day's Usage by field.
func fieldChart(days []store.UsageDay, first, last time.Time) usageChart {
	byDay := map[string][]int64{}
	for _, d := range days {
		for _, f := range usageFields {
			byDay[d.Day.Format(dayKey)] = append(byDay[d.Day.Format(dayKey)], f.Of(d.Usage))
		}
	}
	var series []usageLegend
	for _, f := range usageFields {
		series = append(series, usageLegend{Label: f.Label, Class: f.Class})
	}
	return drawChart("Tokens per day", series, byDay, first, last)
}

// groupChart stacks each day's tokens by the breakdown's top groups, with
// the rest as "Other". rows are the breakdown's table rows, in rep.Groups
// order, for the groups' labels.
func groupChart(rep store.UsageReport, rows []usageRow, byLabel string, first, last time.Time) usageChart {
	var (
		series []usageLegend
		keys   []string
		rest   bool
	)
	for i, g := range rep.Groups {
		if g.Sum() == 0 {
			continue // most tokens first: none of the rest have any
		}
		if len(keys) == usageTopGroups {
			rest = true
			break
		}
		l := usageLegend{Label: rows[i].Label, Title: rows[i].Title, Class: "g" + strconv.Itoa(len(keys)+1)}
		if rows[i].Machine != "" {
			l.Label += " · " + rows[i].Machine
		}
		series = append(series, l)
		keys = append(keys, g.Key)
	}
	if rest {
		series = append(series, usageLegend{Label: "Other", Class: "g0"})
	}
	byDay := map[string][]int64{}
	for _, d := range rep.GroupDays {
		var vals []int64
		other := int64(0)
		for _, t := range d.Tokens {
			other += t
		}
		for _, k := range keys {
			vals = append(vals, d.Tokens[k])
			other -= d.Tokens[k]
		}
		if rest {
			vals = append(vals, other)
		}
		byDay[d.Day.Format(dayKey)] = vals
	}
	return drawChart("Tokens per day by "+strings.ToLower(byLabel), series, byDay, first, last)
}

// drawChart draws one stacked bar per day from first to last, both local
// midnights, from each day's values, one per series; a day with none is an
// empty slot.
func drawChart(title string, series []usageLegend, byDay map[string][]int64, first, last time.Time) usageChart {
	var top int64
	for _, vals := range byDay {
		var sum int64
		for _, v := range vals {
			sum += v
		}
		top = max(top, sum)
	}
	c := usageChart{Title: title, First: first.Format("Jan 2"), Last: last.Format("Jan 2"), Max: num(top).Short, Legend: series}
	n := 0
	// AddDate, not Add, so a DST change keeps each step on a midnight.
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		vals := byDay[day.Format(dayKey)]
		b := usageBar{X: coord(float64(n*usageSlot + (usageSlot-usageBarW)/2)), W: coord(usageBarW)}
		b.Title = day.Format("Mon, Jan 2")
		y := float64(usageChartH)
		for i, s := range series {
			var v int64
			if i < len(vals) {
				v = vals[i]
			}
			b.Title += fmt.Sprintf(" · %s %s", s.Label, num(v).Full)
			if v == 0 {
				continue
			}
			h := float64(v) / float64(top) * usageChartH
			y -= h
			b.Segs = append(b.Segs, usageSeg{Class: s.Class, Y: coord(y), H: coord(h)})
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
