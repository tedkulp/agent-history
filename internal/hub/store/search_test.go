package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/protocol"
)

// searchSession is a Claude Code Session: a user prompt, an assistant reply
// with thinking, a Bash call with its output, and a closing reply.
func searchSession(prompt, reply, thinking, command, output string) string {
	return fmt.Sprintf(`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":%q}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-01T10:00:01.000Z","message":{"id":"msg_1","model":"m","content":[{"type":"thinking","thinking":%q},{"type":"text","text":%q}]}}
{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2026-09-01T10:00:02.000Z","message":{"id":"msg_2","model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":%q}}]}}
{"type":"user","uuid":"r1","parentUuid":"a2","timestamp":"2026-09-01T10:00:03.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":%q}]}}
`, prompt, thinking, reply, command, output)
}

func seedSearch(t *testing.T, sessions ...string) *Store {
	t.Helper()
	s, _ := openParsing(t)
	for i, body := range sessions {
		appendTo(t, s, protocol.SourceClaudeCode, claudeKey(uuid(i+1)), []byte(body))
	}
	for parseNext(t, s) {
	}
	return s
}

func search(t *testing.T, s *Store, q string, f FeedFilter) []Hit {
	t.Helper()
	hits, err := s.Search(context.Background(), ParseQuery(q), SearchFilter{FeedFilter: f}, 50)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return hits
}

func TestSearchIndexesTextAndToolInputOnly(t *testing.T) {
	s := seedSearch(t, searchSession("find the zebra", "a giraffe appears", "secret pondering", "ls /walrus", "output from penguin"))
	for q, want := range map[string]string{"zebra": "u1", "giraffe": "a1", "walrus": "a2", "Bash": "a2"} {
		hits := search(t, s, q, FeedFilter{})
		var got []string
		for _, h := range hits {
			got = append(got, h.MessageID)
		}
		// The title is the first prompt, so "zebra" also hits the title row.
		if !strings.Contains(strings.Join(got, ","), want) {
			t.Errorf("%q hits %v, want %s", q, got, want)
		}
	}
	for _, q := range []string{"penguin", "pondering"} {
		if hits := search(t, s, q, FeedFilter{}); len(hits) != 0 {
			t.Errorf("%q hits %+v, want none", q, hits)
		}
	}
	// A prefix of the last term matches; the earlier ones must be whole.
	if hits := search(t, s, "giraffe appe", FeedFilter{}); len(hits) != 1 || hits[0].Role != "assistant" {
		t.Errorf("prefix hits %+v", hits)
	}
	if hits := search(t, s, "gira appears", FeedFilter{}); len(hits) != 0 {
		t.Errorf("non-last prefix hits %+v", hits)
	}
}

func TestSearchRowsAreRebuiltOnReparse(t *testing.T) {
	s, c := openParsing(t)
	key := claudeKey(uuid(1))
	appendTo(t, s, protocol.SourceClaudeCode, key, []byte(searchSession("first", "reply", "", "x", "")))
	parseNext(t, s)
	c.advance(parseInterval)
	appendTo(t, s, protocol.SourceClaudeCode, key, []byte(`{"type":"user","uuid":"u2","parentUuid":"r1","timestamp":"2026-09-01T10:01:00.000Z","message":{"role":"user","content":"second"}}`+"\n"))
	parseNext(t, s)
	var n int
	s.read.QueryRow(`SELECT count(*) FROM search`).Scan(&n)
	// title, u1, a1, a2 and u2: the tool_result carries no text of its own.
	if n != 5 {
		t.Errorf("search rows = %d, want 5", n)
	}
	if hits := search(t, s, "second", FeedFilter{}); len(hits) != 1 || hits[0].MessageID != "u2" {
		t.Errorf("hits = %+v", hits)
	}
}

