# Claude Code as a Source — on-disk Session/Transcript format

Research date: 2026-09-26. Local Claude Code version inspected: **2.1.283** (macOS, `~/.claude`).
Primary sources: official docs at code.claude.com/docs (formerly docs.claude.com — redirects there),
the `anthropics/claude-code` `CHANGELOG.md`, and direct on-disk inspection of `~/.claude` on this machine.

## 1. Where history lives on disk

Root config/data directory: **`~/.claude`** (macOS and Linux both; Windows uses `%USERPROFILE%\.claude`).
Override with the **`CLAUDE_CONFIG_DIR`** environment variable — when set, "every `~/.claude` path... lives
under that directory instead" (code.claude.com/docs/en/claude-directory). This is a first-party,
documented override, not a hack (confirmed independently by `CHANGELOG.md` line "Respect
CLAUDE_CONFIG_DIR everywhere" at v1.0.6, and many later fixes that make UI surfaces honor it).

Session Transcripts (what agent-history calls a **Transcript** + **Raw record**, since for Claude Code
these are the same file — the on-disk format *is* the transcript, there's no separate "raw" export):

```
~/.claude/projects/<project>/<session>.jsonl
```

- `<project>` is the working-directory path with `/` replaced by `-` (e.g. `/Users/ted/src/foo` →
  `-Users-ted-src-foo`). Confirmed on disk: `~/.claude/projects/-Users-tedkulp-src-agent-history/`.
- `<session>` is a UUID (v4-looking), e.g. `3d34bfcc-90e7-4fd0-900f-86c047b22433.jsonl`. This UUID is
  the **sessionId**, and it is the field also embedded inside every line of the file (see §3).
- Related/adjacent files documented officially:
  - `projects/<project>/<session>.orphaned-<timestamp>-<suffix>.jsonl`
  - `projects/<project>/<session>.jsonl.superseded-<timestamp>`
  (previous transcripts Claude Code "set aside instead of overwriting" — they don't show in the
  session picker but are not deleted.)
  - `projects/<project>/<session>/subagents/` — sub-agent transcripts (see §7)
  - `projects/<project>/<session>/tool-results/` — large tool outputs spilled to separate files
    (confirmed on disk: a `WebFetch` result too large to inline was written to
    `.../<session>/tool-results/toolu_<id>.txt` and the transcript line just referenced it)
  - `projects/<project>/memory/` — "Auto memory," Claude's own cross-session notes, keyed by
    repo path; explicitly *not* subject to the age-based cleanup sweep except that an emptied
    directory can be removed.

Other on-disk pieces worth knowing about (from `code.claude.com/docs/en/claude-directory`):
- `~/.claude/history.jsonl` — one line per **prompt typed** (for up-arrow/Ctrl+R recall), *not* a
  full transcript. Has `display`, `pastedContents`, `timestamp` (epoch ms), `project`, `sessionId`.
  This file is explicitly **not** subject to the retention sweep ("kept until manually deleted").
- `~/.claude.json` (note: in `$HOME` directly, not inside `~/.claude/`) — app state, OAuth, per-project
  UI state, MCP server list; backed up on every write to `~/.claude/backups/.claude.json.backup.<ts>`
  (keeps 5 newest).
- `~/.claude/sessions/` — one small liveness file per *currently running* CLI process (for detecting
  concurrent sessions/crashes), removed on clean exit, swept on next launch — not part of the
  age-based cleanup.
- `~/.claude/file-history/<session>/` — pre-edit file snapshots for checkpoint/undo, capped at the
  100 most recent checkpoints.
- `~/.claude/shell-snapshots/`, `session-env/` — captured shell state, not transcript content.
- Per-session scratchpad (not history, but env-relevant): macOS
  `/private/tmp/claude-<uid>/<project>/<session-id>/scratchpad/`, Linux `/tmp/claude-<uid>/...`
  (or under `$TMPDIR`).

## 2. File format

**JSONL** — one JSON object per line, no enclosing array, no pretty-printing (lines are minified,
no spaces after `:`/`,`). Stored as **plaintext** on disk (not encrypted at rest by Claude Code itself;
docs explicitly say: "Claude Code clients store session transcripts locally in plaintext under
`~/.claude/projects/`" — code.claude.com/docs/en/data-usage#data-retention).

Every line has at least `type` and (for conversation-bearing lines) `sessionId`. Observed `type`
values on disk in this install: `user`, `assistant`, `system`, `attachment`, `file-history-snapshot`,
`mode`, `permission-mode`, `atis-latch`, `last-prompt`, `summary` (leaf-uuid pointer). Only `user` and
`assistant` carry the actual conversation payload; the rest are session/UI bookkeeping interleaved in
the same file.

## 3. How a Session is identified, and message threading

- **sessionId**: a UUID, stable for the life of one CLI invocation/resume chain; it is both the
  filename (`<session>.jsonl`) and a field repeated on every substantive line.
- **uuid** / **parentUuid**: every conversational line has its own `uuid`, and a `parentUuid` pointing
  at the line it responds to — this is a linked list/tree, not just line order, and is how forks
  (e.g. `/rewind`, retried turns) are represented without deleting anything.
- **`version`**: the exact Claude Code build that wrote the line (e.g. `"2.1.283"`) — present per-line,
  so a single file can span multiple client versions if the binary was upgraded mid-session.
- **`cwd`** and **`gitBranch`**: present on essentially every line — this is per-message, not just
  per-session, so a `cd` or `git checkout` mid-session is captured at the point it happened.
- Resuming a session (`claude --resume` / `--continue`) **appends more lines to the same file** —
  the sessionId and filename do not change across a resume. (This has been a stable CLI feature since
  `v0.2.93` per `CHANGELOG.md`: "Resume conversations from where you left off from with
  `claude --continue` and `claude --resume`".)

## 4. Append vs. rewrite while a Session is live

Empirically **append-only** while a session is live, confirmed two ways:
1. On disk, `mtime` keeps advancing and file size only grows during an active session (checked
   directly on this run's own subagent transcript file — see §7 — size went from X to a larger X
   after further turns, never shrank).
2. Directly observed granularity: I ran two more tool calls within the same assistant turn and the
   file's byte size **did not change** until the turn (including all its tool calls) fully completed —
   i.e. Claude Code batches the write per completed assistant turn/message rather than streaming each
   token or each individual tool call to disk mid-turn. So it's "append per turn," not "append per
   byte," but never a full-file rewrite of prior content.
3. Confirms the docs' own language: transcripts that would otherwise be overwritten are instead
   renamed to `.orphaned-<timestamp>-<suffix>.jsonl` / `.jsonl.superseded-<timestamp>` — i.e. even in
   edge cases (e.g. resume conflicts), Claude Code goes out of its way to avoid destructively
   overwriting a transcript in place; it moves the old one aside instead.

## 5. Metadata present per line

From direct inspection of real (redacted) lines, an `assistant` line looks like:

- `sessionId`, `uuid`, `parentUuid`, `version`, `gitBranch`, `cwd`, `entrypoint` (e.g. `"cli"`),
  `userType` (e.g. `"external"`), `timestamp` (ISO-8601 UTC), `isSidechain` (bool — see §7),
  `effort` (e.g. `"high"`), `requestId`.
- `message.model` — the exact model id used for that turn (e.g. `"claude-opus-5"`), so model choice
  is tracked **per assistant message**, not just per session (relevant if a session mixes models).
- `message.usage` — full token accounting per turn: `input_tokens`, `output_tokens`,
  `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens_details.thinking_tokens`,
  `service_tier`, and a breakdown of ephemeral cache TTLs (`ephemeral_1h_input_tokens`,
  `ephemeral_5m_input_tokens`).
- `message.content` — an array of content blocks; observed block `type`s: `text`, `tool_use`,
  `tool_result`, `thinking` (implied by `thinking_tokens` field; text is also seen). Tool-use blocks
  carry `id`, `name`, `input`; the corresponding result comes back later as a `user`-type line with a
  `tool_result` block carrying the matching `tool_use_id`.
- `user` lines: `message.role`, `message.content` (text and/or `tool_result` blocks), `promptId`
  (groups the several transcript lines produced by one human prompt).
- `system` lines: `subtype`, `level`, `content`, sometimes `commandRun` (e.g. for slash-command
  execution) and `isMeta`.
- Git info is limited to `gitBranch` per line; no commit SHA is embedded in the transcript itself
  (git state would have to be reconstructed by the Collector by checking the repo at collection time,
  or is not available after the fact for a since-rewritten branch).

## 6. Tool calls, tool results, and large-output handling

- A tool call is a `tool_use` content block inside an `assistant` message (`id`, `name`, `input`).
- Its result is a `tool_result` content block inside a subsequent `user`-type line, matched by
  `tool_use_id`.
- Large tool outputs are **not always inlined**: confirmed on disk that a large `WebFetch` result was
  spilled to `projects/<project>/<session>/tool-results/<tool_use_id>.txt` with the transcript line
  instead holding a `<persisted-output>` marker and a pointer to that file. A Collector parsing only
  the `.jsonl` will therefore sometimes need to also read the sibling `tool-results/` directory to get
  full raw tool output — this is a real gap risk for a from-scratch parser.
- There is also a `wireToolInputs` field on `assistant` lines duplicating the raw tool input keyed by
  `tool_use_id` — appears to be the literal args as sent to the tool-execution layer (may differ
  subtly from what's redundantly shown in the `tool_use` block for display, e.g. escaping).

## 7. Sub-agents (Task tool / `Agent` tool) — representation, and a real format change

Two coexisting mechanisms observed:

1. **`isSidechain` flag.** Every line has `isSidechain: true/false`. This is the original/underlying
   sub-agent marker: a sidechain line belongs to a sub-agent's own reasoning thread rather than the
   main agent's.
2. **Separate per-agent files (current behavior, confirmed on disk today).** Sub-agent transcripts are
   *not* interleaved into the main `<session>.jsonl` at all in the current version. Instead:
   ```
   projects/<project>/<session>/subagents/agent-<agentId>.jsonl
   projects/<project>/<session>/subagents/agent-<agentId>.meta.json
   ```
   - Every line inside the sub-agent's own `.jsonl` still carries `isSidechain: true` and an added
     `agentId` field, plus its own `sessionId` (same as the parent) — but it's a **physically separate
     file**, not extra lines mixed into the parent transcript.
   - The `.meta.json` sidecar records how the sub-agent was spawned, e.g. (redacted, real shape):
     `agentType`, `description`, `model`, `spawnDepth`, `toolUseId` (the parent's `tool_use` id that
     spawned it — this is the join key back to the parent transcript), and, for worktree-isolated
     agents, `worktreePath`/`worktreeBranch`/`spawnedWithWorktree`.
   - The parent transcript's own `assistant` line still contains the ordinary `tool_use` block with
     `name: "Agent"` (or `"Task"`) and the prompt/config passed to the sub-agent; the sub-agent's
     *output* comes back as an ordinary `tool_result` in the parent, same as any other tool. The
     detailed transcript of what the sub-agent itself did (its own tool calls, thinking, etc.) is only
     in `subagents/agent-<id>.jsonl`.
   - `CHANGELOG.md` corroborates this is a real, evolved subsystem, not a fluke of this install:
     "Fixed session cleanup not removing the full session directory including subagent transcripts"
     (v2.1.98), "Fixed compaction writing duplicate multi-MB subagent transcript files on prompt-too-long
     retries" (v2.1.97), "Added token count, tool uses, and duration metrics to Task tool results"
     (older), "Fixed Remote Control sessions not streaming subagent transcripts," etc. — i.e. sub-agent
     transcripts as **separate files** have existed for a while and are actively maintained/fixed, but
     the `isSidechain` field itself long predates the separate-file layout and is kept for
     backward-compatible line-level identification.

**Takeaway for parity with other Sources**: don't assume "one Session = one file" for a Source that has
a sub-agent/Task concept — check for a sibling directory holding per-sub-agent transcripts, and a
metadata sidecar linking each one back to the specific parent `tool_use_id` that spawned it. The same
question is worth asking of Codex, oh-my-pi, and opencode explicitly: is a sub-agent invocation (a) a
sidechain-flagged block inline in the same file, (b) a wholly separate transcript file, or (c) not
persisted at all beyond its final tool result.

## 8. Local cleanup / retention behaviour

Fully documented (code.claude.com/docs/en/claude-directory, code.claude.com/docs/en/data-usage,
`settings-reference`), and independently confirmed by `.last-cleanup` and file-age evidence on this
machine:

- Setting: **`cleanupPeriodDays`** in `settings.json`. **Default 30 days, minimum 1; `0` is now
  rejected with a validation error** (this used to silently disable transcript persistence entirely —
  fixed per `CHANGELOG.md` at v2.1.89: "Changed `cleanupPeriodDays: 0`... to be rejected... it
  previously silently disabled transcript persistence").
- The setting has existed essentially since the beginning: `CHANGELOG.md` v0.2.117 — "Introduced
  settings.cleanupPeriodDays."
- Swept on this age basis: session transcripts (`<session>.jsonl`), subagent transcripts, tool-results,
  file-history checkpoints, plan files, debug logs, paste-cache, image-cache, uploads, shell-snapshots,
  backups (keeps 5 newest `.claude.json` backups regardless of age), feedback-bundles, usage-data,
  trash, and some legacy dirs (`todos/`, `statsig/`, `logs/`).
- **Not** swept by age: `history.jsonl` (the prompt-recall log), `stats-cache.json`, cached changelog,
  `remote-settings.json` (deleted on logout instead).
- A special carve-out: sessions "started or most recently continued in Claude Desktop or Cowork" are
  exempt from the standard sweep by default, capped separately by `desktopSessionCleanupPeriodDays`
  (added because desktop/Cowork sessions were disappearing after 30 days when users expected them to
  persist while still open in the app).
- On this machine: `~/.claude/.last-cleanup` = `2026-09-26T17:27:47.249Z` (i.e. the sweep runs roughly
  at each launch), and empirically the oldest transcript files present are from **2026-09-12** — 14
  days old on a 2026-09-26 "today," consistent with (well under) the 30-day default. No session file
  older than 30 days exists, which is exactly the expected effect of the sweep.
- **Data-usage-policy retention** (separate from the on-disk sweep, but related): consumer accounts
  get 5-year or 30-day server-side retention depending on model-training opt-in; commercial accounts
  get 30-day standard retention or Zero Data Retention if enabled. This is Anthropic-side handling of
  data that *was* transmitted (e.g. for inference), separate from what's cached locally in
  `~/.claude/projects/`.

**Implication for agent-history's Collector**: Claude Code will delete a Machine's local Session history
after `cleanupPeriodDays` (30 by default) with no separate opt-in required — a Collector that doesn't
poll/ship reasonably often (well under 30 days, and ideally much sooner given desktop/Cowork sessions
aside) will silently lose old Sessions it hasn't yet collected. This is a strong argument for the
Collector to run frequently (e.g. as a background service) rather than on a long cron.

## 9. Format stability across versions

- The **JSONL-file-per-session-under-`projects/<cwd-path>/`** layout, the **`cleanupPeriodDays`**
  setting, and **`CLAUDE_CONFIG_DIR`** are all old — first `CHANGELOG.md` evidence around
  v0.2.100–v1.0.6, i.e. present from very early in the tool's life and never fundamentally
  restructured since (only refined: e.g. `CLAUDE_CONFIG_DIR` handling was progressively made more
  consistent across surfaces like VS Code well into the v2.1.x series).
- One real early-history wrinkle: at v0.2.93 (`--continue`/`--resume` introduced) there is a
  `CHANGELOG.md` line at v0.2.100 saying "Made db storage optional; missing db support disables
  `--continue` and `--resume`" — implying some kind of local database (indexing, not necessarily the
  transcript content itself) was part of the resume mechanism very early on. No SQLite file exists on
  today's disk layout and no `.db` files were found under `~/.claude` on this machine, so if a DB was
  ever used for the transcript content itself, it was long since replaced by plain JSONL files — but
  this is the one area where an older version might legitimately have differed and is worth flagging
  as low-confidence/unverified rather than dismissing.
- The **sub-agent-as-separate-file** layout (`subagents/agent-<id>.jsonl` + `.meta.json`) is a more
  recent addition/evolution than the base transcript format — `CHANGELOG.md` shows active bug-fixing
  of this specific subsystem as late as the v2.1.9x–2.1.2xx range (session-cleanup-must-include-subagent-
  transcripts, compaction-duplicating-subagent-files, Remote-Control-not-streaming-subagent-transcripts).
  Treat the *existence* of per-sub-agent files, and the exact sidecar schema, as less stable than the
  core per-line schema (`type`/`uuid`/`parentUuid`/`sessionId`/`cwd`/`gitBranch`/`message`), which has
  been additive-only in every change observed (new optional fields added — `wireToolInputs`,
  `attributionSkill`, `context_management`, richer `usage.iterations` — never a field removed or
  renamed in the changelog entries reviewed).
- No changelog entry found that describes a breaking schema migration of existing `.jsonl` files (no
  "we now rewrite old transcripts to a new format" entry) — the model is additive/forward-compatible
  fields plus new adjacent files, not periodic schema versioning of already-written lines.

## 10. Redacted sample

Field names, structure, and general shape are real (taken from on-disk inspection); all UUIDs, paths,
timestamps, prompt/response text, and hashes below are fabricated/redacted placeholders.

```jsonl
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<REDACTED user prompt>"}]},"sessionId":"00000000-0000-4000-8000-000000000001","uuid":"11111111-1111-4111-8111-111111111111","parentUuid":null,"timestamp":"2026-09-26T12:00:00.000Z","cwd":"/Users/<user>/src/<project>","gitBranch":"main","version":"2.1.283","entrypoint":"cli","userType":"external","isSidechain":false,"promptId":"22222222-2222-4222-8222-222222222222"}
{"type":"assistant","message":{"model":"claude-opus-5","id":"msg_REDACTED0001","role":"assistant","content":[{"type":"tool_use","id":"toolu_REDACTED0001","name":"Bash","input":{"command":"ls -la","description":"List files"}}],"usage":{"input_tokens":12,"cache_read_input_tokens":4096,"cache_creation_input_tokens":0,"output_tokens":40,"output_tokens_details":{"thinking_tokens":0}}},"sessionId":"00000000-0000-4000-8000-000000000001","uuid":"33333333-3333-4333-8333-333333333333","parentUuid":"11111111-1111-4111-8111-111111111111","timestamp":"2026-09-26T12:00:02.500Z","cwd":"/Users/<user>/src/<project>","gitBranch":"main","version":"2.1.283","isSidechain":false,"effort":"high"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_REDACTED0001","content":"<REDACTED tool output>"}]},"sessionId":"00000000-0000-4000-8000-000000000001","uuid":"44444444-4444-4444-8444-444444444444","parentUuid":"33333333-3333-4333-8333-333333333333","timestamp":"2026-09-26T12:00:02.900Z","cwd":"/Users/<user>/src/<project>","gitBranch":"main","version":"2.1.283","isSidechain":false}
{"type":"assistant","message":{"model":"claude-opus-5","id":"msg_REDACTED0002","role":"assistant","content":[{"type":"tool_use","id":"toolu_REDACTED0002","name":"Agent","input":{"description":"Sub-task","subagent_type":"general-purpose","prompt":"<REDACTED sub-agent instructions>"}}],"usage":{"input_tokens":50,"output_tokens":80}},"sessionId":"00000000-0000-4000-8000-000000000001","uuid":"55555555-5555-4555-8555-555555555555","parentUuid":"44444444-4444-4444-8444-444444444444","timestamp":"2026-09-26T12:00:05.000Z","cwd":"/Users/<user>/src/<project>","gitBranch":"main","version":"2.1.283","isSidechain":false}
```

Sub-agent sidecar meta (`projects/<project>/<session>/subagents/agent-REDACTEDID.meta.json`):

```json
{
  "agentType": "general-purpose",
  "description": "Sub-task",
  "toolUseId": "toolu_REDACTED0002",
  "spawnDepth": 1,
  "model": "sonnet",
  "requestShape": "background",
  "requestNonInteractive": true
}
```

Sub-agent transcript line (`projects/<project>/<session>/subagents/agent-REDACTEDID.jsonl`):

```jsonl
{"type":"user","agentId":"REDACTEDID","isSidechain":true,"message":{"role":"user","content":[{"type":"text","text":"<REDACTED sub-agent instructions>"}]},"sessionId":"00000000-0000-4000-8000-000000000001","uuid":"66666666-6666-4666-8666-666666666666","parentUuid":null,"timestamp":"2026-09-26T12:00:05.100Z","cwd":"/Users/<user>/src/<project>","gitBranch":"main","version":"2.1.283"}
```

## 11. Notable/surprising findings (for other Source tickets)

- **Sub-agent transcripts live in physically separate sibling files**, not inline in the parent's
  `.jsonl`, in the current version — linked only by a `toolUseId` in a `.meta.json` sidecar plus a
  shared `agentId`/`sessionId`. This is a real design choice worth checking for each other Source:
  does it even distinguish a sub-agent invocation in its history at all, and if so, inline
  (sidechain-style) or as a separate artifact?
- **Large tool outputs can be off-loaded to sibling files** (`tool-results/<tool_use_id>.txt`) with
  only a placeholder in the JSONL line — a naive "just parse the `.jsonl`" Collector will silently
  under-capture some tool output unless it also globs the `tool-results/` directory.
- **Writes are batched per completed assistant turn, not streamed continuously** — so "tailing" a live
  session's file for near-real-time collection will see it update in bursts (whole turns appearing at
  once, including all of that turn's tool calls/results) rather than smoothly growing byte-by-byte.
- **Local retention is real and short by default (30 days) and applies to the transcript files
  themselves**, not just server-side data — a Collector that runs infrequently can lose Sessions
  permanently, this is a bigger operational risk than it might seem from reading only the
  privacy/data-usage docs (which mostly talk about server-side retention).
- **The `<project>` directory key is the *raw cwd path* with slashes replaced by dashes** — it changes
  if the same repo is opened from a different absolute path (e.g. a symlink, a different mount point,
  or — directly relevant to agent-history's own use of worktrees — each `git worktree` gets its own
  `<project>` directory, so one logical repository can be split across many `projects/<project>/`
  directories on the same Machine, each self-contained with its own `subagents/`, `memory/`, etc.
  Confirmed on disk in this very research task: the invoking session's project directory is
  `-Users-tedkulp-src-agent-history` (the main worktree), while this sub-agent's transcript file is
  filed as `subagents/agent-a278efb3d63c43b84.jsonl` *under that same parent project*, even though the
  sub-agent's own `cwd` is a different worktree path
  (`/Users/tedkulp/src/agent-history/.claude/worktrees/agent-a278efb3d63c43b84`) — i.e. sub-agent
  transcript filing follows the *parent session's* project directory, not the sub-agent's own cwd. Any
  Collector needs to decide how it wants to fold worktree-per-project layouts back into one logical
  project.
- **`isSidechain` predates the separate-subagent-file layout** and is still written even inside the
  now-separate per-agent files — a sign that the underlying data model has a longer history than the
  current on-disk layout, and that field-level additions (not removals/renames) are how the format has
  evolved. Good precedent to expect from the other Sources too: check for "vestigial" fields that hint
  at an older, different on-disk layout for the same logical concept.

## Sources

- code.claude.com/docs/en/claude-directory — `~/.claude` directory structure, cleanup/retention list,
  `CLAUDE_CONFIG_DIR`, scratchpad paths (fetched 2026-09-26; this page superseded
  docs.claude.com/en/docs/claude-code/settings via a 301 redirect during this research).
- code.claude.com/docs/en/data-usage — data training/retention policy, "stores session transcripts
  locally in plaintext under `~/.claude/projects/` for 30 days by default... Adjust the period with
  `cleanupPeriodDays`" (fetched 2026-09-26).
- code.claude.com/docs/en/settings-reference — `cleanupPeriodDays` description/category (fetched
  2026-09-26).
- `anthropics/claude-code` `CHANGELOG.md` (raw.githubusercontent.com/anthropics/claude-code/main/CHANGELOG.md,
  fetched 2026-09-26, 7640 lines, versions 0.2.21 through 2.1.283) — used for: `cleanupPeriodDays`
  introduced at v0.2.117; `CLAUDE_CONFIG_DIR` "respect everywhere" at v1.0.6; `--continue`/`--resume`
  introduced at v0.2.93 and made db-storage-optional at v0.2.100; multiple subagent-transcript-file
  bugfixes from v2.1.97 through v2.1.222+; `cleanupPeriodDays: 0` validation fix at v2.1.89;
  `desktopSessionCleanupPeriodDays` addition; Task tool history (mode deprecation, metrics added,
  permission-denial retry behavior).
- Local inspection of `~/.claude` on macOS, Claude Code version 2.1.283, on 2026-09-26: directory
  listing (`ls -la ~/.claude`, `find ~/.claude -maxdepth 3 -type d`), direct JSON parsing of multiple
  `projects/*/*.jsonl` files (session sizes from 7 to 149+ lines), the current session's own
  `projects/-Users-tedkulp-src-agent-history/<session>.jsonl` and its
  `<session>/subagents/agent-<id>.{jsonl,meta.json}` files (including this very research task's own
  transcript file, used to empirically test append-vs-rewrite behavior), `~/.claude/history.jsonl`,
  `~/.claude/.last-cleanup`, `~/.claude/backups/`, `~/.claude/settings.json`, and `~/.claude.json`.
