# oh-my-pi on-disk history format

Resolves GitHub issue [#4](https://github.com/tedkulp/agent-history/issues/4).

## What is oh-my-pi?

**oh-my-pi** ("omp") is a terminal coding agent built by Stencil Labs
(`can1357/oh-my-pi`, https://github.com/can1357/oh-my-pi). Its own docs
describe it as "a coding agent with the IDE wired in," and are explicit
about lineage:

> omp is a fork of pi-mono by Mario Zechner, rewritten as a coding-first
> surface: sessions, subagents, slash commands, extensions — all
> TypeScript, all MIT, all on GitHub.

So it is a substantial fork/rewrite of Mario Zechner's **pi** /
**pi-mono** (`badlogic/pi-mono`, https://github.com/badlogic/pi-mono),
not merely a themed distribution of it. It is a large Bun/TypeScript
monorepo (`packages/coding-agent`, `packages/ai`, `packages/tui`, …) with
a Rust native-helper layer (`crates/pi-*`: shell, PTY, native FS/VFS,
etc.). The CLI binary and config-directory name are both `omp`.

Confirmed against a live, in-use install on this machine at `~/.omp`
(the repo's own `dirs.ts` calls this exact layout out by name — see
below), so everything here is cross-checked against both the upstream
source (shallow-cloned from GitHub) and real on-disk data, not source
alone.

## Where it stores data

All paths are computed by `packages/utils/src/dirs.ts`, shared by macOS
and Linux (no separate Windows path scheme is described here).

- **Config root**: `~/.omp` (dir name literally `".omp"`, overridable via
  the `PI_CONFIG_DIR` env var — legacy `pi` naming survives as the env
  var prefix even though the app/brand is `omp`).
- **Agent dir**: `~/.omp/agent` (override: `PI_CODING_AGENT_DIR` env
  var). Named profiles live at `~/.omp/profiles/<name>/agent`, selected
  by `OMP_PROFILE` (canonical) or the legacy `PI_PROFILE` fallback.
- **Sessions dir** (the thing this ticket cares about):
  `~/.omp/agent/sessions/<encoded-cwd>/` — one subdirectory per project
  working directory, one JSONL file per Session inside it.
- **Blob store**: `~/.omp/agent/blobs` (content-addressed, sha256-named;
  large embedded images get externalized here rather than inflating the
  transcript).
- Assorted SQLite DBs live in `~/.omp/agent/*.db` (details below).

**Surprising finding — XDG applies on macOS too.** The module's own
doc-comment says "XDG is a Linux convention," and describes redirecting
to `$XDG_DATA_HOME/omp`, `$XDG_STATE_HOME/omp`, `$XDG_CACHE_HOME/omp`
only after `omp config init-xdg`/`migrate` has run. But the actual guard
in `DirResolver`'s constructor is:

```ts
if ((process.platform === "linux" || process.platform === "darwin") && isDefault) { ... }
```

i.e. the code checks **both** `linux` and `darwin`. On a default (no
named profile, no `PI_CODING_AGENT_DIR` override) install, setting
`XDG_DATA_HOME`/`XDG_STATE_HOME`/`XDG_CACHE_HOME` on **macOS** and having
already run the migration will just as validly redirect session storage
away from `~/.omp/agent/sessions` as it does on Linux. A collector that
only checks `~/.omp` on macOS could silently miss a redirected install.
No existence check is performed by omp itself if the env var is set and
the target dir exists — it trusts a prior migration happened.

Other relevant overrides: `OMP_WORKTREE_DIR` (worktree base),
`OMP_APP_NAME` (usage-attribution label for embedders), `OMP_GITHUB_CACHE_DB`
/`OMP_COMMIT_CACHE_DB`/`OMP_AUTH_BROKER_SNAPSHOT_CACHE` (cache DB
relocation, not relevant to sessions).

## File format: JSONL, not SQLite

The Session/Transcript **Raw record** is a plain UTF-8 JSONL file — one
JSON object per line, newline-terminated. Path:

```
~/.omp/agent/sessions/<encoded-cwd>/<ISO8601-timestamp>_<uuid>.jsonl
```

e.g. `2026-09-17T17-58-38-183Z_01a0b085-4167-7696-9b6e-1d8861a69862.jsonl`,
observed live in `~/.omp/agent/sessions/-src-<project>/`.

There are three SQLite databases under `~/.omp/agent/`, but **none of
them hold the transcript**:

| DB | Purpose |
|---|---|
| `history.db` | FTS5 index of past *prompt inputs* (readline-style recall) and session titles — not message content. |
| `agent.db` | Settings, OAuth/auth credentials, per-model perf stats, usage/billing counters, install/client tracking. |
| `models.db` | Cached model catalog metadata. |
| `stats.db` | Secondary, derived per-session stats (`messages`/`tool_calls`/`user_messages` row-per-entry, keyed by `session_file` path) rebuilt/reconciled from the JSONL — a query index, not a source of truth. |

The JSONL file is authoritative; the databases are all either unrelated
state or derived indexes that `omp gc` explicitly reconciles/deletes
rows from when a session is archived.

### Directory-name encoding (and how it identifies a cwd)

`cwd` is bucketed into a directory name by `session-paths.ts`:
- Inside `$HOME`: `-<home-relative-path-with-/-and-\-and-:-replaced-by-->`
  (e.g. cwd `~/src/foo` → dir `-src-foo`).
- Inside the OS temp root: `-tmp-<relative-path>`.
- Otherwise (outside both): legacy absolute form
  `--<absolute-path-with-separators-dashed>--`.

**Format churn even at the directory-layout level:** release 17.2.5–17.2.8
shipped a different, hashed directory-naming scheme
(`<home|tmp|abs>-<readable-basename>-<sha256-of-canonical-cwd>`), which
17.2.9 reverted — and the revert initially had no reverse migration,
stranding sessions written under the hashed scheme (their own issue
#7677). Current code carries **both** a legacy-absolute migrator and a
hashed-dir migrator that opportunistically merge old directories into
the canonical one on first access. This is concrete evidence the
on-disk layout is not stable across versions, independent of the JSONL
schema itself.

### Session identification

Each session has a `crypto.randomUUID()` `id` recorded in its header
`session` entry, which is also embedded in the filename
(`<timestamp>_<uuid>.jsonl`). A `parentSession` field (id or, for
sub-agents, an absolute path — see below) and a `previousSessionFiles`
array (recorded on session moves/renames) provide lineage.

### Entry schema

Every physical line (after an optional fixed-width title slot, see
below) is one of:

- **`session`** — the header: `id`, `version` (currently `3`), `cwd`,
  `additionalDirectories` (multi-root workspace support),
  `parentSession`, `previousSessionFiles`, `title`/`titleSource`,
  `timestamp`, `providerPromptCacheKey`.
- **`message`** — an `AgentMessage`: `role` (`user`/`assistant`/
  `toolResult`/`custom`/…), `content` blocks (`text`, `thinking`,
  `toolCall`, `image`, provider-native payloads), plus for assistant
  turns: `provider`, `model`, `api`, `usage` (input/output/cacheRead/
  cacheWrite/reasoningTokens + a cost breakdown), `stopReason`,
  `duration`/`ttft`, and a raw `providerPayload` (e.g. OpenAI Responses
  reasoning items, Anthropic signed thinking/text blocks) preserved
  byte-exact so replay/resume doesn't invalidate provider-side signature
  or encryption validation.
- **`model_change`**, **`thinking_level_change`**, **`service_tier_change`**
  — point-in-time settings changes, each its own entry.
- **`model_usage`** — token/cost accounting for non-conversation model
  calls (e.g. auto-title generation) that shouldn't pollute the
  transcript.
- **`compaction`**, **`branch_summary`**, **`reset_boundary`** — context
  management history (summarization, `/clear` boundaries).
- **`custom`** / **`custom_message`** — extension-defined entries (only
  the latter participates in LLM context).
- **`title_change`** — append-only audit trail of title edits (separate
  from the mutable title slot described next).
- **`credential_pin`** — sha256 hash (not raw identity) of which OAuth
  account served requests, for prompt-cache affinity on resume.
- **`session_init`** — sub-agent header, see below.
- **`ttsr_injection`**, **`mode_change`**, **`label`** — misc bookkeeping.

Every non-header entry carries `id` / `parentId` / `timestamp`, forming
a **parent-pointer tree**, not just an ordered list — this is what
enables branching (`/fork`, `/tan`) and was itself a schema migration
(see "Format stability" below).

Large text fields (>500,000 chars) are truncated in place with an
appended marker on persistence; embedded images ≥1KB base64 are
externalized to the blob store and replaced with a `blob:sha256:<hash>`
reference — both are content-preserving-enough compromises a
downstream parser needs to know about (a `[Session persistence
truncated large content]` suffix marks truncated strings).

### Tool calls / tool results

Represented as ordinary content inside `message` entries: `toolCall`
blocks (`id`, `name`, `arguments`, `intent`) inside an assistant
message's `content` array, and a corresponding `toolResult`-role
message (or `providerPayload`-carried result, depending on provider)
holding the output. No separate "event stream" — it's all folded into
the same message-entry schema as regular chat turns.

### Sub-agents: separate, sibling JSONL files

Sub-agent transcripts are **not** inline in the parent transcript. Each
sub-agent gets its own, structurally-identical session JSONL file
written to a sibling "artifacts directory" named after the parent file
(parent `<name>.jsonl` → directory `<name>/`), one file per subtask id:
`<artifactsDir>/<subtaskId>.jsonl` (confirmed in
`packages/coding-agent/src/task/executor.ts`: `subtaskSessionFile =
path.join(options.artifactsDir, `${id}.jsonl`)`). This matches what's on
disk: a parent session directory contained files like
`HerdrBoundaryResearch.jsonl` and `T3SwitchResearch.jsonl` — each one a
full session file for a named sub-agent run.

Each sub-agent file opens with a `session_init` entry (not `session`)
carrying: the full system prompt, initial task text, allowed tool
names, the agent definition name, semantic `modelRole` + resolved
model, whether it's read-only, output schema (for structured
sub-agents), spawn allowlist, and whether it ran inside an **isolation
worktree** (`isolated: true` → "never revivable, transcript-only after
park"). `omp gc`'s stats-cleanup code explicitly walks `parentSession`
chains (id-or-path) to reconcile/transfer derived rows between parent
and child sessions before deleting anything.

### Live-write behavior: append-only, with one deliberate exception

- Normal turns are appended with `O_APPEND`, one `write()` per line, no
  `fsync` (a crash can drop the last unflushed page but won't corrupt
  earlier lines; a failed partial write is rolled back with
  `ftruncate` to the pre-write size).
- The **session title** is the one field that's mutated in place without
  rewriting the file: it lives in a **fixed-width 256-byte padded slot**
  (`SessionTitleSlotEntry`, `type: "title"`, padded via a `pad` field of
  spaces) that a title update overwrites at its exact byte offset with
  `pwrite`-style `fs.writeSync(fd, buf, ..., offset)` — no shift of
  everything after it. This is exactly what's on disk: the physical
  first line of a session file is the `{"type":"title",...,"pad":"...
  "}` slot, and the logical `{"type":"session",...}` header is the
  *second* physical line.
- Full-history rewrites (compaction, pruning, "shake"/context-shrink,
  image-dropping) write a temp file and atomically `rename()` it into
  place, guarded by an **optimistic-concurrency precondition**
  (`expectedSize` must match the file's current byte length) and a
  **cross-process lock**: a lockfile (`.<name>.jsonl.lock`, contents
  `pid:timestamp`, stale-lock detection via `kill(pid, 0)`) plus a
  native OS-level file-lock sidecar (`.<name>.jsonl.lock.os`) that the
  kernel reclaims automatically even on `SIGKILL`. A leftover
  `.jsonl.lock.os` sidecar file was directly observed on disk next to a
  live session file, confirming this is exercised in normal use, not
  just theoretical.
- A crash mid-rewrite on a platform where `rename()` over an open handle
  fails (Windows `EPERM`-style semantics) falls back to move-aside +
  replace + rollback-on-failure, leaving `.jsonl.<snowflake>.bak`
  backup files that a later session listing/delete pass has explicit
  logic to clean up (`#11499`).

### Metadata captured

- **cwd** and **additional workspace directories** in the header.
- **Model/provider/api** on every assistant turn and on dedicated
  `model_change` entries (role like `default`/`smol`/`slow`, and whether
  a fallback model was resolved).
- **Token usage & cost**: input/output/cacheRead/cacheWrite/
  reasoningTokens/total, plus a per-category `cost` object, on every
  assistant message; separately tracked for non-chat model calls via
  `model_usage` entries.
- **Timestamps**: ISO8601 `timestamp` on every entry, plus an epoch-ms
  `timestamp` inside the message payload itself.
- **Thinking level / service tier** changes as their own entry types.
- **OAuth account identity**: only a sha256 hash of account+scope (not
  raw email/uuid) is persisted, for prompt-cache affinity on resume —
  the code comment is explicit that this makes exported sessions
  "pseudonymous, not anonymous," since an unsalted hash of a guessable
  email is still linkable.
- **No explicit git-remote/branch metadata field** was found anywhere in
  the session entry schema (`session-entries.ts`). If agent-history
  wants git provenance (repo/branch) for an oh-my-pi Session, it will
  most likely have to be inferred from `cwd` at collection time, not
  read off the transcript.

## Retention / cleanup

There is **no automatic background deletion**. Cleanup is entirely
opt-in via the `omp gc` CLI subcommand
(`packages/coding-agent/src/cli/gc-cli.ts`), gated by settings that
default to *enabled* but only take effect when someone actually runs
the command:

- `gc.archive` (default `true`), `gc.blobs` (default `true`), `gc.wal`
  (default `true`).
- `gc.retainNewestGlobal` = 20, `gc.retainNewestPerCwd` = 10,
  `gc.coldArchiveAfterDays` = 30.

Policy: never touches a session with an "active" status (`pending`,
`interrupted`, `unknown`) or one modified within the last 5 minutes;
otherwise, after keeping the 20 most-recent sessions globally and 10
more per project beyond that, anything older than 30 days gets
gzip-archived (not deleted) to
`~/.omp/agent/archive/sessions/**/*.jsonl.gz`, sibling artifacts
directories moved alongside it. Archiving also deletes the
now-redundant rows from `history.db` and reconciles/deletes rows from
`stats.db` (with lineage-aware transfer to any still-retained
parent/child sessions first). Unreferenced content-addressed blobs are
deleted (with a 5-minute grace window) once nothing in any session
references their hash anymore.

**Implication for a Collector**: a Session's JSONL file can (a) get
gzip-renamed to a different path under `archive/sessions/` after 30
days idle, and (b) never disappears outright unless a human explicitly
runs `omp gc --apply` — but only if that flag is set, since the default
without `--apply` is a dry-run report.

## Format stability across versions

The schema **is versioned** (`session.version`, currently `3`) and
migrated in place on load
(`packages/coding-agent/src/session/session-migrations.ts`):

- **v1 → v2**: added the `id`/`parentId` tree structure to every entry
  (before this, position in the file *was* the only structure) and
  converted `compaction.firstKeptEntryIndex` (an array index) to
  `compaction.firstKeptEntryId` (a stable id) — i.e. even the meaning of
  "where does compacted history resume" changed representation.
- **v2 → v3**: renamed the `hookMessage` message role to `custom`.

Combined with the directory-naming churn described above (hashed dirs
shipped and reverted within three patch releases, 17.2.5→17.2.9), the
practical takeaway is: **treat the format as versioned and still
moving**, always branch on `session.version` rather than assuming a
field's presence/shape, and expect the loader-level migration functions
(not the raw bytes) to be the authoritative definition of "current
schema" going forward.

## Minimal redacted sample

First three physical lines of a real session file on this machine
(title/cwd/ids redacted; the title slot's `pad` field truncated for
readability):

```json
{"type":"title","v":1,"title":"<REDACTED_TITLE>","source":"auto","updatedAt":"2026-09-17T18:13:24.506Z","pad":"                    "}
{"type":"session","version":3,"id":"<REDACTED_UUID>","timestamp":"2026-09-17T17:58:38.183Z","cwd":"<REDACTED_PATH>","title":"<REDACTED_TITLE>","titleSource":"auto"}
{"type":"model_change","id":"e2d0263c","parentId":null,"timestamp":"2026-09-17T17:58:38.203Z","model":"openai-codex/gpt-5.6-sol","resolvedModelIsFallback":false}
```

A short user turn later in the same file:

```json
{"type":"message","id":"41b5ce7a","parentId":"25a6a097","timestamp":"2026-09-17T18:13:05.756Z","message":{"role":"user","content":[{"type":"text","text":"use the recommendations"}],"attribution":"user","timestamp":1789668785722}}
```

## Sources

- Repo: https://github.com/can1357/oh-my-pi (shallow-cloned for this
  research; primary source for every claim above unless noted
  otherwise)
  - `packages/utils/src/dirs.ts` — all path/env-override resolution
  - `packages/coding-agent/src/session/session-paths.ts` — cwd→dirname
    encoding and migration history
  - `packages/coding-agent/src/session/session-storage.ts` — append vs.
    atomic-rewrite mechanics, cross-process locking
  - `packages/coding-agent/src/session/session-entries.ts` — entry
    schema, `CURRENT_SESSION_VERSION`
  - `packages/coding-agent/src/session/session-migrations.ts` — v1→v2→v3
    migrations
  - `packages/coding-agent/src/session/session-persistence.ts` —
    truncation/blob-externalization on persist
  - `packages/coding-agent/src/cli/gc-cli.ts`,
    `packages/coding-agent/src/cli/gc-settings.ts` — retention/archival
  - `packages/coding-agent/src/task/executor.ts` — sub-agent session
    file placement
- Upstream (fork origin): https://github.com/badlogic/pi-mono (Mario
  Zechner's pi/pi-mono)
- Live install on this machine: `~/.omp/agent/{sessions,agent.db,
  history.db,models.db,config.yml}` — used only to confirm the source
  matches real on-disk behavior; no non-redacted content is reproduced
  above.
