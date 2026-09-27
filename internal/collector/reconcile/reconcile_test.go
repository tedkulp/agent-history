package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/api"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	machine = "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33"
	sess    = "5f1c9a2e-8d1b-4f7a-9c2e-5b0d7e1a90e2"
	proj    = "-Users-ted-src-app"
)

var zstdDec, _ = zstd.NewReader(nil)

func TestDecide(t *testing.T) {
	L := []byte("a\nb\n")
	cases := []struct {
		name string
		hub  *protocol.ManifestRecord
		want action
	}{
		{"not on hub", nil, action{kind: doAppend, offset: 0}},
		{"identical", &protocol.ManifestRecord{Length: 4, Sha256: hexSum(L)}, action{kind: doNothing}},
		{"hub has prefix", &protocol.ManifestRecord{Length: 2, Sha256: hexSum(L[:2])}, action{kind: doAppend, offset: 2}},
		{"hub prefix diverged", &protocol.ManifestRecord{Length: 2, Sha256: hexSum([]byte("x\n"))}, action{kind: doReplace}},
		{"hub longer", &protocol.ManifestRecord{Length: 9, Sha256: hexSum([]byte("a\nb\nc\nd\ne"))}, action{kind: doReplace}},
		{"same length, different content", &protocol.ManifestRecord{Length: 4, Sha256: hexSum([]byte("a\nc\n"))}, action{kind: doReplace}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decide(tc.hub, L); got != tc.want {
				t.Fatalf("decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestContentCutsJSONLAtLastNewline(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) source.Record {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return source.Record{Key: "p/" + name, Path: p}
	}
	cases := []struct {
		rec  source.Record
		want string
	}{
		{write("a.jsonl", "{\"x\":1}\n{\"y\":"), "{\"x\":1}\n"},
		{write("b.jsonl", "{\"half\":"), ""},
		{write("c.jsonl", "done\n"), "done\n"},
		{write("d.txt", "not jsonl, no newline"), "not jsonl, no newline"},
	}
	for _, tc := range cases {
		got, err := content(tc.rec)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: content = %q, want %q", tc.rec.Key, got, tc.want)
		}
	}
}

// fixture is a Hub behind httptest plus a Claude Code root on disk.
type fixture struct {
	t       *testing.T
	store   *store.Store
	hub     *hubclient.Client
	root    string
	appends atomic.Int32

	mu    sync.Mutex
	posts []post // every records request, in order
}

// post is one records request as the Hub received it.
type post struct {
	mode   string
	offset int64
	body   []byte // decompressed
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, store: st, root: t.TempDir()}
	h := api.New(st, nil, api.Floor{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.appends.Add(1)
			compressed, _ := io.ReadAll(r.Body)
			body, err := zstdDec.DecodeAll(compressed, nil)
			if err != nil {
				t.Errorf("request body is not zstd: %v", err)
			}
			offset, _ := strconv.ParseInt(r.Header.Get(protocol.HeaderOffset), 10, 64)
			f.mu.Lock()
			f.posts = append(f.posts, post{mode: r.Header.Get(protocol.HeaderMode), offset: offset, body: body})
			f.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(compressed))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.hub, err = hubclient.New(srv.URL, machine, "0.0.0-dev", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(rel, body string) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) run() Result {
	f.t.Helper()
	recs, err := claudecode.Adapter{}.Layouts()[0].Discover(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	info := protocol.MachineInfo{DisplayName: "test", HomeDir: "/Users/ted"}
	res, err := Reconcile(context.Background(), f.hub, info, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) manifest() map[string]protocol.ManifestRecord {
	f.t.Helper()
	m, err := f.store.Manifest(context.Background(), machine, "")
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]protocol.ManifestRecord{}
	for _, r := range m {
		out[r.RecordKey] = r
	}
	return out
}

func (f *fixture) hubContent(key string) string {
	f.t.Helper()
	b, err := f.store.CurrentContent(context.Background(), machine, protocol.SourceClaudeCode, key)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestReconcileShipsClaudeCodeRoot(t *testing.T) {
	f := newFixture(t)
	files := map[string]string{
		proj + "/" + sess + ".jsonl":                          "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n{\"type\":\"us",
		proj + "/" + sess + "/subagents/agent-a1.jsonl":       "{\"type\":\"user\"}\n",
		proj + "/" + sess + "/subagents/agent-a1.meta.json":   "{\"agentType\":\"Explore\"}",
		proj + "/" + sess + "/tool-results/toolu_01.txt":      "big output without trailing newline",
		proj + "/" + sess + "/tool-results/p1/page-1.jpg":     "\xff\xd8\xff\xe0binary",
		proj + "/" + sess + "/custom-title.json":              "{\"title\":\"hi\"}\n",
		proj + "/" + sess + ".orphaned-1790000000-ab12.jsonl": "{}\n",
		proj + "/memory/MEMORY.md":                            "not history",
	}
	for k, v := range files {
		f.write(k, v)
	}

	res := f.run()
	if res.Failed != 0 || res.Replaced != 0 {
		t.Fatalf("result %+v", res)
	}

	m := f.manifest()
	if len(m) != len(files)-1 {
		t.Fatalf("manifest has %d records, want %d: %v", len(m), len(files)-1, m)
	}
	for key, body := range files {
		if key == proj+"/memory/MEMORY.md" {
			if _, ok := m[key]; ok {
				t.Errorf("memory file shipped")
			}
			continue
		}
		want := body
		if filepath.Ext(key) == ".jsonl" {
			want = body[:lastNewline(body)]
		}
		got, ok := m[key]
		if !ok {
			t.Errorf("%s missing from manifest", key)
			continue
		}
		if got.Length != int64(len(want)) || got.Sha256 != hexSum([]byte(want)) {
			t.Errorf("%s: manifest %+v, want length %d", key, got, len(want))
		}
		if c := f.hubContent(key); c != want {
			t.Errorf("%s: hub content %q, want %q", key, c, want)
		}
	}

	// A second run uploads nothing.
	before := f.appends.Load()
	res = f.run()
	if f.appends.Load() != before || res.Uploaded != 0 || res.Unchanged != len(files)-1 {
		t.Fatalf("second run: %d new uploads, result %+v", f.appends.Load()-before, res)
	}
}

func TestReconcileAppendsGrowthAndFinishedLine(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	f.write(key, "{\"n\":1}\n{\"n\":")
	f.run()
	if c := f.hubContent(key); c != "{\"n\":1}\n" {
		t.Fatalf("after first run %q", c)
	}

	f.write(key, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n")
	before := f.appends.Load()
	res := f.run()
	if res.Uploaded != 1 || f.appends.Load()-before != 1 {
		t.Fatalf("result %+v, uploads %d", res, f.appends.Load()-before)
	}
	if c := f.hubContent(key); c != "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n" {
		t.Fatalf("after growth %q", c)
	}
}

func TestReconcileReplacesRewrittenRecord(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	f.write(key, "{\"n\":1}\n{\"n\":2}\n")
	f.run()
	f.write(key, "{\"n\":9}\n")
	res := f.run()
	if res.Replaced != 1 || res.Uploaded != 0 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if c := f.hubContent(key); c != "{\"n\":9}\n" {
		t.Fatalf("hub content %q", c)
	}
	if m := f.manifest()[key]; m.Sha256 != hexSum([]byte("{\"n\":9}\n")) {
		t.Fatalf("manifest %+v", m)
	}
	if p := f.takePosts(); p[len(p)-1].mode != protocol.ModeReplace {
		t.Fatalf("last request was %s, want replace", p[len(p)-1].mode)
	}

	// Nothing more to do on the next run.
	if res := f.run(); res.Unchanged != 1 || res.Replaced != 0 {
		t.Fatalf("second run %+v", res)
	}
}

// jsonlLines builds JSONL content of at least n bytes from lines of about
// lineLen bytes each.
func jsonlLines(n, lineLen int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "{\"i\":%d,\"pad\":\"%s\"}\n", i, strings.Repeat("x", lineLen))
	}
	return b.String()
}

func (f *fixture) takePosts() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.posts
	f.posts = nil
	return p
}

func checkChunks(t *testing.T, posts []post, wantFirstMode string) {
	t.Helper()
	if len(posts) < 2 {
		t.Fatalf("%d requests, want several chunks", len(posts))
	}
	var off int64
	for i, p := range posts {
		wantMode := protocol.ModeAppend
		if i == 0 {
			wantMode = wantFirstMode
		}
		if p.mode != wantMode || p.offset != off {
			t.Fatalf("chunk %d: mode %s offset %d, want %s at %d", i, p.mode, p.offset, wantMode, off)
		}
		if len(p.body) > protocol.ChunkSize {
			t.Fatalf("chunk %d is %d bytes, over 8 MiB", i, len(p.body))
		}
		if p.body[len(p.body)-1] != '\n' {
			t.Fatalf("chunk %d does not end on a line boundary", i)
		}
		off += int64(len(p.body))
	}
}

func TestReconcileChunksLargeSession(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	body := jsonlLines(20<<20, 1000)
	f.write(key, body)
	if res := f.run(); res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	checkChunks(t, f.takePosts(), protocol.ModeAppend)
	if m := f.manifest()[key]; m.Length != int64(len(body)) || m.Sha256 != hexSum([]byte(body)) {
		t.Fatalf("manifest %+v, want length %d", m, len(body))
	}

	// Rewriting it sends one replace and then appends.
	body = jsonlLines(18<<20, 900)
	f.write(key, body)
	if res := f.run(); res.Replaced != 1 || res.Failed != 0 {
		t.Fatalf("replace result %+v", res)
	}
	checkChunks(t, f.takePosts(), protocol.ModeReplace)
	if m := f.manifest()[key]; m.Length != int64(len(body)) || m.Sha256 != hexSum([]byte(body)) {
		t.Fatalf("manifest after replace %+v, want length %d", m, len(body))
	}
}

func TestNextChunk(t *testing.T) {
	cases := []struct {
		name  string
		rest  string
		jsonl bool
		want  int
	}{
		{"fits", "a\nb\n", true, 4},
		{"ends on last line that fits", "aaa\nbbb\nccc\n", true, 8},
		{"long first line goes alone", "aaaaaaaaaaaa\nb\n", true, 13},
		{"small lines before a long one", "a\nbbbbbbbbbbbbbb\n", true, 2},
		{"non-JSONL splits at the size", "0123456789abcdef", false, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextChunk([]byte(tc.rest), tc.jsonl, 10); got != tc.want {
				t.Fatalf("nextChunk = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCutOversizedLine(t *testing.T) {
	cases := []struct {
		in, want string
		cut      bool
	}{
		{"a\nbb\n", "a\nbb\n", false},
		{"a\n0123456789\nb\n", "a\n", true},
		{"0123456789\n", "", true},
		{"012345678\n", "012345678\n", false},
	}
	for _, tc := range cases {
		got, cut := cutOversizedLine([]byte(tc.in), 10)
		if string(got) != tc.want || cut != tc.cut {
			t.Errorf("cutOversizedLine(%q) = %q, %v; want %q, %v", tc.in, got, cut, tc.want, tc.cut)
		}
	}
}

// scriptedHub answers records requests with canned errors, then passes
// through to a real Hub.
type scriptedHub struct {
	Hub
	errs  []error
	calls int
	acked *protocol.RecordState // when set, 200s return this state instead
}

func (s *scriptedHub) next() error {
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *scriptedHub) Append(ctx context.Context, source, key string, offset int64, prefix string, data []byte) (protocol.RecordState, error) {
	if err := s.next(); err != nil {
		return protocol.RecordState{}, err
	}
	st, err := s.Hub.Append(ctx, source, key, offset, prefix, data)
	if err == nil && s.acked != nil {
		st = *s.acked
	}
	return st, err
}

func (s *scriptedHub) Replace(ctx context.Context, source, key string, data []byte) (protocol.RecordState, error) {
	if err := s.next(); err != nil {
		return protocol.RecordState{}, err
	}
	return s.Hub.Replace(ctx, source, key, data)
}

func (f *fixture) runWith(hub Hub) Result {
	f.t.Helper()
	recs, err := claudecode.Adapter{}.Layouts()[0].Discover(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := Reconcile(context.Background(), hub, protocol.MachineInfo{HomeDir: "/Users/ted"}, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func status(code int) error {
	return &hubclient.StatusError{StatusCode: code, Body: protocol.Error{Error: "x"}}
}

func TestReconcileErrorActions(t *testing.T) {
	key := proj + "/" + sess + ".jsonl"
	cases := []struct {
		name      string
		errs      []error
		wantCalls int
		wantOK    bool
	}{
		{"415 is retried once", []error{status(415)}, 2, true},
		{"415 twice skips the record", []error{status(415), status(415)}, 2, false},
		{"400 skips the record", []error{status(400)}, 1, false},
		{"413 skips the record", []error{status(413)}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(key, "{\"n\":1}\n")
			hub := &scriptedHub{Hub: f.hub, errs: tc.errs}
			res := f.runWith(hub)
			if hub.calls != tc.wantCalls {
				t.Fatalf("%d calls, want %d", hub.calls, tc.wantCalls)
			}
			if ok := res.Uploaded == 1 && res.Failed == 0; ok != tc.wantOK {
				t.Fatalf("result %+v", res)
			}
		})
	}
}

func TestReconcileFlagsShaMismatchAfterAck(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	f.write(key, "{\"n\":1}\n")
	hub := &scriptedHub{Hub: f.hub, acked: &protocol.RecordState{Length: 8, Sha256: hexSum([]byte("tampered"))}}
	rec := source.Record{Key: key, Path: filepath.Join(f.root, filepath.FromSlash(key))}
	_, err := Ship(context.Background(), hub, protocol.SourceClaudeCode, rec, nil, slog.Default())
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err = %v, want MismatchError", err)
	}
}

func TestReconcileRecoversFromConflictMidChunks(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	body := jsonlLines(10<<20, 1000)
	f.write(key, body)
	// The manifest hides everything, but the Hub already has the first 3 MiB.
	first := body[:strings.LastIndexByte(body[:3<<20], '\n')+1]
	if _, err := f.hub.Append(context.Background(), protocol.SourceClaudeCode, key, 0, protocol.EmptySha256, []byte(first)); err != nil {
		t.Fatal(err)
	}
	res := f.runWith(staleManifest{f.hub})
	if res.Uploaded != 1 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if m := f.manifest()[key]; m.Length != int64(len(body)) || m.Sha256 != hexSum([]byte(body)) {
		t.Fatalf("manifest %+v", m)
	}
}

func lastNewline(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// staleManifest hides what the Hub holds, like an append whose ack was lost.
type staleManifest struct{ Hub }

func (staleManifest) Manifest(context.Context) ([]protocol.ManifestRecord, error) { return nil, nil }

func TestReconcileRecoversFromConflict(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	f.write(key, "{\"n\":1}\n")
	f.run()
	f.write(key, "{\"n\":1}\n{\"n\":2}\n")

	recs, err := claudecode.Adapter{}.Layouts()[0].Discover(f.root)
	if err != nil {
		t.Fatal(err)
	}
	info := protocol.MachineInfo{HomeDir: "/Users/ted"}
	before := f.appends.Load()
	res, err := Reconcile(context.Background(), staleManifest{f.hub}, info, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Failed != 0 || f.appends.Load()-before != 2 {
		t.Fatalf("result %+v, %d requests", res, f.appends.Load()-before)
	}
	if c := f.hubContent(key); c != "{\"n\":1}\n{\"n\":2}\n" {
		t.Fatalf("hub content %q", c)
	}
}

func TestReconcileFillsCache(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	f.write(key, "{\"n\":1}\n{\"half\":")
	recs, err := claudecode.Adapter{}.Layouts()[0].Discover(f.root)
	if err != nil {
		t.Fatal(err)
	}
	c := cache.Load(filepath.Join(t.TempDir(), "cache.json"), "h", nil)
	if _, err := Reconcile(context.Background(), f.hub, protocol.MachineInfo{HomeDir: "/Users/ted"}, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, Options{Cache: c}); err != nil {
		t.Fatal(err)
	}
	e, ok := c.Get(cache.Key(protocol.SourceClaudeCode, key))
	fi, _ := os.Stat(filepath.Join(f.root, filepath.FromSlash(key)))
	if !ok || e.Length != 8 || e.Sha256 != hexSum([]byte("{\"n\":1}\n")) || !e.Unchanged(fi) {
		t.Fatalf("cache entry %+v, ok %v", e, ok)
	}
}

func TestReconcileStopsOnTransientError(t *testing.T) {
	for _, err := range []error{status(503), &url.Error{Op: "Post", URL: "http://hub", Err: errors.New("connection refused")}} {
		f := newFixture(t)
		f.write(proj+"/"+sess+".jsonl", "{\"n\":1}\n")
		f.write(proj+"/"+sess+"/subagents/agent-a.jsonl", "{\"n\":1}\n")
		hub := &scriptedHub{Hub: f.hub, errs: []error{err}}
		recs, _ := claudecode.Adapter{}.Layouts()[0].Discover(f.root)
		_, got := Reconcile(context.Background(), hub, protocol.MachineInfo{HomeDir: "/Users/ted"}, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, Options{})
		if !hubclient.Transient(got) {
			t.Fatalf("%v: Reconcile err = %v, want a transient error", err, got)
		}
		if hub.calls != 1 {
			t.Fatalf("%v: %d records calls, want the reconcile to stop after 1", err, hub.calls)
		}
	}
}

func TestReconcileStopsWhenAsked(t *testing.T) {
	f := newFixture(t)
	f.write(proj+"/"+sess+".jsonl", "{\"n\":1}\n")
	recs, _ := claudecode.Adapter{}.Layouts()[0].Discover(f.root)
	stop := make(chan struct{})
	close(stop)
	_, err := Reconcile(context.Background(), f.hub, protocol.MachineInfo{HomeDir: "/Users/ted"}, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, Options{Stop: stop})
	if !errors.Is(err, ErrStopped) || f.appends.Load() != 0 {
		t.Fatalf("err = %v, %d uploads", err, f.appends.Load())
	}
}
