// Package web serves the Hub's HTML interface: the home feed and the
// Transcript page (hub.md §4.7), rendered with templ.
package web

//go:generate go tool templ generate

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
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
	Label string
	Rows  []feedRow
}

type feedRow struct {
	store.FeedRow
	Time        string
	Prompt      string
	Project     string
	ProjectFull string
}

func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Feed(r.Context(), feedPageSize)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, feedPage(groupByDay(rows, s.now())))
}

// groupByDay groups feed rows (newest first) under "Today", "Yesterday" or
// their date.
func groupByDay(rows []store.FeedRow, now time.Time) []feedDay {
	var days []feedDay
	for _, row := range rows {
		t := time.UnixMilli(row.LastActivityAt).In(now.Location())
		label := dayLabel(t, now)
		if len(days) == 0 || days[len(days)-1].Label != label {
			days = append(days, feedDay{Label: label})
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
