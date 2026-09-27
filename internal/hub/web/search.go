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

// searchPageSize is the number of hits per search page (hub.md §4.7).
const searchPageSize = 50

type hitView struct {
	Href        string
	Title       templ.Component // the Session title with the terms marked
	Snippet     templ.Component // nil for a title hit
	Source      string
	Machine     string
	Project     string
	ProjectFull string
	Role        string
	ChildOf     string // "↳ child of …" for a hit in a Child Session
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
	Hits   []hitView
	More   string // "Load more" URL, "" on the last page
}

// search serves /?q= with terms: a ranked hit list with a facet sidebar, or
// with htmx, the next page of hits.
func (s *server) search(w http.ResponseWriter, r *http.Request, p feedParams, terms []string) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	f := p.filter()
	hits, err := s.store.Search(r.Context(), terms, f, offset, searchPageSize+1)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v := searchView{Params: p}
	if len(hits) > searchPageSize {
		hits = hits[:searchPageSize]
		v.More = p.url("offset", strconv.Itoa(offset+searchPageSize))
	}
	mark := termMarker(terms)
	for _, h := range hits {
		v.Hits = append(v.Hits, hitFor(h, p.Q, mark))
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, r, http.StatusOK, searchHits(v))
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

// hitFor is the view of one hit. A Message hit links to its anchor with the
// query to highlight; a title hit links to the top of the Transcript.
func hitFor(h store.Hit, q string, mark func(string) string) hitView {
	v := hitView{
		Href:    sessionHref(h.SessionID) + "?" + url.Values{"hl": {q}}.Encode(),
		Title:   templ.Raw(mark(titleOr(h.Title, h.NativeID))),
		Source:  h.Source,
		Machine: h.Machine,
		Role:    h.Role,
	}
	v.Project, v.ProjectFull = projectName(h.ProjectCwd)
	if h.MessageID != "" {
		v.Href += "#m-" + h.MessageID
		v.Snippet = templ.Raw(markSnippet(h.Snippet))
	} else {
		v.Role = "title"
	}
	if h.ParentID != 0 {
		v.ChildOf = "↳ child of " + h.ParentTitle
	}
	return v
}

// facets builds the Machine, Source and Project facet groups. Each count
// links to the search with that chip added.
func (s *server) facets(ctx context.Context, p feedParams, terms []string, f store.FeedFilter) ([]facetGroup, error) {
	fs, err := s.store.SearchFacets(ctx, terms, f)
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
