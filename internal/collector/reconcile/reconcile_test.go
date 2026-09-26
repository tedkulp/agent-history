package reconcile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, store: st, root: t.TempDir()}
	h := api.New(st, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.appends.Add(1)
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
	res, err := Reconcile(context.Background(), f.hub, info, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, nil)
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
	if res.Failed != 0 || res.Deferred != 0 {
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

func TestReconcileDefersRewrittenRecord(t *testing.T) {
	f := newFixture(t)
	key := proj + "/" + sess + ".jsonl"
	f.write(key, "{\"n\":1}\n")
	f.run()
	f.write(key, "{\"n\":9}\n{\"n\":2}\n")
	res := f.run()
	if res.Deferred != 1 || res.Uploaded != 0 {
		t.Fatalf("result %+v", res)
	}
	if c := f.hubContent(key); c != "{\"n\":1}\n" {
		t.Fatalf("hub content changed to %q", c)
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
	res, err := Reconcile(context.Background(), staleManifest{f.hub}, info, []Source{{ID: protocol.SourceClaudeCode, Records: recs}}, nil)
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
