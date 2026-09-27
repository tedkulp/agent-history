// Package web serves the Hub's HTML interface: the home feed and the
// Transcript page (hub.md §4.7), rendered with templ.
package web

//go:generate go tool templ generate

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

//go:embed static
var staticFS embed.FS

// feedPageSize is the number of rows per feed page (hub.md §4.7).
const feedPageSize = 50

type server struct {
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
}

// New returns the Web UI handler. Times render in time.Local, the container's
// TZ. A nil logger means slog.Default().
func New(s *store.Store, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	srv := &server{store: s, log: log, now: time.Now}
	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.feed)
	mux.HandleFunc("GET /sessions/{id}", srv.transcript)
	mux.HandleFunc("GET /static/chroma.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(chromaCSS)
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return mux
}

type feedDay struct {
	Label string // "" continues the previous page's last day
	Rows  []feedRow
}

type feedRow struct {
	store.FeedRow
	Time        string
	Prompt      string
	Project     string
	ProjectFull string
}

// chip is one filter chip; Href toggles it (hub.md §4.7).
type chip struct {
	Label string
	Title string // hover text
	Count int    // shown when > 0
	On    bool
	Href  string
}

type chipRow struct {
	Label string
	Chips []chip
}

type feedView struct {
	Chips    []chipRow
	Filtered bool
	Days     []feedDay
	More     string // "Load more" URL, "" on the last page
}

// noProject is the project param value for "No project".
const noProject = "-"

// feedParams is the feed's state, all of it in the URL.
type feedParams struct {
	Machine, Project, Source string
}

func (p feedParams) filter() store.FeedFilter {
	f := store.FeedFilter{Machine: p.Machine, Source: p.Source}
	if p.Project != "" {
		cwd := p.Project
		if cwd == noProject {
			cwd = ""
		}
		f.Project = &cwd
	}
	return f
}

// url is the feed URL for p, with extra params appended in order.
func (p feedParams) url(extra ...string) string {
	v := url.Values{}
	for k, val := range map[string]string{"machine": p.Machine, "project": p.Project, "source": p.Source} {
		if val != "" {
			v.Set(k, val)
		}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// feedURL links to the feed filtered to a Machine and, when project is
// non-nil, one of its Projects ("" = No project).
func feedURL(machineID string, project *string) string {
	p := feedParams{Machine: machineID}
	if project != nil {
		p.Project = projectParam(*project)
	}
	return p.url()
}

func projectParam(cwd string) string {
	if cwd == "" {
		return noProject
	}
	return cwd
}

func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := feedParams{Machine: q.Get("machine"), Project: q.Get("project"), Source: q.Get("source")}
	if p.Machine == "" {
		p.Project = ""
	}
	f := p.filter()
	after := ""
	if at, err := strconv.ParseInt(q.Get("before"), 10, 64); err == nil {
		// before_id breaks ties; without it, before is strict.
		id, _ := strconv.ParseInt(q.Get("before_id"), 10, 64)
		f.Before = &store.Cursor{At: at, ID: id}
		after = dayLabel(time.UnixMilli(at).In(s.now().Location()), s.now())
	}
	rows, err := s.store.Feed(r.Context(), f, feedPageSize+1)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v := feedView{Filtered: p != feedParams{}}
	if len(rows) > feedPageSize {
		rows = rows[:feedPageSize]
		last := rows[len(rows)-1]
		v.More = p.url("before", strconv.FormatInt(last.LastActivityAt, 10), "before_id", strconv.FormatInt(last.ID, 10))
	}
	v.Days = groupByDay(rows, s.now(), after)
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, http.StatusOK, feedRows(v))
		return
	}
	if v.Chips, err = s.chips(r.Context(), p); err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, feedPage(v))
}

