package web

import (
	"fmt"
	"io"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSearchResultsLinkToTheMessage(t *testing.T) {
	main := `{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"where is the <b>kiwi</b> & co"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-01T10:00:01.000Z","message":{"id":"msg_1","model":"m","content":[{"type":"text","text":"The KIWI is here."}]}}
{"type":"custom-title","customTitle":"Fruit <i>hunt</i>"}
`
	child := `{"type":"user","uuid":"c1","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"child kiwi task"}}
`
	srv := newSite(t, map[string]string{
		"-Users-ted-src-app/" + sess + ".jsonl":                   main,
		"-Users-ted-src-app/" + sess + "/subagents/agent-a.jsonl": child,
	})

	code, body := get(t, srv.URL+"/?q=kiwi")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	_, home := get(t, srv.URL+"/")
	id := sessionID(t, home, "Fruit")
	for _, want := range []string{
		`value="kiwi"`,
		`href="/sessions/` + id + `?hl=kiwi#m-u1"`,
		`href="/sessions/` + id + `?hl=kiwi#m-a1"`,
		// Transcript HTML is escaped in the snippet and the title.
		`where is the &lt;b&gt;<mark>kiwi</mark>&lt;/b&gt; &amp; co`,
		`The <mark>KIWI</mark> is here.`,
		`Fruit &lt;i&gt;hunt&lt;/i&gt;`,
		"↳ child of Fruit &lt;i&gt;hunt&lt;/i&gt;",
		"<mark>kiwi</mark> task",
		`<span class="role">user</span>`, `<span class="role">assistant</span>`,
		"2 hits",
		`class="facets"`, "<h4>Machine</h4>", "<h4>Source</h4>", "<h4>Project</h4>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("results lack %q", want)
		}
	}
	for _, bad := range []string{"<b>kiwi", "<i>hunt"} {
		if strings.Contains(body, bad) {
			t.Errorf("results render Transcript HTML %q", bad)
		}
	}

	// A title hit shows only in its group's header, which links to the top of
	// the Transcript.
	_, body = get(t, srv.URL+"/?q=hunt")
	if !strings.Contains(body, `href="/sessions/`+id+`?hl=hunt"`) || !strings.Contains(body, "Fruit &lt;i&gt;<mark>hunt</mark>&lt;/i&gt;") {
		t.Error("title hit doesn't link to the top")
	}
	if strings.Count(body, `class="group"`) != 1 || strings.Contains(body, `class="hit"`) || !strings.Contains(body, "1 hit<") {
		t.Error("title-only Hit shows Hit rows")
	}

	// The Transcript page loads the script that marks hl terms.
	_, body = get(t, srv.URL+"/sessions/"+id+"?hl=kiwi")
	if !strings.Contains(body, `src="/static/app.js"`) || !strings.Contains(body, `id="m-u1"`) {
		t.Error("Transcript lacks the highlight script or anchors")
	}
	if code, js := get(t, srv.URL+"/static/app.js"); code != 200 || !strings.Contains(js, "createTreeWalker") {
		t.Errorf("app.js: status %d", code)
	}
}

func TestSearchInputThatIsNotASearchShowsTheFeed(t *testing.T) {
	srv := newSite(t, map[string]string{"-Users-ted-src-app/" + sess + ".jsonl": sessionLine("/Users/ted/src/app", "hello there", 1)})
	for _, q := range []string{"", "   ", `"`, "*", "( )", `"" * -`} {
		code, body := get(t, srv.URL+"/?q="+url.QueryEscape(q))
		if code != 200 || !strings.Contains(body, `class="row"`) || strings.Contains(body, `class="results"`) {
			t.Errorf("q=%q: status %d, feed not shown", q, code)
		}
	}
	for _, q := range []string{`"hello`, "hello AND", "(hello", "hel*", "NEAR(hello there)", "title:hello", `hello"`, "-hello +there ^x"} {
		code, body := get(t, srv.URL+"/?q="+url.QueryEscape(q))
		if code != 200 || !strings.Contains(body, `class="results"`) {
			t.Errorf("q=%q: status %d, no results page", q, code)
		}
	}
}

