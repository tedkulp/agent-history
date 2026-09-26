# opencode on-disk history format

Research for GitHub issue #5 ("opencode on-disk history format"). Covers where and how
opencode (the **Source**) stores Session history on disk on macOS and Linux, both today
and historically.

All repo-path citations pin to commit
[`a42f393`](https://github.com/anomalyco/opencode/commit/a42f393c850bec0c0f395fb91bf19b1ee8b31666)
(HEAD of `dev` as of 2026-09-26, shallow-cloned for this research), since opencode does not
tag every commit and moves fast. Paths are stable relative to the repo root.

## Summary

opencode currently (since roughly **v1.1.2x, mid-January 2026**, stabilizing through
mid-February 2026) stores all Session history in a single **SQLite database** at
`$XDG_DATA_HOME/opencode/opencode.db` (WAL mode). Before that, from opencode's early
releases through late 2025 (last seen locally at v0.4.45), it stored history as a **tree
of individual JSON files**, one file per session/message/part, under
`$XDG_DATA_HOME/opencode/storage/`. The JSON-to-SQLite migration is a real, historically
rocky, semi-automatic process (it ran automatically on first launch of a SQLite-capable
build, with bug reports of it silently skipping, losing sessions, or double-running); a
manual `opencode db migrate` command was later added as a fallback. Because opencode does
not delete the legacy JSON tree after migrating, a machine that has been running opencode
since 2025 can have **both** an old `storage/` JSON tree and a current `opencode.db`
SQLite file, and the file layout of "the Source's raw data" has fundamentally changed
shape at least once.

## 1. Locations and overrides

- Data directory: opencode uses the `xdg-basedir` npm package to resolve
  `xdgData`, and joins `"opencode"` onto it —
  [`packages/core/src/global.ts:3,10-11`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/global.ts#L3-L11).
  This resolves to `$XDG_DATA_HOME/opencode`, defaulting to `~/.local/share/opencode`
  **on both macOS and Linux** — opencode does not use macOS's
  `~/Library/Application Support` convention anywhere in the codebase (confirmed by
  grepping the whole repo for `Library/Application Support`: no hits outside the
  unrelated Electron desktop app's own auto-update cache).
- Config directory can be overridden independently via `OPENCODE_CONFIG_DIR`:
  `config: Flag.OPENCODE_CONFIG_DIR ?? Path.config` —
  [`packages/core/src/global.ts:64`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/global.ts#L64).
  This does **not** move the data/session directory, only config.
  `OPENCODE_TEST_HOME` overrides `os.homedir()` for tests
  ([`global.ts:19`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/global.ts#L19)).
- Database file path/name resolution, including override:
  [`packages/core/src/database/database.ts:41-52`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/database/database.ts#L41-L52).
  ```ts
  export function path() {
    if (Flag.OPENCODE_DB) {
      if (Flag.OPENCODE_DB === ":memory:" || isAbsolute(Flag.OPENCODE_DB)) return Flag.OPENCODE_DB
      return join(Global.Path.data, Flag.OPENCODE_DB)
    }
    if (["latest", "beta", "prod"].includes(InstallationChannel) || OPENCODE_DISABLE_CHANNEL_DB)
      return join(Global.Path.data, "opencode.db")
    return join(Global.Path.data, `opencode-${InstallationChannel...}.db`)
  }
  ```
  So: default is `<data>/opencode.db`; `OPENCODE_DB` env var can force an absolute path or
  `:memory:`; non-stable install channels (e.g. a dev/nightly build) get their own
  `opencode-<channel>.db` sibling file unless `OPENCODE_DISABLE_CHANNEL_DB` is set. There is
  also `OPENCODE_SQLITE` referenced by the desktop app for remote DB access (PR title
  "desktop: remote OPENCODE_SQLITE env", #13545) — not chased further, low relevance to a
  Collector reading a local machine.
- Legacy JSON storage root: `<data>/storage/` (see §2), read/written by
  [`packages/opencode/src/storage/storage.ts`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts).
- **Observed locally, not yet in upstream docs:** on the inspected macOS machine (mise-installed
  opencode v1.18.32), the resolved data dir is `~/.local/share/opencode/`, with
  `opencode.db` (41MB) plus `-wal`/`-shm` sidecars, and a leftover `storage/` JSON tree
  from August–October 2025 that was never deleted after migration.

## 2. Format history / format stability across versions

Two eras, confirmed from source:

**Era 1 — per-file JSON ("legacy" storage), pre-2026.** Implemented by
[`packages/opencode/src/storage/storage.ts`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts),
a generic key-value JSON-file store: `file(dir, key) = path.join(dir, ...key) + ".json"`
([storage.ts:66-68](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts#L66-L68)).
This module also embeds its own numbered migration list (`MIGRATIONS: Migration[]`,
[storage.ts:81](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts#L81)),
which is itself evidence the JSON layout changed shape *at least twice* even within the
JSON era:
  - An older, more deeply nested layout under a project folder:
    `storage/session/info/*.json`, `storage/session/message/<sessionID>/*.json`,
    `storage/session/part/<sessionID>/<messageID>/*.json`
    ([storage.ts:96-172](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts#L96-L172),
    migration `Storage.migration.1`).
  - A flatter layout that migration 1 rewrites into: `session/<projectID>/<id>.json`,
    `message/<sessionID>/<id>.json`, `part/<messageID>/<id>.json` — this matches what was
    observed locally (`storage/session/<project-hash>/ses_<id>.json`,
    `storage/message/ses_<id>/msg_<id>.json`, `storage/part/msg_<id>/prt_<id>.json`).
  - A further migration (`Storage.migration.2`,
    [storage.ts:174-196](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts#L174-L196))
    splits a `summary.diffs` field out into a separate `session_diff/<id>.json` file.
  - The bare `storage/migration` marker file observed locally (containing just a version
    number) is written/read by `parseMigration()`
    ([storage.ts:76-79](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts#L76-L79))
    to track which of these in-place JSON migrations have already run — it is **not**
    related to the later SQLite migration, which is a separate mechanism.

**Era 2 — SQLite, from ~January 2026 onward.** GitHub issue/PR history pins the rollout
precisely:
  - Issue [#7840](https://github.com/anomalyco/opencode/issues/7840) "sqlite vs embedded
    postgres" (2026-01-11) — design discussion kicks off.
  - Issue [#8586](https://github.com/anomalyco/opencode/pull/8586) "move to sqlite"
    (2026-01-15, around release **v1.1.23**).
  - PR [#9012](https://github.com/anomalyco/opencode/pull/9012) "feat: refactor storage
    into storage interface, add sqlite storage provider" (2026-01-17) — proposes
    `StorageProvider` interface with `JsonStorageProvider` and `SqliteStorageProvider`,
    plus `opencode sqlite init`/`opencode sqlite import` migration commands.
  - PR [#9268](https://github.com/anomalyco/opencode/pull/9268) "fix(Sqlite): ... moved
    Session import to `migrateFromGlobal`" (2026-01-18, around **v1.1.26**) — author
    confirms in the PR description "the migration ran and local DB created" using Drizzle
    auto-generated migrations.
  - A rocky stabilization period through February 2026, with real user-facing bugs that
    are directly relevant to Collector design: issue
    [#13560](https://github.com/anomalyco/opencode/issues/13560) "Sessions not appearing",
    [#13611](https://github.com/anomalyco/opencode/issues/13611)/[#13612](https://github.com/anomalyco/opencode/pull/13612)
    "should not run session JSON to sqlite migration when started with `opencode serve`",
    [#13636](https://github.com/anomalyco/opencode/issues/13636) "SQLite Migration Ate My
    Sessions!", [#13654](https://github.com/anomalyco/opencode/issues/13654) "JSON→SQLite
    session migration silently skips for incremental upgrades",
    [#13818](https://github.com/anomalyco/opencode/issues/13818) "No message parts after
    sqlite migration".
  - PR [#13874](https://github.com/anomalyco/opencode/pull/13874) "feat(cli): add db
    migrate command for JSON to SQLite migration" (merged 2026-02-16) adds a manual
    `opencode db migrate` escape hatch that merges via `onConflictDoNothing()` when the
    automatic migration didn't run or was incomplete.
  - By the currently-installed **v1.18.32** (2026-09-21), SQLite is the sole,
    long-stable backend; the legacy `JsonStorageProvider` path still exists in the
    codebase only as the source side of the one-time migration, not as an ongoing
    read/write path.

**Within the SQLite era, the schema itself keeps evolving via Drizzle migrations** — the
`migration` table in the DB records every schema change
([`packages/core/src/database/migration.ts:17-38`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/database/migration.ts#L17-L38)
creates and seeds it), matching the dozens of timestamped migration names observed
locally (e.g. `20260622170816_reset_v2_session_state`). **Net: neither era has had a
frozen, stable on-disk shape — the SQLite table schema has continued to change via
migrations for 8+ months, and it's normal for a machine's DB to be mid-way through many
schema versions.**

## 3. How a Session is identified

- Session ID format: `ses_<26-char id>` — e.g. schema
  `packages/schema/src/session-v1.ts` re-exports `packages/schema/src/v1/session.ts`;
  message/part IDs use the same generator
  ([`MessageID`/`PartID`, v1/session.ts:19-27](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/v1/session.ts#L19-L27)):
  `msg_` + `ascending()`, `prt_` + `ascending()` (a K-sortable ID generator, hence the
  observed base62-ish ascending-sortable IDs).
- The current (v2) `Session.Info` schema's primary key fields:
  [`packages/schema/src/session.ts:16-40`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/session.ts#L16-L40) —
  `id`, `parentID` (optional), `projectID`, `agent`, `model`, `cost`, `tokens`, `time`,
  `title`, `location` (directory + workspaceID), `subpath`, `revert`.
- In SQLite, a session is one row of the `session` table keyed by `id`, scoped further by
  `project_id` and `workspace_id`. Row-to-schema mapping is explicit in
  [`packages/core/src/session/info.ts:15-40`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/session/info.ts#L15-L40),
  which confirms the exact column set observed locally (`project_id`, `parent_id`,
  `agent`, `model` (JSON), `cost`, `tokens_input/output/reasoning/cache_read/cache_write`,
  `directory`, `workspace_id`).
- A `project` is identified by a hash-like `ProjectID`, and in the legacy migration code
  the project ID is derived from the repo's root commit: `git rev-list --max-parents=0
  --all`, sorted, first ID taken —
  [`storage.ts:104-115`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/storage/storage.ts#L104-L115).
  This explains the "hex hash" project IDs observed locally: it's derived from the git
  repo's first commit SHA, not simply a hash of the worktree path (for git projects;
  non-git projects presumably get some other ID — not chased further).

## 4. Appended vs. rewritten while a Session is live

- **Legacy JSON era:** each entity (`session/info`, one `message`, one `part`) is its own
  file; a live session appends new files (new message/part JSON files) and rewrites the
  single `session/info/<id>.json` file on each update (title changes, token/cost
  accumulation, `time.updated`). Individual message/part files, once written, are
  effectively immutable except for in-place tool-state transitions
  (pending → running → completed/error, see §6) which rewrite that one part's file.
- **SQLite era:** the `session` row is updated in place (UPDATE, not append) as
  cost/tokens/title/time change. Messages and parts are each their own row
  (`message`, `part` tables), inserted once and then updated in place as content streams
  in (e.g. a `part` row for a `tool` call transitions through `ToolState` values as the
  tool call runs — see
  [`packages/schema/src/v1/session.ts:304-323`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/v1/session.ts#L304-L323)).
  Additionally there is a `session_message`/event-log table
  (observed locally with `type`, `seq` columns) that looks append-only/event-sourced —
  consistent with `packages/schema/src/session-message.ts`'s event types like
  `agent-switched`, `model-switched` (each event has `time.created` but no `updated`,
  [session-message.ts:24-40](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/session-message.ts#L24-L40)).
  The DB is opened by the opencode process itself in WAL mode with explicit pragmas —
  [`database.ts:26-30`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/database/database.ts#L26-L30):
  `PRAGMA journal_mode = WAL`, `synchronous = NORMAL`, `busy_timeout = 5000`,
  `cache_size = -64000`, `foreign_keys = ON`, plus a passive WAL checkpoint on open. So
  while opencode is running, the `.db` file itself may lag behind the `-wal` file — the
  `-wal`/`-shm` sidecars observed locally are exactly this.

## 5. Metadata captured

- **Session-level** (`Session.Info`,
  [session.ts:16-40](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/session.ts#L16-L40)):
  `projectID`, `agent` (agent/mode used), `model` (`{id, providerID, variant}`), `cost`
  (total $ cost, `Schema.Finite`), `tokens.{input, output, reasoning, cache.{read,
  write}}`, `time.{created, updated, archived?}`, `title`, `location.{directory,
  workspaceID}`, `subpath`, `revert` state, `parentID`.
- **Project-level** (`Project.Info`,
  [project.ts:29-39](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/project.ts#L29-L39)):
  `worktree` (the working directory / cwd of the project), `vcs` (currently only literal
  `"git"` is supported —
  [project.ts:10](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/project.ts#L10)),
  `name`, `icon`, `commands.start`, `time.{created, updated, initialized}`, `sandboxes`
  (array). This confirms the `project` table's `worktree`/`vcs` columns capture exactly
  "the cwd this session ran in" and "is this a git repo", not deeper git metadata like
  branch/remote/commit SHA of the session itself (no such fields found in `Project.Info`
  or `Session.Info`).
- **Token/cost tracking** is per-session cumulative totals (not per-message in the
  `session` table), broken out by input/output/reasoning/cache-read/cache-write — this
  matches the columns observed locally exactly.
- No per-session git branch/commit/remote capture was found in the schema; the "git info"
  captured is limited to project identity (root-commit-derived project ID at migration
  time) and `vcs: "git"` marker, plus a separate snapshot/patch subsystem
  (`packages/core/src/session/*` "snapshot"/"patch" parts,
  [v1/session.ts:88-100](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/v1/session.ts#L88-L100))
  that stores file diffs per message, not repo-level git state.

## 6. Tool calls / results and sub-agent representation

- **Tool calls** are a `part` row/file with `type: "tool"`:
  `ToolPart = { ...partBase, type: "tool", callID, tool, state: ToolState, metadata? }`
  ([v1/session.ts:315-322](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/v1/session.ts#L315-L322)).
  `ToolState` is a discriminated union `ToolStatePending | ToolStateRunning |
  ToolStateCompleted | ToolStateError`
  ([v1/session.ts:304-313](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/v1/session.ts#L304-L313)),
  so a single `part` row is mutated in place as the tool call progresses, rather than
  separate "call" and "result" records. Other part types confirmed in the same file:
  `text`, `reasoning`, `snapshot`, `patch`, plus file-source references (`file`,
  `symbol`) for `@`-mentions.
- **Sub-agents** are represented as ordinary Sessions with a `parentID` pointing at the
  invoking session:
  `Session.Info.parentID` ([session.ts:21](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/session.ts#L21)),
  stored as `parent_id` in the `session` table
  ([info.ts:19](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/session/info.ts#L19),
  [projector.ts:48](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/core/src/session/projector.ts#L48)).
  The `agent` field on a session references an `Agent.ID`, and agents themselves declare
  a `mode: "subagent" | "primary" | "all"`
  ([`packages/schema/src/agent.ts:26`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/schema/src/agent.ts#L26)) —
  this is the source of the "(@general subagent)"/"(@explore subagent)" session titles
  observed locally: a sub-agent invocation creates a **child Session** (own row, own
  messages/parts, own token/cost totals) whose `parent_id` links it back to the
  originating session, and whose `agent` names which subagent config ran it. There is no
  separate "sub-agent call" part type distinct from a session hierarchy — the tool call
  that *launches* the subagent is presumably an ordinary `ToolPart` in the parent session
  (e.g. a `task`/`agent` tool), and the subagent's own turn-by-turn work lives entirely in
  the child session's own message/part rows. (Not independently confirmed which specific
  tool name triggers this — out of scope for this pass.)

## 7. Retention / cleanup behavior

No automatic session pruning, TTL, or expiry was found for session/message/part data
itself. What exists:

- Manual deletion only, via HTTP API handlers:
  `session.remove` (DELETE a whole session),
  `session.removeMessage`, `session.removePart`
  ([`packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts:178-179,380-393`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts#L178-L179)).
- A `time.archived` field can be set via the API
  ([`session.ts:200-201`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts#L200-L201))
  — this is a soft "archived" marker, not a deletion, and requires explicit user/API
  action, not a background job.
- The only automatic "prune" found in the codebase is unrelated to session history: the
  internal git-based file-snapshot subsystem runs `git gc --prune=7.days` on its own
  shadow git repo used for file-change tracking/undo
  ([`packages/opencode/src/snapshot/index.ts:23,300-313`](https://github.com/anomalyco/opencode/blob/a42f393c850bec0c0f395fb91bf19b1ee8b31666/packages/opencode/src/snapshot/index.ts#L23-L313)).
  This does not touch the `session`/`message`/`part` tables.
- **Practical implication:** left alone, opencode's session history grows unboundedly
  (consistent with the 41MB DB observed locally after ~months of use, plus a
  never-cleaned-up legacy JSON tree). A Collector does not need to race a cleanup job, but
  should expect the DB to only grow, and should expect leftover legacy JSON files to
  remain indefinitely on machines that predate the SQLite cutover.

## Mapping to agent-history vocabulary

- opencode is a **Source**.
- Each `session` table row (SQLite era) or each `storage/session/**/ses_*.json` file
  (legacy era) is one **Session**.
- The SQLite database file (`opencode.db` + its `-wal`/`-shm` sidecars) — or, for
  machines that haven't upgraded past ~early 2026, the `storage/` JSON file tree — is the
  **Raw record** for opencode Sessions on that Machine.
- **Collector implication (the important one):** since v1.x, opencode's Raw record is not
  a stable set of per-Session files that a Collector can simply enumerate, stat, and copy.
  It is rows inside a single live SQLite database that the opencode process itself has
  open in WAL mode. A Collector for opencode cannot use a naive "watch this directory /
  tail this file per Session" strategy the way it plausibly could for a JSON-file-per-session
  Source. It needs one of:
  - a **read-only, concurrent SQLite connection** against `opencode.db` (SQLite's WAL mode
    is designed to support this: readers don't block the writer and vice versa, but the
    Collector must still handle `SQLITE_BUSY`/lock contention gracefully, must not assume
    the on-disk `.db` file alone is current — it must also read `-wal` — and must track a
    session/message/part **schema version** so a Drizzle migration on the opencode side
    doesn't silently break field mapping), or
  - a periodic **snapshot/copy** of the `.db`+`-wal`+`-shm` triple (SQLite has an online
    backup API; a raw `cp` of a live WAL-mode DB while it's being written is not
    guaranteed consistent without also copying/checkpointing the WAL), or
  - going through opencode's own HTTP API (`session.list`/`session.get`/etc., the same
    handlers cited in §6-7) instead of touching the file/DB layer at all, trading raw-file
    fidelity for a stable, versioned interface.
  - Separately, the Collector should special-case machines where the legacy `storage/`
    JSON tree still exists on disk (common, since opencode does not delete it after
    migrating) — those files are a second, independent Raw record source for older
    Sessions that may predate anything in the current `opencode.db`.

## Redacted samples

`session` table shape (columns as inspected locally; types elided):

```
sqlite> .schema session
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  project_id TEXT,
  workspace_id TEXT,
  parent_id TEXT,
  slug TEXT,
  directory TEXT,
  path TEXT,
  title TEXT,
  version TEXT,
  ...
  cost REAL,
  tokens_input INTEGER, tokens_output INTEGER, tokens_reasoning INTEGER,
  tokens_cache_read INTEGER, tokens_cache_write INTEGER,
  agent TEXT, model TEXT,
  time_created INTEGER, time_updated INTEGER,
  time_compacting INTEGER, time_archived INTEGER
);
```

Legacy per-file JSON sample (`storage/session/<project-hash>/ses_<id>.json`), redacted:

```json
{
  "id": "ses_REDACTEDID000000000000000",
  "version": "0.4.45",
  "title": "REDACTED session title",
  "time": { "created": 1755177596535, "updated": 1755177600532 }
}
```

## Note for sibling Source tickets (Claude Code, Codex, oh-my-pi)

The single biggest surprise here, worth flagging on any sibling "on-disk format" ticket:
**"on-disk format" is not guaranteed to mean "a directory of files."** opencode is proof
that a Source can migrate its history storage from discrete per-Session files to a single
live SQLite database mid-lifecycle, and can do so imperfectly (data loss during migration
was a real, reported bug upstream). Before assuming a Collector can simply glob/stat/copy
files per Session for any given Source, confirm:
1. whether that Source's format has ever changed shape across its version history, and
2. whether the current format is a live database (needing WAL-aware read/snapshot
   handling and lock-contention avoidance) versus genuinely static files.
Any Source using SQLite (or another embedded DB) needs its own read strategy —
either a schema-versioned read-only query path, an online-backup-style snapshot, or use
of the Source's own API — rather than the file-copy approach that likely works fine for a
pure-JSON-file Source.

## What's observed-locally vs. primary-source

Everything under headings 1–7 and the Mapping section above cites a specific
`anomalyco/opencode` source file/line as primary evidence, except where explicitly marked
"Observed locally, not yet in upstream docs" (the exact local data-dir size/mtimes, the
specific local version 1.18.32, and the specific local table/column names as directly
`.schema`'d — which do line up with, and are corroborated by, the source-level schema
definitions cited above). The version/date timeline in §2 (issue/PR numbers and dates) is
from GitHub's own issue/PR metadata (`gh api search/issues`, `gh pr view`), i.e. primary
source, not local inference.
