package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/a-h/templ"

	"github.com/tedkulp/agent-history/internal/hub/store"
)

// searchPageSize is the number of Sessions per search page, and
// searchMoreSize the number of Hits a group's "+N more" loads (hub.md §4.7).
const (
	searchPageSize = 20
	searchMoreSize = 20
)

// groupView is one Session of the search results with its best Hits.
type groupView struct {
	ID          string          // the Session id
	Href        string          // the Transcript, with the query to highlight
	Title       templ.Component // the Session title with the terms marked
	ChildOf     string          // "↳ child of …" for a Child Session
	Source      string
	Machine     string
	Project     string
	ProjectFull string
	Time        string // last activity
	Count       string // "14 hits"
	Hits        []hitView
	More        moreView
}

// hitView is one Message Hit row of a group.
type hitView struct {
	Href    string
	Role    string
	Time    string
	Snippet templ.Component
}

// moreView is a group's footer: "+N more" loads the next Hits in place,
// beside a link to the Transcript. Shown while N > 0, and after the last
// load.
type moreView struct {
	N    int
	URL  string
	Href string // the Transcript
}

// facetItem is one facet count; Href adds its chip (hub.md §4.7).
type facetItem struct {
	Label string
	Title string // hover text
	Hits  int
	On    bool
	Href  string
}

type facetGroup struct {
	Label string
	Items []facetItem
}

type searchView struct {
	Params feedParams
	Chips  []chipRow
	Facets []facetGroup
	Groups []groupView
	More   string // "Load more" URL, "" on the last page
}

// search serves /?q= with terms: the matching Sessions, ranked, each with
// its best Hits, beside a facet sidebar; or with htmx, the next page of
// Sessions.
func (s *server) search(w http.ResponseWriter, r *http.Request, p feedParams, terms []string) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	f := p.filter()
	groups, err := s.store.SearchSessions(r.Context(), terms, f, offset, searchPageSize+1)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v := searchView{Params: p}
	if len(groups) > searchPageSize {
		groups = groups[:searchPageSize]
		v.More = p.url("offset", strconv.Itoa(offset+searchPageSize))
	}
	mark := termMarker(terms)
	for _, g := range groups {
		v.Groups = append(v.Groups, s.groupFor(g, p.Q, mark))
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, http.StatusOK, searchGroups(v))
		return
	}
	if v.Chips, err = s.chips(r.Context(), p); err != nil {
		s.internal(w, r, err)
		return
	}
	if v.Facets, err = s.facets(r.Context(), p, terms, f); err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, searchPage(v))
}

// groupFor is the view of one Session of the results. A title Hit counts,
// but has no row of its own: the header shows the marked title.
func (s *server) groupFor(g store.SessionHits, q string, mark func(string) string) groupView {
	v := groupView{
		ID:      strconv.FormatInt(g.SessionID, 10),
		Href:    transcriptHL(g.SessionID, q),
		Title:   templ.Raw(mark(titleOr(g.Title, g.NativeID))),
		Source:  g.Source,
		Machine: g.Machine,
		Time:    s.localTime(g.LastActivityAt, "Jan 2 15:04"),
		Count:   hitCount(g.Hits),
	}
	v.Project, v.ProjectFull = projectName(g.ProjectCwd)
	if g.ParentID != 0 {
		v.ChildOf = "↳ child of " + g.ParentTitle
	}
	for _, h := range g.Top {
		v.Hits = append(v.Hits, s.hitFor(h, q))
	}
	messages := g.Hits
	if g.TitleHit {
		messages--
	}
	v.More = moreFor(g.SessionID, q, 0, messages-len(g.Top))
	return v
}

// hitFor is the view of one Message Hit: a link to its anchor with the
// query to highlight.
func (s *server) hitFor(h store.Hit, q string) hitView {
	return hitView{
		Href:    transcriptHL(h.SessionID, q) + "#m-" + h.MessageID,
		Role:    h.Role,
		Time:    s.localTime(h.Timestamp, "Jan 2 15:04"),
		Snippet: templ.Raw(markSnippet(h.Snippet)),
	}
}

// moreFor is a group's footer with n Hits left to load, the next from
// offset past the top ones.
func moreFor(sessionID int64, q string, offset, n int) moreView {
	return moreView{
		N:    n,
		URL:  sessionHref(sessionID) + "/hits?" + url.Values{"q": {q}, "offset": {strconv.Itoa(offset)}}.Encode(),
		Href: transcriptHL(sessionID, q),
	}
}

