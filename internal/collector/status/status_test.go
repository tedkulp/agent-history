package status

import (
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/protocol"
)

func ptr[T any](v T) *T { return &v }

func TestFormatRunningService(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	r := Report{
		Version:          "0.3.1",
		MachineID:        "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33",
		DisplayName:      "work-laptop",
		HubURL:           "http://hub.vpn:8080",
		ServiceRunning:   true,
		ServiceInstalled: true,
		Hub:              Hub{Reachable: ptr(true), LastSync: ptr(now.Add(-12 * time.Second)), Parsing: &protocol.Health{SessionsWithWarnings: 3}},
		Pending:          ptr(0),
		Sources: []Source{
			{ID: "claude-code", Root: "/Users/ted/.claude/projects", Enabled: true, Detected: true, Layouts: []string{"jsonl"}, Records: 1284, Excluded: ptr(12)},
			{ID: "oh-my-pi", Root: "/Users/ted/.omp/agent", Enabled: true},
			{ID: "opencode", Enabled: false},
		},
	}
	var b strings.Builder
	Format(&b, r, now)
	want := `agent-history 0.3.1   machine 3f6c2a4e…  "work-laptop"   service: running
Hub      http://hub.vpn:8080  reachable, last sync 12s ago
         Hub: 3 Sessions with parse warnings, 0 failed to parse
Uploads  0 pending

claude-code  detected       /Users/ted/.claude/projects
             layouts: jsonl   records: 1,284 (12 excluded)
oh-my-pi     not detected   /Users/ted/.omp/agent
opencode     disabled
`
	if got := b.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatWarningsAndErrors(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	r := Report{
		Version:          "0.3.1",
		MachineID:        "abc",
		HubURL:           "http://hub",
		ServiceInstalled: true,
		ServiceOutdated:  true,
		Hub:              Hub{Reachable: ptr(false), LastError: &Failure{"connection refused", now.Add(-3 * time.Minute)}},
		Sources: []Source{
			{ID: "claude-code", Root: "/r", Enabled: true, Detected: true, Layouts: []string{"jsonl"}, Records: 5,
				LastError: &Failure{"permission denied", now.Add(-2 * time.Hour)}},
		},
	}
	var b strings.Builder
	Format(&b, r, now)
	got := b.String()
	for _, want := range []string{
		"service: not running",
		"Hub      http://hub  unreachable\n",
		"last error 3m ago: connection refused",
		"Service  service definition outdated, run `agent-history service install`",
		"records: 5\n",
		"last error 2h ago: permission denied",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Uploads") {
		t.Errorf("unknown pending count printed:\n%s", got)
	}
}

func TestFormatServiceNotInstalled(t *testing.T) {
	var b strings.Builder
	Format(&b, Report{}, time.Now())
	if !strings.Contains(b.String(), "Service  not installed") {
		t.Errorf("got:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "not contacted yet") {
		t.Errorf("got:\n%s", b.String())
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1284: "1,284", 1234567: "1,234,567"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFormatUpgradeRequired(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for min, want := range map[string]string{
		"0.4.0": "Hub      http://hub  upgrade Collector (Hub requires ≥ 0.4.0), last sync 3h ago\n",
		"":      "Hub      http://hub  upgrade Collector (Hub requires a newer version), last sync 3h ago\n",
	} {
		r := Report{
			HubURL:           "http://hub",
			ServiceInstalled: true,
			Hub:              Hub{Reachable: ptr(true), LastSync: ptr(now.Add(-3 * time.Hour)), UpgradeRequired: true, MinCollectorVersion: min},
		}
		var b strings.Builder
		Format(&b, r, now)
		if got := b.String(); !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatDrift(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	mod := time.Date(2026, 9, 25, 18, 2, 0, 0, time.UTC)
	r := Report{
		ServiceInstalled: true,
		Sources: []Source{
			{ID: "claude-code", Root: "/r", Enabled: true, Detected: true, Layouts: []string{"jsonl"}, Records: 5,
				Unclaimed: 7, UnclaimedPaths: []string{"a", "b", "c", "d", "e"},
				Ignored: 8, IgnoredPaths: []IgnoredPath{{"-p/memory/MEMORY.md", mod}}},
			{ID: "codex", Root: "/c", Enabled: true, Detected: true, Layouts: []string{"jsonl"}, Records: 1,
				Unclaimed: 1, UnclaimedPaths: []string{"x.bin"}},
		},
	}
	var b strings.Builder
	Format(&b, r, now)
	got := b.String()
	for _, want := range []string{
		"             ignored: -p/memory/MEMORY.md (modified " + mod.Local().Format("2006-01-02 15:04") + ")\n",
		"             ignored: 7 more\n",
		"             unclaimed: 7, not shipped: a, b, c, d, e, …\n",
		"             unclaimed: 1, not shipped: x.bin\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatParseHealthSingular(t *testing.T) {
	var b strings.Builder
	Format(&b, Report{Hub: Hub{Parsing: &protocol.Health{SessionsWithWarnings: 1, SessionsFailed: 1}}}, time.Now())
	if want := "Hub: 1 Session with parse warnings, 1 failed to parse\n"; !strings.Contains(b.String(), want) {
		t.Errorf("missing %q in:\n%s", want, b.String())
	}
}
