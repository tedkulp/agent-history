// Package web serves the Hub's HTML interface: the home feed and the
// Transcript page (hub.md §4.7), rendered with templ.
package web

//go:generate go tool templ generate

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/tedkulp/agent-history/internal/hub/live"
	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

//go:embed static
var staticFS embed.FS

// feedPageSize is the number of rows per feed page (hub.md §4.7).
const feedPageSize = 50

type server struct {
	store *store.Store
	live  *live.Broadcaster
	log   *slog.Logger
	now   func() time.Time
}

// New returns the Web UI handler. Open Transcript pages hear from b when their
// Session is parsed again from live data; a nil b means they never do. Times
// render in time.Local, the container's TZ. A nil logger means
// slog.Default().
func New(s *store.Store, b *live.Broadcaster, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	if b == nil {
		b = live.New()
	}
	srv := &server{store: s, live: b, log: log, now: time.Now}
	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.feed)
	mux.HandleFunc("GET /events", srv.feedEvents)
	mux.HandleFunc("GET /sessions/{id}", srv.transcript)
	mux.HandleFunc("GET /sessions/{id}/events", srv.events)
	mux.HandleFunc("GET /sessions/{id}/messages", srv.transcriptTail)
	mux.HandleFunc("GET /sessions/{id}/parts/{part}/output", srv.toolOutput)
	mux.HandleFunc("GET /blobs/{sha}", srv.blob)
	mux.HandleFunc("GET /healthz", srv.healthz)
	mux.HandleFunc("GET /static/chroma.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(chromaCSS)
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return mux
}

// healthz answers 200 once the database answers SELECT 1. A Store exists
// only after its migrations ran, and serve starts listening only after the
// start-up passes (hub.md §4.1), so a 200 means the Hub is ready.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("health check failed", "err", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
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

// driftBanner is one "unrecognised data" banner above the feed (hub.md §4.7).
type driftBanner struct {
	Text string
	Href string
}

type feedView struct {
	Params   feedParams
	Reparse  string // the re-parse banner; "" when no re-parse is queued
	Banners  []driftBanner
	Chips    []chipRow
	Filtered bool
	Days     []feedDay
	More     string // "Load more" URL, "" on the last page
	Events   string // where the first page hears of live Sessions; "" on later pages
	Reload   string // the first page, which the "Sessions updated" pill reloads
}

// noProject is the project param value for "No project".
const noProject = "-"

// feedParams is the feed's or search's state, all of it in the URL.
type feedParams struct {
	Q                        string // the search box input
	Machine, Project, Source string
	Warnings                 bool
}

// feedParamsFrom reads the feed's or search's state from its URL query. A
// Project needs its Machine.
func feedParamsFrom(q url.Values) feedParams {
	p := feedParams{Q: q.Get("q"), Machine: q.Get("machine"), Project: q.Get("project"), Source: q.Get("source"), Warnings: q.Get("warnings") == "1"}
	if p.Machine == "" {
		p.Project = ""
	}
	return p
}

// shows reports whether the feed with p's chips lists the live Session s. It
// is store.chipWhere in Go, so a feed stream needs no query: a new chip
// changes both.
func (p feedParams) shows(s live.FeedSession) bool {
	return (p.Machine == "" || p.Machine == s.MachineID) &&
		(p.Source == "" || p.Source == s.Source) &&
		(p.Project == "" || p.Project == projectParam(s.ProjectCwd)) &&
		(!p.Warnings || s.Warnings)
}

func (p feedParams) filter() store.FeedFilter {
	f := store.FeedFilter{Machine: p.Machine, Source: p.Source, Warnings: p.Warnings}
	if p.Project != "" {
		cwd := p.Project
		if cwd == noProject {
			cwd = ""
		}
		f.Project = &cwd
	}
	return f
}

// withMachine is p with the Machine set to id ("" = any). A Project belongs to
// one Machine, so switching Machine drops it.
func (p feedParams) withMachine(id string) feedParams {
	if id != p.Machine {
		p.Machine, p.Project = id, ""
	}
	return p
}

