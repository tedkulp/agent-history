// PROTOTYPE — throwaway. Fake in-memory data for the web UI prototype.
// Shapes follow the Normalized Transcript model (#9) and Hub storage schema (#12),
// loosely. Nothing here is meant to survive into the real Hub.
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Machine struct {
	ID, Name, OS, Home string
	LastSeen           time.Time
}

type Session struct {
	ID, MachineID, Source, Title, Cwd, ProjectCwd, Branch, Model string
	Started, Last                                                time.Time
	ParentID, SpawnCallID                                        string
	Unknown                                                      int
	Messages                                                     []*Message
}

type Message struct {
	ID, Role string
	Time     time.Time
	Model    string
	Parts    []*Part
}

type Part struct {
	ID, Kind   string // text, thinking, tool_call, image, attachment, marker, unknown
	Text       string
	ToolName   string
	ToolInput  string
	ToolOutput string
	Status     string // ok, error, pending
	DiffFile   string
	DiffOld    string
	DiffNew    string
	ChildID    string
}

var (
	machines   []*Machine
	sessions   []*Session
	sessByID   = map[string]*Session{}
	partByID   = map[string]*Part{}
	machByID   = map[string]*Machine{}
	baseTime   = time.Date(2026, 9, 26, 9, 0, 0, 0, time.Local)
	partSerial int
)

// --- builder ---------------------------------------------------------------

type sb struct {
	s   *Session
	t   time.Time
	cur *Message
}

func newSession(id, mach, source, title, cwd, branch string, daysAgo float64) *sb {
	m := machByID[mach]
	pc := cwd
	if cwd == m.Home || strings.HasPrefix(cwd, "/tmp") || strings.HasPrefix(cwd, "/private/tmp") {
		pc = ""
	}
	model := map[string]string{"claude-code": "claude-opus-5-5", "codex": "gpt-5.3-codex", "opencode": "claude-sonnet-5", "oh-my-pi": "claude-sonnet-5"}[source]
	start := baseTime.Add(-time.Duration(daysAgo * float64(24*time.Hour)))
	s := &Session{ID: id, MachineID: mach, Source: source, Title: title, Cwd: cwd, ProjectCwd: pc, Branch: branch, Model: model, Started: start}
	sessions = append(sessions, s)
	sessByID[id] = s
	return &sb{s: s, t: start}
}

func (b *sb) msg(role string) *Message {
	b.t = b.t.Add(time.Duration(40+len(b.s.Messages)*7%90) * time.Second)
	m := &Message{ID: fmt.Sprintf("%s-m%d", b.s.ID, len(b.s.Messages)+1), Role: role, Time: b.t}
	if role == "assistant" {
		m.Model = b.s.Model
	}
	b.s.Messages = append(b.s.Messages, m)
	b.s.Last = b.t
	b.cur = m
	return m
}

func (b *sb) part(p *Part) *sb {
	partSerial++
	p.ID = fmt.Sprintf("p%d", partSerial)
	partByID[p.ID] = p
	b.cur.Parts = append(b.cur.Parts, p)
	return b
}

func (b *sb) user(text string) *sb { b.msg("user"); return b.part(&Part{Kind: "text", Text: text}) }
func (b *sb) asst(text string) *sb {
	b.msg("assistant")
	if text != "" {
		b.part(&Part{Kind: "text", Text: text})
	}
	return b
}
func (b *sb) say(text string) *sb   { return b.part(&Part{Kind: "text", Text: text}) }
func (b *sb) think(text string) *sb { return b.part(&Part{Kind: "thinking", Text: text}) }
func (b *sb) tool(name, in, out string) *sb {
	return b.part(&Part{Kind: "tool_call", ToolName: name, ToolInput: in, ToolOutput: out, Status: "ok"})
}
func (b *sb) toolErr(name, in, out string) *sb {
	return b.part(&Part{Kind: "tool_call", ToolName: name, ToolInput: in, ToolOutput: out, Status: "error"})
}
func (b *sb) edit(file, old, new string) *sb {
	return b.part(&Part{Kind: "tool_call", ToolName: "Edit", ToolInput: file, DiffFile: file, DiffOld: old, DiffNew: new, ToolOutput: "The file " + file + " has been updated.", Status: "ok"})
}
func (b *sb) task(child *Session, desc string) *sb {
	child.ParentID = b.s.ID
	b.part(&Part{Kind: "tool_call", ToolName: "Task", ToolInput: desc, ToolOutput: "Sub-agent finished: " + child.Title, Status: "ok", ChildID: child.ID})
	child.SpawnCallID = b.cur.Parts[len(b.cur.Parts)-1].ID
	return b
}
func (b *sb) marker(text string) *sb {
	b.msg("assistant")
	return b.part(&Part{Kind: "marker", Text: text})
}
func (b *sb) unknown(typ, excerpt string) *sb {
	b.s.Unknown++
	return b.part(&Part{Kind: "unknown", ToolName: typ, Text: excerpt})
}
func (b *sb) attach(label string) *sb { return b.part(&Part{Kind: "attachment", Text: label}) }

