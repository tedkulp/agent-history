package exclude

import (
	"errors"
	"testing"

	"github.com/tedkulp/agent-history/internal/collector/source"
)

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"/Users/ted/src/secret/**", "/Users/ted/src/secret", true},
		{"/Users/ted/src/secret/**", "/Users/ted/src/secret/app", true},
		{"/Users/ted/src/secret/**", "/Users/ted/src/secret/a/b/c", true},
		{"/Users/ted/src/secret/**", "/Users/ted/src/secretive", false},
		{"/Users/ted/src/secret/**", "/Users/ted/src", false},
		{"/Users/ted/src/secret", "/Users/ted/src/secret", true},
		{"/Users/ted/src/secret", "/Users/ted/src/secret/app", false},
		{"/Users/*/src/secret", "/Users/ted/src/secret", true},
		{"/Users/*/src/secret", "/Users/ted/x/src/secret", false},
		{"/**/client-*/**", "/Users/ted/work/client-acme/api", true},
		{"/**/client-*/**", "/Users/ted/work/clients/api", false},
		{"/**/client-*", "/client-acme", true},
		{"**", "/anything/at/all", true},
		{"/a/**/b", "/a/b", true},
		{"/a/**/b", "/a/x/y/b", true},
		{"/a/**/b", "/a/x/y/c", false},
		{"/a/?/c", "/a/b/c", true},
		{"/a/[bc]/d", "/a/c/d", true},
	} {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestNewRejectsBadPattern(t *testing.T) {
	if _, err := New([]string{"/ok/**", "/bad/[x"}); err == nil {
		t.Fatal("New accepted a malformed pattern")
	}
}

// fakeLayout answers StartCwd and Parent from maps.
type fakeLayout struct {
	source.Layout
	cwd    map[string]string // Record key → starting cwd; missing means not readable yet
	parent map[string]string // Record key → parent Record key
	reads  map[string]int
}

func (l *fakeLayout) StartCwd(rec source.Record) (string, error) {
	l.reads[rec.Key]++
	if c, ok := l.cwd[rec.Key]; ok {
		return c, nil
	}
	if rec.Key == "broken" {
		return "", errors.New("unreadable")
	}
	return "", nil
}

func (l *fakeLayout) Parent(root string, rec source.Record) (source.Record, bool) {
	p, ok := l.parent[rec.Key]
	return source.Record{Key: p, Path: root + "/" + p}, ok
}

func newFake() *fakeLayout {
	return &fakeLayout{
		cwd: map[string]string{
			"secret.jsonl": "/src/secret/app",
			"open.jsonl":   "/src/open",
		},
		parent: map[string]string{
			"secret/agent.jsonl":     "secret.jsonl",
			"secret/tool-results/a":  "secret.jsonl",
			"open/custom-title.json": "open.jsonl",
			"late/agent.jsonl":       "late.jsonl",
		},
		reads: map[string]int{},
	}
}

func rec(key string) source.Record { return source.Record{Key: key, Path: "/root/" + key} }

func TestFilterDecides(t *testing.T) {
	f, err := New([]string{"/src/secret/**"})
	if err != nil {
		t.Fatal(err)
	}
	l := newFake()
	// A Child Session's own cwd doesn't matter: it follows its parent.
	l.cwd["secret/agent.jsonl"] = "/src/open"
	for _, c := range []struct {
		key                 string
		excluded, decidable bool
	}{
		{"secret.jsonl", true, true},
		{"secret/agent.jsonl", true, true},
		{"secret/tool-results/a", true, true},
		{"open.jsonl", false, true},
		{"open/custom-title.json", false, true},
		{"late.jsonl", false, false},
		{"late/agent.jsonl", false, false},
		{"broken", false, false},
	} {
		ex, ok := f.Excluded("claude-code", l, "/root", rec(c.key))
		if ex != c.excluded || ok != c.decidable {
			t.Errorf("Excluded(%q) = %v, %v; want %v, %v", c.key, ex, ok, c.excluded, c.decidable)
		}
	}
	if l.reads["secret/agent.jsonl"] != 0 {
		t.Error("read the cwd of a record that has a parent")
	}
	if n := f.Count("claude-code"); n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
	if n := f.Count("codex"); n != 0 {
		t.Errorf("Count(codex) = %d, want 0", n)
	}

	// Decisions are cached; undecided records are asked again.
	reads := l.reads["secret.jsonl"]
	f.Excluded("claude-code", l, "/root", rec("secret.jsonl"))
	if l.reads["secret.jsonl"] != reads {
		t.Error("decided record's cwd was read again")
	}
	l.cwd["late.jsonl"] = "/src/secret"
	if ex, ok := f.Excluded("claude-code", l, "/root", rec("late/agent.jsonl")); !ex || !ok {
		t.Errorf("once readable, late child = %v, %v; want excluded", ex, ok)
	}
	if n := f.Count("claude-code"); n != 5 {
		t.Errorf("Count = %d, want 5", n)
	}
}

func TestFilterForget(t *testing.T) {
	f, _ := New([]string{"/src/secret/**"})
	l := newFake()
	f.Excluded("claude-code", l, "/root", rec("secret.jsonl"))
	f.Excluded("claude-code", l, "/root", rec("secret/agent.jsonl"))
	f.Forget("claude-code", map[string]bool{"secret.jsonl": true})
	if n := f.Count("claude-code"); n != 1 {
		t.Errorf("Count after Forget = %d, want 1", n)
	}
}

func TestEmptyFilterNeverReads(t *testing.T) {
	for _, f := range []*Filter{nil, mustNew(t, nil)} {
		l := newFake()
		if ex, ok := f.Excluded("claude-code", l, "/root", rec("late.jsonl")); ex || !ok {
			t.Errorf("empty filter: Excluded = %v, %v; want ship", ex, ok)
		}
		if len(l.reads) != 0 {
			t.Error("empty filter read a cwd")
		}
		if f.Count("claude-code") != 0 {
			t.Error("empty filter counted exclusions")
		}
	}
}

func mustNew(t *testing.T, patterns []string) *Filter {
	t.Helper()
	f, err := New(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
