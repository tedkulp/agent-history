package web

import (
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
		"· user", "· assistant",
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

	// A title hit links to the top of the Transcript.
	_, body = get(t, srv.URL+"/?q=hunt")
	if !strings.Contains(body, `href="/sessions/`+id+`?hl=hunt"`) || !strings.Contains(body, "· title") {
		t.Error("title hit doesn't link to the top")
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
		`href="/?machine=m1&amp;q=otter" title="m1"><span>laptop</span> <span class="n">4</span>`,
		`href="/?machine=m2&amp;q=otter" title="m2"><span>desk</span> <span class="n">2</span>`,
		`href="/?q=otter&amp;source=claude-code"`,
		`href="/?machine=m1&amp;project=-&amp;q=otter"`,
		`href="/?machine=m1&amp;project=%2FUsers%2Fted%2Fsrc%2Fapp&amp;q=otter"`,
		`<span>laptop › app</span> <span class="n">2</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("facets lack %q", want)
		}
	}
	if n := strings.Count(body, `class="hit"`); n != 6 {
		t.Errorf("hits = %d, want 6", n)
	}

	_, body = get(t, srv.URL+"/?q=otter&machine=m1&project=-")
	if !strings.Contains(body, "</mark> two") || strings.Contains(body, "</mark> one") || strings.Contains(body, "</mark> three") {
		t.Error("Project facet does not filter")
	}
	if n := strings.Count(body, `class="hit"`); n != 2 {
		t.Errorf("filtered hits = %d, want 2", n)
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
	if n := strings.Count(body, `class="hit"`); n != searchPageSize {
		t.Fatalf("first page = %d hits", n)
	}
	if !strings.Contains(body, `hx-get="/?offset=50&amp;q=badger"`) {
		t.Fatal("no Load more link")
	}
	resp, next := getHX(t, srv.URL+"/?offset=50&q=badger")
	if resp != 200 || strings.Count(next, `class="hit"`) != 10 || strings.Contains(next, "<html") || strings.Contains(next, "Load more") {
		t.Errorf("second page: status %d, %d hits", resp, strings.Count(next, `class="hit"`))
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