// --- fixtures --------------------------------------------------------------

func bigFile() string {
	var sb strings.Builder
	sb.WriteString("package protocol\n\n// Record is one Raw record as the Collector ships it.\n")
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&sb, "func handler%03d(w http.ResponseWriter, r *http.Request) { // line %d\n\tdecodeDelta(r.Body, %d)\n}\n", i, i*3+4, i)
	}
	return sb.String()
}

func testLog() string {
	var sb strings.Builder
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&sb, "=== RUN   TestInvoice/case_%02d\n--- PASS: TestInvoice/case_%02d (0.0%ds)\n", i, i, i%9)
	}
	sb.WriteString("=== RUN   TestInvoice/rounding_eur\n    invoice_test.go:212: total = 100.01, want 100.00\n--- FAIL: TestInvoice/rounding_eur (0.01s)\nFAIL\n")
	return sb.String()
}

func init() {
	for _, m := range []*Machine{
		{ID: "m1", Name: "mbp-ted", OS: "darwin/arm64", Home: "/Users/ted", LastSeen: baseTime.Add(-2 * time.Minute)},
		{ID: "m2", Name: "forge", OS: "linux/amd64", Home: "/home/ted", LastSeen: baseTime.Add(-40 * time.Minute)},
		{ID: "m3", Name: "work-mbp", OS: "darwin/arm64", Home: "/Users/tkulp", LastSeen: baseTime.Add(-26 * time.Hour)},
	} {
		machines = append(machines, m)
		machByID[m.ID] = m
	}

	// Child first so the parent can link it.
	child := newSession("s2", "m1", "claude-code", "Research: opencode storage layout", "/Users/ted/src/agent-history", "main", 0.3)
	child.user("Find where opencode stores sessions on disk and whether the legacy JSON tree is still read.").
		asst("Checking the opencode source.").
		tool("Grep", `pattern: "storage/session"  path: packages/opencode/src`, "src/storage/storage.ts:41\nsrc/storage/migrate.ts:12\nsrc/session/index.ts:88").
		tool("Read", "packages/opencode/src/storage/migrate.ts", "export async function migrate() {\n  // copies legacy JSON files into opencode.db once\n  // the legacy tree is never deleted\n}").
		asst("opencode moved to a SQLite DB (`opencode.db`, WAL mode). The legacy `storage/` JSON tree is migrated once and **never deleted**, so both eras must be read.")

	s := newSession("s1", "m1", "claude-code", "Design the Collector → Hub ingestion protocol", "/Users/ted/src/agent-history", "main", 0.3)
	s.user("Let's design the ingestion protocol between the Collector and the Hub. Start by reading the protocol stub I have.").
		asst("I'll read the current stub first.").
		think("The user wants a protocol design. The stub is probably large; I should skim the handler shapes before proposing append deltas vs full replace.").
		tool("Read", "internal/protocol/record.go", bigFile()).
		say("The stub has 160 handlers, which is generated noise. The real shape is: one Raw record per file, identified by a record key.").
		user("Right. How should appends work? Files grow every few seconds while a Session is live.").
		asst("Two options:\n\n1. **Append deltas**: send `(record_key, offset, bytes, prefix_sha256)`; the Hub checks the prefix hash and appends.\n2. **Full replace**: resend the whole file each time.\n\nDeltas are cheaper. If the prefix hash mismatches, the Hub answers `409` and the Collector falls back to a full replace.").
		user("Go with deltas plus the 409 fallback. Can you look at how opencode stores things? It's not file-per-session anymore I think.").
		asst("I'll spin up a sub-agent for that.").
		task(child.s, "Research opencode on-disk storage: DB vs legacy JSON tree").
		say("The sub-agent confirmed opencode uses `opencode.db` now. For opencode, the Raw record will be a Session's rows exported as JSONL.").
		user("Good. Write the delta request type.").
		asst("").
		edit("internal/protocol/record.go",
			"type Record struct {\n\tKey  string\n\tBody []byte\n}",
			"type Record struct {\n\tKey          string\n\tOffset       int64\n\tBytes        []byte\n\tPrefixSHA256 string // hex; empty when Offset == 0\n}").
		tool("Bash", "go build ./...", "").
		toolErr("Bash", "go test ./internal/protocol/...", "--- FAIL: TestAppend (0.00s)\n    record_test.go:31: unknown field Body in struct literal\nFAIL").
		say("One test still uses the old `Body` field. Fixing it.").
		edit("internal/protocol/record_test.go", "r := Record{Key: \"a\", Body: []byte(\"x\")}", "r := Record{Key: \"a\", Bytes: []byte(\"x\")}").
		tool("Bash", "go test ./internal/protocol/...", "ok  \tgithub.com/tedkulp/agent-history/internal/protocol\t0.012s")
	s.marker("Context compacted: 41 messages summarized")
	s.user("Now write up the 409 → full-replace fallback so I can paste it in the ticket.").
		asst("**Fallback rule**\n\nWhen the Hub's stored prefix hash for `record_key` does not match `PrefixSHA256`, it answers `409 Conflict` with its current `(length, sha256)`. The Collector then sends a full replace. The Hub keeps the replaced content as a **superseded version**; nothing is deleted.\n\n```\nPOST /api/v1/records/append   -> 200 | 409\nPOST /api/v1/records/replace  -> 200\nGET  /api/v1/manifest         -> {record_key: [length, sha256]}\n```")

	b := newSession("s3", "m1", "codex", "Add SQLite migration runner", "/Users/ted/src/agent-history", "hub-schema", 1.2)
	b.user("Add a tiny migration runner: numbered .sql files embedded, tracked by PRAGMA user_version.").
		asst("Creating `internal/db/migrate.go`.").
		edit("internal/db/migrate.go", "", "//go:embed migrations/*.sql\nvar migrations embed.FS\n\nfunc Migrate(db *sql.DB) error {\n\tvar v int\n\tdb.QueryRow(\"PRAGMA user_version\").Scan(&v)\n\t// apply each file > v in its own transaction\n}").
		tool("Bash", "go test ./internal/db/", "ok  \tgithub.com/tedkulp/agent-history/internal/db\t0.031s").
		asst("Done. Migrations are forward-only; each file runs in one transaction and bumps `user_version`.")

	b = newSession("s4", "m1", "opencode", "Fix broken RSS feed dates", "/Users/ted/src/blog", "main", 2.5)
	b.user("The RSS feed shows every post dated 1970. Fix it.").
		asst("Looking at the feed template.").
		tool("Read", "layouts/_default/rss.xml", "<pubDate>{{ .Date.Unix }}</pubDate>").
		edit("layouts/_default/rss.xml", "<pubDate>{{ .Date.Unix }}</pubDate>", "<pubDate>{{ .Date.Format \"Mon, 02 Jan 2006 15:04:05 -0700\" }}</pubDate>").
		attach("snapshot: layouts/_default/rss.xml (patch)").
		asst("RSS wants RFC 1123Z dates, not a Unix timestamp. Fixed.")

	b = newSession("s5", "m1", "claude-code", "What's eating my disk space?", "/Users/ted", "", 3.1)
	b.user("My disk is almost full. What's using it?").
		asst("").
		tool("Bash", "du -sh ~/* 2>/dev/null | sort -h | tail -5", "4.1G\t/Users/ted/Movies\n9.8G\t/Users/ted/Library\n22G\t/Users/ted/src\n31G\t/Users/ted/.ollama").
		asst("`~/.ollama` is 31 GB of local models. `ollama rm` the ones you don't use.")

	b = newSession("s6", "m2", "oh-my-pi", "Upgrade k3s cluster to 1.31", "/home/ted/src/homelab", "k3s-131", 0.8)
	b.user("Plan the k3s upgrade from 1.29 to 1.31. Can't skip minors, right?").
		asst("Correct: go 1.29 → 1.30 → 1.31, servers first, then agents.").
		tool("Bash", "kubectl get nodes -o wide", "NAME    STATUS   ROLES                  VERSION\nnode1   Ready    control-plane,master   v1.29.6+k3s1\nnode2   Ready    <none>                 v1.29.6+k3s1\nnode3   Ready    <none>                 v1.29.6+k3s1").
		edit("ansible/group_vars/all.yml", "k3s_version: v1.29.6+k3s1", "k3s_version: v1.30.4+k3s1").
		tool("Bash", "ansible-playbook upgrade.yml --limit node1", "PLAY RECAP\nnode1 : ok=12 changed=3 unreachable=0 failed=0").
		asst("node1 is on 1.30. Run the playbook for the agents next, then repeat for 1.31.")

	b = newSession("s7", "m2", "codex", "Write backup cron for postgres", "/home/ted/src/homelab", "main", 4.0)
	b.user("Nightly pg_dump of the immich database to the NAS, keep 14 days.").
		asst("").
		edit("backup/pg-backup.sh", "", "#!/bin/sh\npg_dump -Fc immich > /mnt/nas/pg/immich-$(date +%F).dump\nfind /mnt/nas/pg -name '*.dump' -mtime +14 -delete").
		edit("backup/crontab", "", "15 3 * * * /opt/backup/pg-backup.sh").
		asst("Runs at 03:15 nightly and prunes dumps older than 14 days.")

	b = newSession("s8", "m2", "claude-code", "Quick regex question", "/tmp/scratch", "", 5.5)
	b.user("Regex to match a semver like 1.2.3 or 1.2.3-rc.1?").
		asst("```\n^\\d+\\.\\d+\\.\\d+(-[0-9A-Za-z.-]+)?$\n```")

	b = newSession("s9", "m3", "claude-code", "Debug flaky invoice test", "/Users/tkulp/work/billing-api", "fix-rounding", 1.9)
	b.user("TestInvoice fails maybe 1 in 5 runs. Find out why.").
		asst("Running it with -count to reproduce.").
		toolErr("Bash", "go test ./invoice -run TestInvoice -count=20", testLog()).
		think("rounding_eur fails with 100.01 vs 100.00. Likely float64 summation order changing with map iteration. Check for a map range in Total().").
		tool("Grep", `pattern: "range lines"  path: invoice/`, "invoice/total.go:18:\tfor _, l := range linesByID {").
		asst("Found it: `Total()` sums float64 line amounts while ranging over a **map**, so the order (and the rounding) changes between runs.").
		edit("invoice/total.go", "for _, l := range linesByID {\n\ttotal += l.Amount\n}", "for _, id := range sortedIDs(linesByID) {\n\ttotal = total.Add(linesByID[id].Amount) // decimal.Decimal\n}").
		tool("Bash", "go test ./invoice -run TestInvoice -count=50", "ok  \tbilling-api/invoice\t0.412s").
		asst("Switched to `decimal.Decimal` and a sorted iteration order. 50/50 passes.")

	b = newSession("s10", "m3", "opencode", "Add retry to Stripe webhook handler", "/Users/tkulp/work/billing-api", "webhook-retry", 6.0)
	b.user("Stripe webhooks sometimes hit us while the DB is failing over. Add a retry with backoff.").
		asst("").
		edit("webhooks/stripe.go", "if err := store.Save(evt); err != nil {\n\treturn err\n}", "err := retry.Do(ctx, 5, 200*time.Millisecond, func() error {\n\treturn store.Save(evt)\n})\nif err != nil {\n\treturn err\n}").
		asst("Retries 5 times with exponential backoff from 200 ms. Stripe itself also retries for 3 days, so this only smooths over short failovers.")

	b = newSession("s11", "m3", "oh-my-pi", "Migrate date-fns to Temporal", "/Users/tkulp/work/web-app", "temporal", 2.2)
	b.user("Replace date-fns with the Temporal API across src/utils/dates.ts.").
		asst("").
		unknown("ttsr_injection", `{"type":"ttsr_injection","rule":"no-date-fns","hits":3}`).
		edit("src/utils/dates.ts", "import { addDays, format } from 'date-fns'\nexport const tomorrow = () => format(addDays(new Date(), 1), 'yyyy-MM-dd')", "export const tomorrow = () =>\n  Temporal.Now.plainDateISO().add({ days: 1 }).toString()").
		asst("Converted. `Temporal.PlainDate` gives ISO strings directly, so the formatter call is gone.")

	b = newSession("s12", "m1", "claude-code", "Hub storage schema", "/Users/ted/src/agent-history", "main", 0.9)
	b.user("Where should raw records live: files on the volume or blobs in SQLite?").
		asst("SQLite. One file to back up. Store each accepted append as a **zstd-compressed chunk row**, and keep the serialized sha256 state per version so append checks never re-read the record.").
		user("And full-text search granularity?").
		asst("One FTS5 row per Message, so a hit maps straight to a Message anchor. Index text Parts plus tool name and input; skip tool output and thinking.")

	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Last.After(sessions[j].Last) })
}

