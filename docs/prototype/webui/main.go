// PROTOTYPE — throwaway. Answers "Web UI: browse and search screens" (#13).
//
// Three structurally different variants of the whole Hub UI on one route,
// switchable via ?v=A|B|C and the floating bottom bar (← / → keys too).
//
// Uses stdlib html/template instead of templ to skip the codegen step; the
// shape (server-rendered pages + htmx for lazy tool output) matches the real
// stack decided in "Hub language and web UI stack" (#7).
//
// Run: go -C prototype/webui run .   then open http://127.0.0.1:8791 (PORT overrides)
package main

import (
	"cmp"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const bigOutput = 4096 // collapse + lazy-load threshold from #7

var variants = []struct{ Key, Name string }{
	{"A", "Drill-down pages"},
	{"B", "Three-pane reader"},
	{"C", "Search-first feed"},
}

type Page struct {
	V, VName                 string
	Q, HL                    string
	Machine, Project, Source string
	Session                  *Session
}

func (p Page) F() Filter { return Filter{p.Machine, p.Project, p.Source} }

func main() {
	tpl := template.Must(template.New("").Funcs(funcs).Parse(sharedTpl + variantA + variantB + variantC))

	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := Page{V: q.Get("v"), Q: q.Get("q"), HL: q.Get("hl"), Machine: q.Get("machine"), Project: q.Get("project"), Source: q.Get("source")}
		if p.V == "" {
			p.V = "A"
		}
		for _, v := range variants {
			if v.Key == p.V {
				p.VName = v.Name
			}
		}
		p.Session = sessByID[q.Get("session")]
		if err := tpl.ExecuteTemplate(w, p.V, p); err != nil {
			log.Println(err)
			fmt.Fprint(w, err)
		}
	})
	// htmx target: full tool output, fetched on expand.
	http.HandleFunc("GET /tool/{id}", func(w http.ResponseWriter, r *http.Request) {
		p := partByID[r.PathValue("id")]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		time.Sleep(150 * time.Millisecond) // make the lazy load visible
		fmt.Fprintf(w, `<pre class="out">%s</pre>`, html.EscapeString(p.ToolOutput))
	})
	addr := "127.0.0.1:" + cmp.Or(os.Getenv("PORT"), "8791")
	log.Printf("prototype on http://%s  (?v=A|B|C)", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// --- template helpers ------------------------------------------------------

var (
	reBold   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reInline = regexp.MustCompile("`([^`]+)`")
)

// md is a toy markdown renderer (the real Hub uses goldmark + chroma).
func md(s string) template.HTML {
	var out strings.Builder
	segs := strings.Split(s, "```")
	for i, seg := range segs {
		if i%2 == 1 {
			seg = strings.TrimPrefix(seg, "\n")
			if nl := strings.Index(seg, "\n"); nl >= 0 && !strings.Contains(seg[:nl], " ") && nl < 12 {
				seg = seg[nl+1:] // drop language tag
			}
			out.WriteString(`<pre class="code">` + html.EscapeString(strings.TrimRight(seg, "\n")) + `</pre>`)
			continue
		}
		for _, para := range strings.Split(strings.TrimSpace(seg), "\n\n") {
			if para == "" {
				continue
			}
			e := html.EscapeString(para)
			e = reBold.ReplaceAllString(e, "<strong>$1</strong>")
			e = reInline.ReplaceAllString(e, "<code>$1</code>")
			e = strings.ReplaceAll(e, "\n", "<br>")
			out.WriteString("<p>" + e + "</p>")
		}
	}
	return template.HTML(out.String())
}

// hl escapes s and wraps case-insensitive matches of q in <mark>.
func hl(s, q string) template.HTML {
	if q == "" {
		return template.HTML(html.EscapeString(s))
	}
	ls, lq := strings.ToLower(s), strings.ToLower(q)
	var b strings.Builder
	for {
		i := strings.Index(ls, lq)
		if i < 0 {
			b.WriteString(html.EscapeString(s))
			break
		}
		b.WriteString(html.EscapeString(s[:i]) + "<mark>" + html.EscapeString(s[i:i+len(q)]) + "</mark>")
		s, ls = s[i+len(q):], ls[i+len(q):]
	}
	return template.HTML(b.String())
}

// mdhl renders markdown, then marks the search term in text (not inside tags).
func mdhl(s, q string) template.HTML {
	h := string(md(s))
	if q == "" {
		return template.HTML(h)
	}
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(html.EscapeString(q)))
	parts := regexp.MustCompile(`<[^>]+>`).Split(h, -1)
	tags := regexp.MustCompile(`<[^>]+>`).FindAllString(h, -1)
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(re.ReplaceAllString(p, "<mark>$0</mark>"))
		if i < len(tags) {
			b.WriteString(tags[i])
		}
	}
	return template.HTML(b.String())
}

func snippet(s, q string) template.HTML {
	s = strings.Join(strings.Fields(s), " ")
	i := strings.Index(strings.ToLower(s), strings.ToLower(q))
	if i < 0 {
		i = 0
	}
	start, end := max(0, i-70), min(len(s), i+len(q)+110)
	pre, post := "", ""
	if start > 0 {
		pre = "…"
	}
	if end < len(s) {
		post = "…"
	}
	return template.HTML(pre) + hl(s[start:end], q) + template.HTML(post)
}