// chips builds the Machine and Source rows and, with a Machine picked, its
// Project row.
func (s *server) chips(ctx context.Context, p feedParams) ([]chipRow, error) {
	machines, err := s.store.FeedMachines(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := s.store.FeedSources(ctx)
	if err != nil {
		return nil, err
	}
	mrow := chipRow{Label: "Machine"}
	for _, m := range machines {
		c := chip{Label: m.Label, Title: m.ID, On: m.ID == p.Machine}
		q := p
		q.Project = ""
		if c.On {
			q.Machine = ""
		} else {
			q.Machine = m.ID
		}
		c.Href = q.url()
		mrow.Chips = append(mrow.Chips, c)
	}
	srow := chipRow{Label: "Source"}
	for _, src := range sources {
		c := chip{Label: src, On: src == p.Source}
		q := p
		if c.On {
			q.Source = ""
		} else {
			q.Source = src
		}
		c.Href = q.url()
		srow.Chips = append(srow.Chips, c)
	}
	rows := []chipRow{mrow, srow}
	if p.Machine == "" {
		return rows, nil
	}
	projects, err := s.store.MachineProjects(ctx, p.Machine)
	if err != nil {
		return nil, err
	}
	prow := chipRow{Label: "Project"}
	for _, pc := range projects {
		key := projectParam(pc.Cwd)
		c := chip{Count: pc.Sessions, On: key == p.Project}
		c.Label, c.Title = projectName(pc.Cwd)
		q := p
		if c.On {
			q.Project = ""
		} else {
			q.Project = key
		}
		c.Href = q.url()
		prow.Chips = append(prow.Chips, c)
	}
	return append(rows, prow), nil
}

// groupByDay groups feed rows (newest first) under "Today", "Yesterday" or
// their date. A first group on the day named by after gets no label, since it
// continues the previous page.
func groupByDay(rows []store.FeedRow, now time.Time, after string) []feedDay {
	var (
		days []feedDay
		cur  string
	)
	for _, row := range rows {
		t := time.UnixMilli(row.LastActivityAt).In(now.Location())
		label := dayLabel(t, now)
		if len(days) == 0 || label != cur {
			d := feedDay{Label: label}
			if len(days) == 0 && label == after {
				d.Label = ""
			}
			days = append(days, d)
			cur = label
		}
		v := feedRow{FeedRow: row, Time: t.Format("15:04"), Prompt: oneLine(row.FirstPrompt)}
		v.Title = titleOr(row.Title, row.NativeID)
		v.Project, v.ProjectFull = projectName(row.ProjectCwd)
		d := &days[len(days)-1]
		d.Rows = append(d.Rows, v)
	}
	return days
}

func dayLabel(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	day := time.Date(y1, m1, d1, 0, 0, 0, 0, now.Location())
	y2, m2, d2 := now.Date()
	today := time.Date(y2, m2, d2, 0, 0, 0, 0, now.Location())
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case y1 == y2:
		return t.Format("Mon, Jan 2")
	}
	return t.Format("Mon, Jan 2, 2006")
}

// titleOr is the Session's title, or its native id when it has none.
func titleOr(title, nativeID string) string {
	if title == "" {
		return nativeID
	}
	return title
}

// localTime formats a Unix-millisecond time in the display zone, or "" for 0.
func (s *server) localTime(ms int64, layout string) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).In(s.now().Location()).Format(layout)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// projectName is the Project's display name (the cwd's basename) and full path.
func projectName(cwd string) (name, full string) {
	if cwd == "" {
		return "No project", ""
	}
	return path.Base(cwd), cwd
}

type headerView struct {
	store.SessionHeader
	Project     string
	ProjectFull string
	Started     string
	MachineHref string
	ProjectHref string
}

type messageView struct {
	ID    string
	Role  string
	Time  string
	Texts []string // rendered HTML of each text Part
}

func (s *server) transcript(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.render(w, r, http.StatusNotFound, notFoundPage())
		return
	}
	h, msgs, ok, err := s.store.Transcript(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		s.render(w, r, http.StatusNotFound, notFoundPage())
		return
	}
	hv := headerView{SessionHeader: h, Started: s.localTime(h.StartedAt, "Jan 2, 2006 15:04")}
	hv.Title = titleOr(h.Title, h.NativeID)
	hv.Project, hv.ProjectFull = projectName(h.ProjectCwd)
	hv.MachineHref = feedURL(h.MachineID, nil)
	hv.ProjectHref = feedURL(h.MachineID, &h.ProjectCwd)
	var views []messageView
	for _, m := range msgs {
		v := messageView{ID: m.ID, Role: m.Role, Time: s.localTime(m.Timestamp, "Jan 2 15:04")}
		for _, p := range m.Parts {
			if p.Kind != parser.KindText {
				continue
			}
			var tp parser.TextPayload
			if err := json.Unmarshal([]byte(p.Payload), &tp); err != nil {
				s.log.Warn("bad text payload", "session", id, "part", p.ID, "err", err)
				continue
			}
			v.Texts = append(v.Texts, renderMarkdown(tp.Text))
		}
		// Messages with nothing renderable yet (e.g. only tool calls) are skipped.
		if len(v.Texts) > 0 {
			views = append(views, v)
		}
	}
	s.render(w, r, http.StatusOK, transcriptPage(hv, views))
}

func (s *server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Warn("rendering page", "path", r.URL.Path, "err", err)
	}
}

func (s *server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