// withQ is p with the search box set to q; "" clears it, keeping the chips.
func (p feedParams) withQ(q string) feedParams {
	p.Q = q
	return p
}

// url is the feed URL for p plus the extra key/value params.
func (p feedParams) url(extra ...string) string {
	v := url.Values{}
	for k, val := range map[string]string{"q": p.Q, "machine": p.Machine, "project": p.Project, "source": p.Source} {
		if val != "" {
			v.Set(k, val)
		}
	}
	if p.Warnings {
		v.Set("warnings", "1")
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
	p := feedParamsFrom(q)
	// Input that leaves no terms shows the feed (hub.md §3.7).
	if terms := store.ParseQuery(p.Q); len(terms) > 0 {
		s.search(w, r, p, terms)
		return
	}
	f := p.filter()
	continuesDay := ""
	if at, err := strconv.ParseInt(q.Get("before"), 10, 64); err == nil {
		// before_id breaks ties; without it, before is strict.
		id, _ := strconv.ParseInt(q.Get("before_id"), 10, 64)
		f.Before = &store.Cursor{At: at, ID: id}
		continuesDay = dayLabel(time.UnixMilli(at).In(s.now().Location()), s.now())
	}
	rows, err := s.store.Feed(r.Context(), f, feedPageSize+1)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v := feedView{Params: p, Filtered: p != feedParams{Q: p.Q}}
	if len(rows) > feedPageSize {
		rows = rows[:feedPageSize]
		last := rows[len(rows)-1]
		v.More = p.url("before", strconv.FormatInt(last.LastActivityAt, 10), "before_id", strconv.FormatInt(last.ID, 10))
	}
	v.Days = groupByDay(rows, s.now(), continuesDay)
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, http.StatusOK, feedRows(v))
		return
	}
	if f.Before == nil {
		// Input that left no terms isn't worth keeping on reload.
		v.Reload = p.withQ("").url()
		v.Events = "/events" + strings.TrimPrefix(v.Reload, "/")
	}
	if v.Chips, err = s.chips(r.Context(), p); err != nil {
		s.internal(w, r, err)
		return
	}
	if v.Banners, err = s.driftBanners(r.Context()); err != nil {
		s.internal(w, r, err)
		return
	}
	if v.Reparse, err = s.reparseBanner(r.Context()); err != nil {
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
		mrow.Chips = append(mrow.Chips, toggle(chip{Label: m.Label, Title: m.ID}, p.Machine, m.ID, p.withMachine))
	}
	srow := chipRow{Label: "Source"}
	for _, src := range sources {
		srow.Chips = append(srow.Chips, toggle(chip{Label: src}, p.Source, src, func(v string) feedParams {
			q := p
			q.Source = v
			return q
		}))
	}
	// The chip's param is warnings=1; toggle works on its string value.
	warnings := ""
	if p.Warnings {
		warnings = "1"
	}
	wrow := chipRow{Label: "Show", Chips: []chip{toggle(chip{Label: "⚠ has warnings", Title: "Sessions with Parse warnings or a failed parse"}, warnings, "1", func(v string) feedParams {
		q := p
		q.Warnings = v != ""
		return q
	})}}
	rows := []chipRow{mrow, srow, wrow}
	if p.Machine == "" {
		return rows, nil
	}
	projects, err := s.store.MachineProjects(ctx, p.Machine)
	if err != nil {
		return nil, err
	}
	prow := chipRow{Label: "Project"}
	for _, pc := range projects {
		c := chip{Count: pc.Sessions}
		c.Label, c.Title = projectName(pc.Cwd)
		prow.Chips = append(prow.Chips, toggle(c, p.Project, projectParam(pc.Cwd), func(v string) feedParams {
			q := p
			q.Project = v
			return q
		}))
	}
	return append(rows, prow), nil
}