func diff(p *Part) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="diff"><div class="diff-file">` + html.EscapeString(p.DiffFile))
	if p.DiffOld == "" {
		b.WriteString(` <span class="new-file">new file</span>`)
	}
	b.WriteString(`</div><pre>`)
	if p.DiffOld != "" {
		for _, l := range strings.Split(p.DiffOld, "\n") {
			b.WriteString(`<span class="del">- ` + html.EscapeString(l) + "</span>\n")
		}
	}
	for _, l := range strings.Split(p.DiffNew, "\n") {
		b.WriteString(`<span class="add">+ ` + html.EscapeString(l) + "</span>\n")
	}
	b.WriteString(`</pre></div>`)
	return template.HTML(b.String())
}

// u builds a link on the single prototype route: u "B" "session" "s1" ...
func u(v string, kv ...string) string {
	q := url.Values{"v": {v}}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	return "/?" + q.Encode()
}

func ago(t time.Time) string {
	d := baseTime.Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func day(t time.Time) string {
	switch baseTime.YearDay() - t.YearDay() {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	return t.Format("Mon Jan 2")
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func headLines(s string, n int) (string, int) {
	ls := strings.Split(s, "\n")
	if len(ls) <= n {
		return s, 0
	}
	return strings.Join(ls[:n], "\n"), len(ls) - n
}

type Chunk struct {
	Part  *Part
	Tools []*Part
}

// chunks groups consecutive tool_call Parts (variant C clusters them).
func chunks(ps []*Part) []Chunk {
	var out []Chunk
	for _, p := range ps {
		if p.Kind == "tool_call" {
			if n := len(out); n > 0 && out[n-1].Tools != nil {
				out[n-1].Tools = append(out[n-1].Tools, p)
				continue
			}
			out = append(out, Chunk{Tools: []*Part{p}})
			continue
		}
		out = append(out, Chunk{Part: p})
	}
	return out
}

type HitGroup struct {
	Session *Session
	Hits    []Hit
}

func groupHits(hs []Hit) []HitGroup {
	var out []HitGroup
	idx := map[string]int{}
	for _, h := range hs {
		i, ok := idx[h.Session.ID]
		if !ok {
			i = len(out)
			idx[h.Session.ID] = i
			out = append(out, HitGroup{Session: h.Session})
		}
		out[i].Hits = append(out[i].Hits, h)
	}
	return out
}

type Facet struct {
	Key, Label string
	N          int
}

func facets(hs []Hit, kind string) []Facet {
	var out []Facet
	idx := map[string]int{}
	for _, h := range hs {
		var k, l string
		switch kind {
		case "machine":
			k, l = h.Session.MachineID, machByID[h.Session.MachineID].Name
		case "source":
			k, l = h.Session.Source, h.Session.Source
		case "project":
			k, l = projKey(h.Session), Project{Cwd: h.Session.ProjectCwd}.Name()
		}
		if i, ok := idx[k]; ok {
			out[i].N++
			continue
		}
		idx[k] = len(out)
		out = append(out, Facet{k, l, 1})
	}
	return out
}

func toolCount(s *Session) int {
	n := 0
	for _, m := range s.Messages {
		for _, p := range m.Parts {
			if p.Kind == "tool_call" {
				n++
			}
		}
	}
	return n
}

var funcs = template.FuncMap{
	"md": md, "hl": hl, "mdhl": mdhl, "snippet": snippet, "diff": diff, "u": u,
	"ago": ago, "day": day, "firstLine": firstLine, "chunks": chunks,
	"groupHits": groupHits, "facets": facets, "toolCount": toolCount,
	"clock":    func(t time.Time) string { return t.Format("15:04") },
	"stamp":    func(t time.Time) string { return t.Format("Mon Jan 2, 15:04") },
	"machines": func() []*Machine { return machines },
	"mach":     func(id string) *Machine { return machByID[id] },
	"sess":     func(id string) *Session { return sessByID[id] },
	"projects": projectsFor,
	"top":      topSessions,
	"filter":   func(m string) Filter { return Filter{Machine: m} },
	"search":   search,
	"projName": func(s *Session) string { return Project{Cwd: s.ProjectCwd}.Name() },
	"projKey":  projKey,
	"projLabel": func(k string) string {
		if k == "-" {
			return "No project"
		}
		return Project{Cwd: k}.Name()
	},
	"big":  func(p *Part) bool { return len(p.ToolOutput) > bigOutput },
	"size": func(p *Part) string { return fmt.Sprintf("%.1f KB", float64(len(p.ToolOutput))/1024) },
	"head": func(s string, n int) string { h, _ := headLines(s, n); return h },
	"more": func(s string, n int) int { _, m := headLines(s, n); return m },
	"children": func(s *Session) []*Session {
		var out []*Session
		for _, c := range sessions {
			if c.ParentID == s.ID {
				out = append(out, c)
			}
		}
		return out
	},
	"variants": func() any { return variants },
	"sources":  func() []string { return []string{"claude-code", "codex", "oh-my-pi", "opencode"} },
	"first": func(s *Session) string {
		for _, m := range s.Messages {
			for _, p := range m.Parts {
				if m.Role == "user" && p.Kind == "text" {
					return p.Text
				}
			}
		}
		return ""
	},
	"sameDay": func(a, b time.Time) bool { return a.YearDay() == b.YearDay() },
	"prev": func(ss []*Session, i int) *Session {
		if i == 0 {
			return nil
		}
		return ss[i-1]
	},
}