func TestSearchFacetsAddChips(t *testing.T) {
	srv := newSiteRecords(t, []record{
		{"m1", "-Users-ted-src-app/" + uuid(1) + ".jsonl", sessionLine("/Users/ted/src/app", "otter one", 1)},
		{"m1", "-Users-ted/" + uuid(2) + ".jsonl", sessionLine("/Users/ted", "otter two", 2)},
		{"m2", "-home-ted-src-app/" + uuid(3) + ".jsonl", sessionLine("/home/ted/src/app", "otter three", 3)},
	})
	_, body := get(t, srv.URL+"/?q=otter")
	// Each Session hits on its prompt and on its title, which is the prompt.
	for _, want := range []string{
		`href="/?machine=m1&amp;q=otter" title="m1"><span>laptop</span> <span class="n">2</span>`,
		`href="/?machine=m2&amp;q=otter" title="m2"><span>desk</span> <span class="n">1</span>`,
		`href="/?q=otter&amp;source=claude-code"`,
		`href="/?machine=m1&amp;project=-&amp;q=otter"`,
		`href="/?machine=m1&amp;project=%2FUsers%2Fted%2Fsrc%2Fapp&amp;q=otter"`,
		`<span>laptop › app</span> <span class="n">1</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("facets lack %q", want)
		}
	}
	if n := strings.Count(body, `class="group"`); n != 3 {
		t.Errorf("groups = %d, want 3", n)
	}

	_, body = get(t, srv.URL+"/?q=otter&machine=m1&project=-")
	if !strings.Contains(body, "</mark> two") || strings.Contains(body, "</mark> one") || strings.Contains(body, "</mark> three") {
		t.Error("Project facet does not filter")
	}
	if n := strings.Count(body, `class="group"`); n != 1 {
		t.Errorf("filtered groups = %d, want 1", n)
	}
	// The active chips stay in the search box's form.
	if !strings.Contains(body, `name="machine" value="m1"`) || !strings.Contains(body, `name="project" value="-"`) {
		t.Error("search form drops the active chips")
	}
	// The clear button drops the query and keeps the chips.
	if !strings.Contains(body, `class="clear" href="/?machine=m1&amp;project=-"`) {
		t.Error("no clear button back to the filtered feed")
	}
	if _, home := get(t, srv.URL+"/"); strings.Contains(home, `class="clear"`) {
		t.Error("clear button shown with no query")
	}
	// Chips keep the query.
	if !strings.Contains(body, `class="chip on" href="/?q=otter"`) {
		t.Error("Machine chip toggles off without keeping q")
	}
}

func TestSearchLoadMore(t *testing.T) {
	var recs []record
	for i := 1; i <= 30; i++ {
		recs = append(recs, record{"m1", "-Users-ted-src-app/" + uuid(i) + ".jsonl", sessionLine("/Users/ted/src/app", "badger", i)})
	}
	srv := newSiteRecords(t, recs)
	_, body := get(t, srv.URL+"/?q=badger")
	if n := strings.Count(body, `class="group"`); n != searchPageSize {
		t.Fatalf("first page = %d Sessions", n)
	}
	if !strings.Contains(body, `hx-get="/?offset=20&amp;q=badger"`) {
		t.Fatal("no Load more link")
	}
	resp, next := getHX(t, srv.URL+"/?offset=20&q=badger")
	if resp != 200 || strings.Count(next, `class="group"`) != 10 || strings.Contains(next, "<html") || strings.Contains(next, "Load more") {
		t.Errorf("second page: status %d, %d Sessions", resp, strings.Count(next, `class="group"`))
	}
	ids := map[string]bool{}
	for _, page := range []string{body, next} {
		for _, part := range strings.Split(page, `class="group" id="g-`)[1:] {
			id := part[:strings.IndexByte(part, '"')]
			if ids[id] {
				t.Errorf("Session %s repeated", id)
			}
			ids[id] = true
		}
	}
}