// --- queries ---------------------------------------------------------------

type Project struct {
	MachineID, Cwd string // Cwd "" = No project
	Sessions       int
	Last           time.Time
}

func (p Project) Name() string {
	if p.Cwd == "" {
		return "No project"
	}
	return p.Cwd[strings.LastIndex(p.Cwd, "/")+1:]
}

func (p Project) Key() string {
	if p.Cwd == "" {
		return "-"
	}
	return p.Cwd
}

func projectsFor(machineID string) []Project {
	idx := map[string]*Project{}
	var out []*Project
	for _, s := range sessions {
		if s.MachineID != machineID || s.ParentID != "" {
			continue
		}
		p := idx[s.ProjectCwd]
		if p == nil {
			p = &Project{MachineID: machineID, Cwd: s.ProjectCwd}
			idx[s.ProjectCwd] = p
			out = append(out, p)
		}
		p.Sessions++
		if s.Last.After(p.Last) {
			p.Last = s.Last
		}
	}
	res := make([]Project, len(out))
	for i, p := range out {
		res[i] = *p
	}
	return res
}

type Filter struct{ Machine, Project, Source string }

func topSessions(f Filter) []*Session {
	var out []*Session
	for _, s := range sessions {
		if s.ParentID != "" {
			continue
		}
		if f.Machine != "" && s.MachineID != f.Machine {
			continue
		}
		if f.Project != "" && projKey(s) != f.Project {
			continue
		}
		if f.Source != "" && s.Source != f.Source {
			continue
		}
		out = append(out, s)
	}
	return out
}