func TestTitleHitRanksAboveBodyHit(t *testing.T) {
	body := searchSession("tell me about otters please, and more words so this row is long", "otters are nice", "", "x", "")
	titled := searchSession("unrelated start", "ok", "", "y", "") + `{"type":"custom-title","customTitle":"Otters"}` + "\n"
	s := seedSearch(t, body, titled)
	hits := search(t, s, "otters", FeedFilter{})
	if len(hits) < 2 || hits[0].MessageID != "" || hits[0].Title != "Otters" {
		t.Fatalf("first hit = %+v, want the title hit", hits)
	}
}

func TestParseQuery(t *testing.T) {
	for in, want := range map[string][]string{
		"":                         nil,
		"   ":                      nil,
		"foo bar":                  {"foo", "bar"},
		`"exact phrase" x`:         {"exact phrase", "x"},
		`"unterminated phrase`:     {"unterminated phrase"},
		`a*b (c) d:e ^f`:           {"a b", "c", "d e", "f"},
		`AND OR NOT`:               {"AND", "OR", "NOT"},
		`* " ( ) - +`:              nil,
		`foo-bar`:                  {"foo bar"},
		`héllo "wörld"`:            {"héllo", "wörld"},
		`""`:                       nil,
		"tab\tsep\nnew":            {"tab", "sep", "new"},
		`"a"b`:                     {"a", "b"},
		`{x} NEAR(y z)`:            {"x", "NEAR y", "z"},
		`"quote "" inside"`:        {"quote", "inside"},
		`col:"x" -y +z`:            {"col", "x", "y", "z"},
		`"* ( )"`:                  nil,
		`a"b"c`:                    {"a", "b", "c"},
		`😀 emoji`:                  {"emoji"},
		`   leading and trailing `: {"leading", "and", "trailing"},
	} {
		if got := ParseQuery(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchNeverErrorsOnFTSSyntax(t *testing.T) {
	s := seedSearch(t, searchSession("hello world", "reply", "", "x", ""))
	for _, q := range []string{
		`"`, `""`, `"hello`, `*`, `hello*`, `*hello`, `AND`, `hello AND`, `AND hello`, `(`, `(hello`, `hello)`,
		`NEAR(hello world)`, `hello OR world`, `NOT hello`, `-hello`, `+hello`, `^hello`, `title:hello`, `{body}: hello`,
		`hello"world`, `'hello'`, `hello\`, `"hello" "`, `a:b:c`, `.`, `...hello`,
	} {
		if _, err := s.Search(context.Background(), ParseQuery(q), SearchFilter{}, 50); err != nil {
			t.Errorf("search %q: %v", q, err)
		}
		if _, err := s.SearchFacets(context.Background(), ParseQuery(q), FeedFilter{}); err != nil {
			t.Errorf("facets %q: %v", q, err)
		}
	}
}

func TestSnippetsEscapeTranscriptHTML(t *testing.T) {
	s := seedSearch(t, searchSession(`look <img src=x onerror=alert(1)> kiwi & "fruit"`, "ok", "", "x", ""))
	hits := search(t, s, "kiwi", FeedFilter{})
	var msg *Hit
	for i := range hits {
		if hits[i].MessageID == "u1" {
			msg = &hits[i]
		}
	}
	if msg == nil {
		t.Fatalf("hits = %+v", hits)
	}
	want := `look &lt;img src=x onerror=alert(1)&gt; <mark>kiwi</mark> &amp; &#34;fruit&#34;`
	if msg.Snippet != want {
		t.Errorf("snippet = %s\nwant      %s", msg.Snippet, want)
	}
}

func TestSearchChildSessionHitsNameTheirParent(t *testing.T) {
	s, _ := openParsing(t)
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1+line2))
	child := `{"type":"user","uuid":"c1","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"child walrus task"}}` + "\n"
	appendTo(t, s, protocol.SourceClaudeCode, childKey, []byte(child))
	for parseNext(t, s) {
	}
	hits := search(t, s, "walrus", FeedFilter{})
	if len(hits) == 0 {
		t.Fatal("no hits in the Child Session")
	}
	for _, h := range hits {
		if h.ParentID != sessionIDOf(t, s, sessUUID) || h.ParentTitle != "hello" {
			t.Errorf("hit = %+v", h)
		}
	}
}

func TestSearchFacetsMatchHitsAndFilter(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	if err := s.UpsertMachine(ctx, "m2", protocol.MachineInfo{Hostname: "desk", HomeDir: "/home/ted"}); err != nil {
		t.Fatal(err)
	}
	add := func(machine, cwd, native, prompt string) {
		body := strings.ReplaceAll(searchSession(prompt, "ok", "", "x", ""), "/Users/ted/src/app", cwd)
		prev, _ := s.CurrentContent(ctx, machine, protocol.SourceClaudeCode, claudeKey(native))
		if _, err := s.Append(ctx, AppendRequest{
			MachineID: machine, Source: protocol.SourceClaudeCode, RecordKey: claudeKey(native),
			PrefixSha256: hexSum(prev), Data: []byte(body), Compressed: zstdBytes(t, []byte(body)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("m1", "/Users/ted/src/app", uuid(1), "llama one")
	add("m1", "/Users/ted/src/app", uuid(2), "llama two")
	add("m1", "/Users/ted", uuid(3), "llama three")
	add("m2", "/home/ted/src/app", uuid(4), "llama four")
	add("m2", "/home/ted/src/app", uuid(5), "no match")
	for parseNext(t, s) {
	}

	terms := ParseQuery("llama")
	fs, err := s.SearchFacets(ctx, terms, FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	all := search(t, s, "llama", FeedFilter{})
	// Each Session hits twice: its first prompt and its title.
	if len(all) != 8 {
		t.Fatalf("hits = %d, want 8", len(all))
	}
	type fc struct {
		machine, value string
		hits           int
	}
	flat := func(fs []Facet) []fc {
		var out []fc
		for _, f := range fs {
			out = append(out, fc{f.MachineID, f.Value, f.Hits})
		}
		return out
	}
	if got, want := flat(fs.Machines), []fc{{"m1", "m1", 6}, {"m2", "m2", 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("machines = %v, want %v", got, want)
	}
	if got, want := flat(fs.Sources), []fc{{"", "claude-code", 8}}; !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
	if got, want := flat(fs.Projects), []fc{{"m1", "/Users/ted/src/app", 4}, {"m2", "/home/ted/src/app", 2}, {"m1", "", 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("projects = %v, want %v", got, want)
	}

	// Each facet's count is the number of hits its filter leaves.
	for _, f := range fs.Projects {
		cwd := f.Value
		if n := len(search(t, s, "llama", FeedFilter{Machine: f.MachineID, Project: &cwd})); n != f.Hits {
			t.Errorf("project %s/%q: %d hits, facet says %d", f.MachineID, cwd, n, f.Hits)
		}
	}
	if n := len(search(t, s, "llama", FeedFilter{Machine: "m2"})); n != 2 {
		t.Errorf("m2 hits = %d", n)
	}
}

func TestSearchPages(t *testing.T) {
	var sessions []string
	for i := 0; i < 7; i++ {
		sessions = append(sessions, searchSession(fmt.Sprint("heron ", i), "ok", "", "x", ""))
	}
	s := seedSearch(t, sessions...)
	terms := ParseQuery("heron")
	seen := map[string]bool{}
	for off := 0; off < 14; off += 5 {
		hits, err := s.Search(context.Background(), terms, SearchFilter{Offset: off}, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			k := fmt.Sprint(h.SessionID, "/", h.MessageID)
			if seen[k] {
				t.Errorf("hit %s repeated", k)
			}
			seen[k] = true
		}
	}
	if len(seen) != 14 {
		t.Errorf("paged hits = %d, want 14", len(seen))
	}
}