func TestSearchGroupExpandsInTranscriptOrder(t *testing.T) {
	var b strings.Builder
	parent := "null"
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("p%02d", i)
		fmt.Fprintf(&b, `{"type":"user","uuid":%q,"parentUuid":%s,"timestamp":"2026-09-01T10:%02d:00.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"marmot %s"}}`+"\n",
			id, parent, i, strings.Repeat("filler ", (i*7)%10))
		parent = fmt.Sprintf("%q", id)
	}
	b.WriteString(`{"type":"custom-title","customTitle":"Burrows"}` + "\n")
	srv := newSite(t, map[string]string{"-Users-ted-src-app/" + sess + ".jsonl": b.String()})
	_, home := get(t, srv.URL+"/")
	id := sessionID(t, home, "Burrows")

	_, body := get(t, srv.URL+"/?q=marmot")
	if strings.Count(body, `class="group"`) != 1 || !strings.Contains(body, "25 hits") || strings.Count(body, `class="hit"`) != 3 {
		t.Fatal("25 Hits in one Session don't make one group of three rows")
	}
	more := `hx-get="/sessions/` + id + `/hits?offset=0&amp;q=marmot"`
	if !strings.Contains(body, more) || !strings.Contains(body, "+22 more") || !strings.Contains(body, `href="/sessions/`+id+`?hl=marmot">Open Transcript`) {
		t.Fatalf("no +22 more control in %s", body)
	}
	shown := map[string]bool{}
	for _, part := range strings.Split(body, `?hl=marmot#m-`)[1:] {
		shown[part[:3]] = true
	}

	var order []string
	code, next := getHX(t, srv.URL+"/sessions/"+id+"/hits?offset=0&q=marmot")
	if code != 200 || strings.Count(next, `class="hit"`) != 20 || !strings.Contains(next, "+2 more") ||
		!strings.Contains(next, `hx-get="/sessions/`+id+`/hits?offset=20&amp;q=marmot"`) {
		t.Fatalf("first expansion: status %d: %s", code, next)
	}
	collect := func(page string) {
		for _, part := range strings.Split(page, `?hl=marmot#m-`)[1:] {
			order = append(order, part[:3])
		}
	}
	collect(next)
	_, last := getHX(t, srv.URL+"/sessions/"+id+"/hits?offset=20&q=marmot")
	if strings.Count(last, `class="hit"`) != 2 || strings.Contains(last, "more<") || !strings.Contains(last, "Open Transcript") {
		t.Errorf("last expansion: %s", last)
	}
	collect(last)
	if len(order) != 22 {
		t.Fatalf("expanded %d Hits, want 22", len(order))
	}
	for i, m := range order {
		if shown[m] {
			t.Errorf("expansion repeats top Hit %s", m)
		}
		if i > 0 && order[i-1] >= m {
			t.Errorf("expansion out of Transcript order: %v", order)
			break
		}
	}
	if code, _ := get(t, srv.URL+"/sessions/999/hits?q=marmot"); code != 404 {
		t.Errorf("unknown Session: status %d", code)
	}
}

// getHX gets url as htmx does.
func getHX(t *testing.T, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMarkSnippetEscapesThenMarks(t *testing.T) {
	in := `<img src=x onerror=alert(1)> ` + store.SnippetOpen + `kiwi` + store.SnippetClose + ` & "fruit"`
	want := `&lt;img src=x onerror=alert(1)&gt; <mark>kiwi</mark> &amp; &#34;fruit&#34;`
	if got := markSnippet(in); got != want {
		t.Errorf("markSnippet = %s, want %s", got, want)
	}
}

func TestTermMarkerMatchesLikeFTS(t *testing.T) {
	for _, c := range []struct{ q, in, want string }{
		{"cat", "Cat concatenate catalog", "<mark>Cat</mark> concatenate <mark>cat</mark>alog"},
		// "cat" must be a whole word; the last term "dog" is a prefix.
		{"cat dog", "catalog dogs dog, cat.", "catalog <mark>dog</mark>s <mark>dog</mark>, <mark>cat</mark>."},
		{"cat dog", "dog catalog", "<mark>dog</mark> catalog"},
		{`"big dog"`, "a big-dog and big  dogs", "a <mark>big-dog</mark> and <mark>big  dog</mark>s"},
		{"a<b", "x a<b y", "x <mark>a&lt;b</mark> y"},
		{"b", "<b>b</b>", "&lt;<mark>b</mark>&gt;<mark>b</mark>&lt;/<mark>b</mark>&gt;"},
	} {
		if got := termMarker(store.ParseQuery(c.q))(c.in); got != c.want {
			t.Errorf("q=%q on %q = %s, want %s", c.q, c.in, got, c.want)
		}
	}
}