func projKey(s *Session) string {
	if s.ProjectCwd == "" {
		return "-"
	}
	return s.ProjectCwd
}

type Hit struct {
	Session *Session
	Message *Message
	Field   string // title, text, tool
	Text    string
}

// search: naive substring over title, text Parts, tool name + input.
// Mirrors the FTS decision in #12: no tool output, no thinking.
func search(q string, f Filter) []Hit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	var hits []Hit
	for _, s := range sessions {
		if f.Machine != "" && s.MachineID != f.Machine || f.Source != "" && s.Source != f.Source || f.Project != "" && projKey(s) != f.Project {
			continue
		}
		if strings.Contains(strings.ToLower(s.Title), q) {
			hits = append(hits, Hit{Session: s, Field: "title", Text: s.Title})
		}
	msgLoop:
		for _, m := range s.Messages {
			for _, p := range m.Parts {
				var txt, field string
				switch p.Kind {
				case "text":
					txt, field = p.Text, "text"
				case "tool_call":
					txt, field = p.ToolName+" "+p.ToolInput+" "+p.DiffNew, "tool"
				}
				if txt != "" && strings.Contains(strings.ToLower(txt), q) {
					hits = append(hits, Hit{Session: s, Message: m, Field: field, Text: txt})
					continue msgLoop
				}
			}
		}
	}
	return hits
}