// driftBanners builds one banner per Source version with recent Parse
// warnings, linking to those Sessions (hub.md §4.7).
func (s *server) driftBanners(ctx context.Context) ([]driftBanner, error) {
	drift, err := s.store.DriftBanners(ctx)
	if err != nil {
		return nil, err
	}
	var out []driftBanner
	for _, d := range drift {
		name := d.Source
		if d.SourceVersion != "" {
			name += " " + d.SourceVersion
		}
		sessions := "1 Session has"
		if d.Sessions != 1 {
			sessions = strconv.Itoa(d.Sessions) + " Sessions have"
		}
		out = append(out, driftBanner{
			Text: name + ": " + sessions + " unrecognised data",
			Href: feedParams{Source: d.Source, Warnings: true}.url(),
		})
	}
	return out, nil
}

// reparseBanner is "Re-parsing N Sessions…" while re-parse jobs remain
// queued, else "" (hub.md §4.7).
func (s *server) reparseBanner(ctx context.Context) (string, error) {
	n, err := s.store.QueuedReparses(ctx)
	if err != nil || n == 0 {
		return "", err
	}
	if n == 1 {
		return "Re-parsing 1 Session…", nil
	}
	return "Re-parsing " + strconv.Itoa(n) + " Sessions…", nil
}

// toggle completes chip c for the value val of a param now set to cur. The
// chip is on when cur is val, and links to with("") to turn it off; otherwise
// it links to with(val).
func toggle(c chip, cur, val string, with func(string) feedParams) chip {
	c.On = cur == val
	if c.On {
		val = ""
	}
	c.Href = with(val).url()
	return c
}

// groupByDay groups feed rows (newest first) under "Today", "Yesterday" or
// their date. A first group on the day labelled continuesDay gets no label,
// since it continues the previous page.
func groupByDay(rows []store.FeedRow, now time.Time, continuesDay string) []feedDay {
	var (
		days []feedDay
		cur  string
	)
	for _, row := range rows {
		t := time.UnixMilli(row.LastActivityAt).In(now.Location())
		label := dayLabel(t, now)
		if len(days) == 0 || label != cur {
			d := feedDay{Label: label}
			if len(days) == 0 && label == continuesDay {
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
	WarningNote string // the Notes summary; "" without warnings
	ParentLabel string // "↰ child of …" for a Child Session
	ParentHref  string // the spawning call in the parent; "" when the parent is a stub
	ForkLabel   string // "forked from …" for a fork
	ForkHref    string // the Session it was forked from; "" when that is a stub
}

// sessionHref is a Session's Transcript page.
func sessionHref(id int64) string { return "/sessions/" + strconv.FormatInt(id, 10) }

// childEntry is one Child Session in the Transcript's outline.
type childEntry struct {
	Href  string
	Title string
}

type messageView struct {
	ID     string
	Role   string
	Time   string
	Chunks []chunkView
	Marker bool // only marker Parts: a centred pill, no time
}

// outlineEntry is one user prompt in the Transcript's outline.
type outlineEntry struct {
	ID   string
	Text string
	Time string
}

// liveState is what a Transcript page needs to catch up with a live parse
// (hub.md §4.7): where to listen and fetch, and the Messages it holds. After
// and Count describe every stored Message, including ones with nothing
// renderable yet.
type liveState struct {
	Events   string
	Messages string
	After    string // the last Message's id; "" when there are none
	Count    int
}

// withTail is st holding count Messages ending with msgs, the ones just
// loaded; with none loaded, After stays as it was.
func (st liveState) withTail(msgs []store.TranscriptMessage, count int) liveState {
	st.Count = count
	if len(msgs) > 0 {
		st.After = msgs[len(msgs)-1].ID
	}
	return st
}

// transcriptView is what the Transcript templates show.
type transcriptView struct {
	Head     headerView
	Outline  []outlineEntry
	Children []childEntry
	Messages []messageView
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
	st := liveState{Events: sessionHref(id) + "/events", Messages: sessionHref(id) + "/messages"}.withTail(msgs, len(msgs))
	s.render(w, r, http.StatusOK, transcriptPage(st, s.transcriptView(h, msgs)))
}

// transcriptTail serves what an open Transcript page needs after a live parse:
// its last Message re-rendered plus every later one, the outline entries they
// add, and the header and Child Sessions afresh (hub.md §4.7). after and
// count are the page's last Message and how many it holds. When the Messages
// before its last one moved, it answers 409 and the page offers a reload.
func (s *server) transcriptTail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	after := q.Get("after")
	count, err := strconv.Atoi(q.Get("count"))
	if err != nil || count < 0 {
		http.Error(w, "bad count", http.StatusBadRequest)
		return
	}
	h, msgs, ok, err := s.store.TranscriptTail(r.Context(), id, after, count)
	switch {
	case errors.Is(err, store.ErrTranscriptChanged):
		http.Error(w, "Transcript changed", http.StatusConflict)
		return
	case err != nil:
		s.internal(w, r, err)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}
	st := liveState{After: after}.withTail(msgs, max(count-1, 0)+len(msgs))
	s.render(w, r, http.StatusOK, transcriptUpdate(st, s.transcriptView(h, msgs)))
}

// heartbeatEvery is how often the events stream sends a comment, so proxies
// and browsers keep an idle stream open.
var heartbeatEvery = 30 * time.Second

// events streams a "changed" event each time the Session is parsed again from
// live data (hub.md §4.7). Once the Session is found it reads nothing from
// the store, so an idle open page costs no queries.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ok, err := s.store.HasTranscript(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	changed, stop := s.live.Subscribe(id)
	defer stop()
	send, ok := startStream(w)
	if !ok {
		return
	}
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		var msg string
		select {
		case <-r.Context().Done():
			return
		case _, open := <-changed:
			if !open {
				return // shutting down
			}
			msg = "event: changed\ndata: changed\n\n"
		case <-beat.C:
			msg = heartbeat
		}
		if !send(msg) {
			return
		}
	}
}