// transcriptHL is a Session's Transcript with q to highlight.
func transcriptHL(sessionID int64, q string) string {
	return sessionHref(sessionID) + "?" + url.Values{"hl": {q}}.Encode()
}

// hitCount is "1 hit" or "n hits".
func hitCount(n int) string {
	if n == 1 {
		return "1 hit"
	}
	return strconv.Itoa(n) + " hits"
}

// sessionHits serves a group's "+N more": the Session's next Message Hits
// for q in Transcript order, and the footer again with what's left
// (hub.md §4.7).
func (s *server) sessionHits(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query().Get("q")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	hits, total, err := s.store.SessionSearchHits(r.Context(), id, store.ParseQuery(q), offset, searchMoreSize)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if total == 0 {
		http.NotFound(w, r)
		return
	}
	var v groupView
	for _, h := range hits {
		v.Hits = append(v.Hits, s.hitFor(h, q))
	}
	shown := offset + len(hits)
	v.More = moreFor(id, q, shown, max(total-store.SearchTopHits-shown, 0))
	s.render(w, r, http.StatusOK, groupHits(v))
}

// facets builds the Machine, Source and Project facet groups. Each count
// links to the search with that chip added.
func (s *server) facets(ctx context.Context, p feedParams, terms []string, f store.FeedFilter) ([]facetGroup, error) {
	fs, err := s.store.SearchSessionFacets(ctx, terms, f)
	if err != nil {
		return nil, err
	}
	machines := facetGroup{Label: "Machine"}
	for _, fc := range fs.Machines {
		machines.Items = append(machines.Items, facetItem{Label: fc.Machine, Title: fc.MachineID, Hits: fc.Hits, On: p.Machine == fc.Value, Href: p.withMachine(fc.Value).url()})
	}
	sources := facetGroup{Label: "Source"}
	for _, fc := range fs.Sources {
		q := p
		q.Source = fc.Value
		sources.Items = append(sources.Items, facetItem{Label: fc.Value, Hits: fc.Hits, On: p.Source == fc.Value, Href: q.url()})
	}
	projects := facetGroup{Label: "Project"}
	for _, fc := range fs.Projects {
		q := p
		q.Machine, q.Project = fc.MachineID, projectParam(fc.Value)
		name, full := projectName(fc.Value)
		if full == "" {
			full = name
		}
		projects.Items = append(projects.Items, facetItem{
			Label: fc.Machine + " › " + name, Title: fc.Machine + ": " + full, Hits: fc.Hits,
			On: p.Machine == fc.MachineID && p.Project == q.Project, Href: q.url(),
		})
	}
	return []facetGroup{machines, sources, projects}, nil
}

// markSnippet HTML-escapes an FTS5 snippet, then turns its sentinels into
// <mark> tags, so Transcript text can't inject markup (hub.md §3.7).
func markSnippet(s string) string {
	s = html.EscapeString(s)
	return strings.NewReplacer(store.SnippetOpen, "<mark>", store.SnippetClose, "</mark>").Replace(s)
}

// termMarker returns a function that HTML-escapes text and wraps each
// case-insensitive match of the terms in <mark>, the way FTS5 matched them:
// a match starts at a word, and ends at one except for the last term, which
// is a prefix. A phrase matches across any run of non-word characters.
func termMarker(terms []string) func(string) string {
	var alts []string
	for i, t := range terms {
		words := strings.Fields(t)
		for j, w := range words {
			words[j] = regexp.QuoteMeta(w)
		}
		alt := strings.Join(words, `[^\pL\pN]+`)
		if i < len(terms)-1 {
			alt += `(?:[^\pL\pN]|$)`
		}
		alts = append(alts, alt)
	}
	if len(alts) == 0 {
		return html.EscapeString
	}
	// RE2 has no lookbehind: the leading group consumes the character before
	// a match, which stays unmarked.
	re := regexp.MustCompile(`(?i)(^|[^\pL\pN])(` + strings.Join(alts, "|") + `)`)
	return func(s string) string {
		var b strings.Builder
		last := 0
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			start, end := m[4], m[5]
			// Trim the trailing boundary a non-last term consumed.
			for end > start {
				r, size := utf8.DecodeLastRuneInString(s[start:end])
				if unicode.IsLetter(r) || unicode.IsNumber(r) {
					break
				}
				end -= size
			}
			b.WriteString(html.EscapeString(s[last:start]))
			b.WriteString("<mark>" + html.EscapeString(s[start:end]) + "</mark>")
			last = end
		}
		b.WriteString(html.EscapeString(s[last:]))
		return b.String()
	}
}