// feedDebounce is the least time between two events on one feed's stream.
var feedDebounce = 2 * time.Second

// feedEvents streams a "changed" event naming the Sessions, among those the
// feed with the query's chips lists, that were parsed from live data since
// the last event, at most one event per feedDebounce (hub.md §4.7). It reads
// nothing from the store, so an idle open feed costs no queries.
func (s *server) feedEvents(w http.ResponseWriter, r *http.Request) {
	p := feedParamsFrom(r.URL.Query())
	sub, stop := s.live.SubscribeFeed()
	defer stop()
	send, ok := startStream(w)
	if !ok {
		return
	}
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	var (
		last time.Time
		wait <-chan time.Time // set while an event waits out the debounce
	)
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-sub.Ready():
			if !open {
				return // shutting down
			}
			if wait != nil {
				continue
			}
			if d := feedDebounce - time.Since(last); d > 0 {
				wait = time.After(d)
				continue
			}
		case <-wait:
			wait = nil
		case <-beat.C:
			if !send(heartbeat) {
				return
			}
			continue
		}
		var ids []int64
		for _, sess := range sub.Take() {
			if p.shows(sess) {
				ids = append(ids, sess.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		slices.Sort(ids)
		data := make([]string, len(ids))
		for i, id := range ids {
			data[i] = strconv.FormatInt(id, 10)
		}
		last = time.Now()
		if !send("event: changed\ndata: " + strings.Join(data, ",") + "\n\n") {
			return
		}
	}
}

// heartbeat is the comment an event stream sends every heartbeatEvery.
const heartbeat = ": heartbeat\n\n"

// startStream answers with an event stream and returns a function that sends
// and flushes one message, reporting whether the client is still there. It
// sets X-Accel-Buffering: no so a proxy passes each message on at once.
func startStream(w http.ResponseWriter) (send func(msg string) bool, ok bool) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return nil, false
	}
	return func(msg string) bool {
		if _, err := io.WriteString(w, msg); err != nil {
			return false
		}
		return rc.Flush() == nil
	}, true
}

// transcriptView turns a Session's header and Messages into what the
// Transcript templates show.
func (s *server) transcriptView(h store.SessionHeader, msgs []store.TranscriptMessage) transcriptView {
	var (
		outline  []outlineEntry
		children []childEntry
		views    []messageView
	)
	hv := headerView{SessionHeader: h, Started: s.localTime(h.StartedAt, "Jan 2, 2006 15:04")}
	hv.Title = titleOr(h.Title, h.NativeID)
	hv.Project, hv.ProjectFull = projectName(h.ProjectCwd)
	hv.MachineHref = feedURL(h.MachineID, nil)
	hv.ProjectHref = feedURL(h.MachineID, &h.ProjectCwd)
	hv.WarningNote = warningNote(h.Warnings)
	if p := h.Parent; p != nil {
		hv.ParentLabel = "↰ child of " + titleOr(p.Title, p.NativeID)
		if p.Parsed {
			hv.ParentHref = sessionHref(p.ID)
			if p.CallPart != "" {
				hv.ParentHref += "#p-" + p.CallPart
			}
		}
	}
	if f := h.ForkedFrom; f != nil {
		hv.ForkLabel = "forked from " + titleOr(f.Title, f.NativeID)
		if f.Parsed {
			hv.ForkHref = sessionHref(f.ID)
		}
	}
	childHrefs := map[string]string{}
	for _, c := range h.Children {
		children = append(children, childEntry{Href: sessionHref(c.ID), Title: titleOr(c.Title, c.NativeID)})
	}
	for native, cid := range h.CallChildren {
		childHrefs[native] = sessionHref(cid)
	}
	for _, m := range msgs {
		v := messageView{ID: m.ID, Role: m.Role, Time: s.localTime(m.Timestamp, "Jan 2 15:04")}
		v.Chunks = s.chunks(h.ID, m.Parts, childHrefs)
		// Messages with nothing renderable yet are skipped.
		if len(v.Chunks) == 0 {
			continue
		}
		v.Marker = true
		for _, c := range v.Chunks {
			v.Marker = v.Marker && c.Marker != nil
		}
		views = append(views, v)
		if m.Role == parser.MessageUser {
			if t := promptLine(m.Parts); t != "" {
				outline = append(outline, outlineEntry{ID: m.ID, Text: t, Time: s.localTime(m.Timestamp, "15:04")})
			}
		}
	}
	return transcriptView{Head: hv, Outline: outline, Children: children, Messages: views}
}

// outlineMax is the most characters of a prompt the outline shows.
const outlineMax = 60

// noteKinds is how many warning kinds the Notes summary names.
const noteKinds = 3

// promptLine is the first line of a user Message's first text Part, for the
// outline.
func promptLine(parts []store.TranscriptPart) string {
	for _, p := range parts {
		if p.Kind != parser.KindText {
			continue
		}
		var tp parser.TextPayload
		if json.Unmarshal([]byte(p.Payload), &tp) != nil {
			continue
		}
		for _, l := range strings.Split(tp.Text, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				return cut(l, outlineMax)
			}
		}
	}
	return ""
}

// warningNote summarises Parse warnings, e.g. "12 items not understood
// (unknown_type: foo ×10, orphan: tool_result ×2)" (hub.md §4.7).
func warningNote(ws []store.ParseWarning) string {
	if len(ws) == 0 {
		return ""
	}
	var (
		total int
		kinds []string
	)
	for i, w := range ws {
		total += w.Count
		switch {
		case i < noteKinds:
			k := w.Kind
			if w.SourceType != "" {
				k += ": " + w.SourceType
			}
			kinds = append(kinds, fmt.Sprintf("%s ×%d", k, w.Count))
		case i == noteKinds:
			kinds = append(kinds, "…")
		}
	}
	items := "items"
	if total == 1 {
		items = "item"
	}
	return fmt.Sprintf("%d %s not understood (%s)", total, items, strings.Join(kinds, ", "))
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
