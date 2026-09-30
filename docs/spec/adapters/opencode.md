# Adapter spec: opencode

## 1. Purpose and scope

opencode is the last Source in the build order. This spec defines, for the Source identifier `opencode`:

- the Collector adapter: root and database resolution, **two Layouts**, Record keys, the per-Session database export, starting-cwd reads for `exclude`
- the Hub parser: `MapKey`, Layout ranks, and how both Layouts become a Transcript

opencode has stored history two ways:

- **`legacy-json`** (through late 2025): a tree of JSON files under `storage/`, one file per Session, Message and part.
- **`sqlite`** (from early 2026): one live SQLite database, `opencode.db`, in WAL mode, with `session`, `message` and `part` tables.

opencode never deletes the legacy tree after migrating, so a Machine can have both. The same Session can appear in both; the database wins.

The Collector never parses either Layout ([ADR 0001](../../adr/0001-hub-parses-collector-ships-raw.md)). For the database it exports each Session's rows verbatim as JSONL. The adapter interfaces are in [`collector.md`](../collector.md) §2.6 and [`hub.md`](../hub.md) §2.5.

Evidence: [`docs/research/opencode-format.md`](../../research/opencode-format.md), plus a local inspection of opencode 1.18.31 data made while writing this spec (table schemas, `data` JSON keys, part types, legacy file keys, the `task` tool's metadata).

Source: [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Spec assembly](https://github.com/tedkulp/agent-history/issues/18)

## 2. Layouts

### 2.1 Root and database

| | |
|---|---|
| Default root | `$XDG_DATA_HOME/opencode`, else `~/.local/share/opencode`, on macOS and Linux alike |
| Database | `$OPENCODE_DB` if set (absolute, or relative to the root); else `<root>/opencode.db` |
| `Detect(root)` | `root/storage` is a directory or the database file exists |
| `Version(root)` | `session.version` of the most recently updated Session row, read with the database query below; else empty |
| Layouts present | `legacy-json` when `storage/session/` is a directory; `sqlite` when the database exists |

- When `init` sees `OPENCODE_DB` in the shell environment, it writes the resolved absolute path to config as `sources.opencode.db` (`collector.md` §2.3).
- Channel databases (`opencode-<channel>.db`, from non-stable builds) are known-ignored in v1.

Source: [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5), [Collector design](https://github.com/tedkulp/agent-history/issues/11); the `db` config key filled in while writing this spec

### 2.2 The `sqlite` Layout

Rank **`2`**. Record-key prefix **`db:`**.

**Record key**: `db:<session id>`, e.g. `db:ses_f4dce8052ffeNGi3JJ0bmIeMHn`. One Raw record per `session` row, Child Sessions included.

**Content**: the Session's rows as JSONL, one row per line:

```jsonl
{"table":"session","row":{"id":"ses_…","project_id":"…","parent_id":null,"directory":"/Users/ted/src/app","title":"…","version":"1.18.31",…,"time_created":1789695980000,"time_updated":1789696158981}}
{"table":"message","row":{"id":"msg_…","session_id":"ses_…","time_created":…,"time_updated":…,"data":"{\"role\":\"user\",…}"}}
{"table":"part","row":{"id":"prt_…","message_id":"msg_…","session_id":"ses_…","time_created":…,"time_updated":…,"data":"{\"type\":\"text\",…}"}}
```

- Lines, in order: the `session` row; its `message` rows ordered by `(time_created, id)`; its `part` rows ordered by `id`; its `session_message` rows ordered by `seq`, if that table exists.
- `row` holds **every column** (`SELECT *`), keyed by column name, in table order. TEXT → JSON string (the `data` columns stay strings, verbatim), INTEGER and REAL → JSON number, NULL → `null`, BLOB → `{"$base64": "…"}`.
- Each line ends with `\n`. The export is deterministic, so an unchanged Session exports to identical bytes and the Hub's no-op `replace` rule applies (`protocol.md` §4.4).
- New columns added by opencode's migrations flow through untouched. A missing `session_message` table is skipped. A missing `session`, `message` or `part` table fails the Layout's discovery, and `status` shows the error.

**Finding changed Sessions.** Each record's change signal (`collector.md` §3.2) is the number of the Session's rows and the latest `time_updated` among the Session and its `message`, `part` and `session_message` rows. One query lists every Session with its signal; a Session whose signal differs from the one last acked is exported. The row count catches deleted rows, which change no `time_updated`.

**Reading the database**: read-only, never creating, changing or checkpointing the WAL, with a `busy_timeout` of 5 s. SQLite's `mode=ro` still creates a missing WAL, so the Collector uses it only while the WAL exists (opencode has the database open, or left committed frames in it); otherwise it opens the database `immutable`. A WAL without its shared-memory file is read with `mode=ro`, which recreates the shared memory only. Like any WAL reader it takes a read mark in the shared memory, which never blocks opencode's writes. A busy or locked database is retried on the next event or rescan (`collector.md` §4.5).

**Change signal**: the database file and `opencode.db-wal`. **`WatchPaths`**: those two files.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); the line shape, row order, value encoding, `session_message` rows and the changed-Session query filled in while writing this spec

### 2.3 The `legacy-json` Layout

Rank **`1`**. Record-key prefix **`json:`**. Each file is its own Raw record.

| File (relative to `<root>/storage/`) | Record key | Role | Session |
|---|---|---|---|
| `session/<projectID>/<ses>.json` | `json:session/<projectID>/<ses>.json` | `main` | `<ses>` |
| `message/<ses>/<msg>.json` | `json:message/<ses>/<msg>.json` | `attachment` | `<ses>` |
| `part/<msg>/<prt>.json` | `json:part/<ses>/<msg>/<prt>.json` | `attachment` | `<ses>` |

- A part's file path doesn't name its Session, so the Collector inserts `<ses>`: it's the directory holding `message/<ses>/<msg>.json`, found by listing the `message/` tree. This is the one legacy key that isn't a literal path (`protocol.md` §3.3 allows it).
- A part whose `<msg>` has no message file gets `json:part/_/<msg>/<prt>.json`. `MapKey` rejects it, so the Hub stores it unattached.

**`KnownIgnored`** (under `storage/`): `migration`, `project/**`, `session_diff/**`, and the pre-2025 nested layout `session/info/**`, `session/message/**`, `session/part/**`.

**`WatchPaths`**: `storage/session/`, `storage/message/`, `storage/part/`, recursively. The tree is static on current opencode versions, so this mostly matters for Machines still running an old build.

Source: [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); the part-key rule and the known-ignored list filled in while writing this spec

### 2.4 Scan paths and root-level files

**Scan paths** (for unclaimed-path detection): `storage/`, and the root's files matching `opencode*.db*`. Everything else at the root (`bin/`, `log/`, `snapshot/`, `tool-output/`, `project/`, `repos/`, `auth.json`) is outside the scan.

`KnownIgnored` at the root: `opencode-*.db`, `opencode-*.db-wal`, `opencode-*.db-shm` (channel databases), and the database's own `-wal` / `-shm` files (read through the database, not shipped).

Source: [Source format drift](https://github.com/tedkulp/agent-history/issues/16)

### 2.5 `MapKey`

- `db:<ses>` → `(<ses>, main, sqlite, 2)`
- `json:session/<projectID>/<ses>.json` → `(<ses>, main, legacy-json, 1)`
- `json:message/<ses>/<msg>.json` → `(<ses>, attachment, legacy-json, 1)`
- `json:part/<ses>/<msg>/<prt>.json` with `<ses>` not `_` → `(<ses>, attachment, legacy-json, 1)`
- anything else, or an id without its `ses_` / `msg_` / `prt_` prefix → not mine

A Session migrated into the database whose legacy files remain has records in both Layouts. The `sqlite` records win (rank 2). The legacy records become `shadow`: kept Raw-only, never parsed (`hub.md` §4.3).

Source: [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

### 2.6 `StartCwd` and `Parent` (Collector, for `exclude`)

- **`StartCwd`**:
  - `sqlite`: `session.directory`, read in the same query that finds changed Sessions
  - `legacy-json` `main`: the session file's `directory` field; if absent, `worktree` from `storage/project/<projectID>.json`
  - a legacy `attachment` inherits its Session's decision
- **`Parent`**:
  - `sqlite`: `db:<parent_id>` when `session.parent_id` is set
  - legacy `attachment`: the Session's `main` key
  - a legacy `main` with a `parentID` field: that Session's `main` key, if discovered

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11)

## 3. Mapping

The two Layouts hold the same logical objects: a Session, Messages (`data` JSON in the database, the whole file in legacy) and parts. After loading, both parse the same way.

### 3.1 Loading

- **`sqlite`**: read the `main` record's lines. A line with an unknown `table` is Raw only. `message.data` and `part.data` are JSON strings, parsed here; one that doesn't parse is a `bad_line` warning.
- **`legacy-json`**: the `main` record is the session object. Each `attachment` is a message (under `message/`) or a part (under `part/`), told apart by its key.
- Parts attach to their Message by `messageID` (legacy) or `message_id` (database). A part whose Message is missing is an `orphan` warning.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

### 3.2 Order and the Transcript

- Messages in **id order** (`msg_` ids sort by creation). Parts within a Message in id order (`prt_`).
- **Revert.** If the Session's `revert` is set (`{messageID, …}`), Messages from `revert.messageID` onward are Raw only. That is how opencode hides an undone tail until it deletes it.
- opencode has no other branches: the Transcript is linear.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the revert rule filled in while writing this spec

### 3.3 Messages

| Message `role` | Handling |
|---|---|
| `user` | `user` Message |
| `assistant` | `assistant` Message. `model` = `modelID`, `provider` = `providerID`. An assistant Message with `summary: true` is a compaction summary: its text becomes a `marker` (`compaction`) instead. |
| anything else | `unknown` Part + `unknown_type` warning |

- `timestamp` = `time.created`.
- **Usage** from the Message's `tokens`: `input` → `input`, `output` → `output`, `reasoning` → `reasoning`, `cache.read` → `cache_read`, `cache.write` → `cache_write`. `cost` is Raw only.
- An assistant Message's `error` is kept as a `text` Part at its end: `Error: <name>: <message>`.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the compaction and error handling filled in while writing this spec

### 3.4 Parts

| Part `type` | Handling |
|---|---|
| `text` | `text`. A `synthetic: true` text (opencode-injected) is Raw only. |
| `reasoning` | `thinking` |
| `tool` | `tool_call` (§3.5) |
| `file` | `image` when `mime` starts with `image/` and `url` is a `data:` URL; otherwise `attachment`, label = `filename` (or `url`) |
| `patch` | `attachment`, label `patch: N files` |
| `snapshot` | `attachment`, label `snapshot` |
| `agent`, `subtask` | `attachment`, label `@<name>` |
| `compaction` | `marker` (`compaction`) |
| `step-start`, `step-finish`, `retry` | Raw only (step bookkeeping; `step-finish` usage duplicates the Message's `tokens`) |
| anything else | `unknown` + `unknown_type` warning |

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5); the part table filled in while writing this spec from local data

### 3.5 Tool calls

opencode keeps a call and its result in **one** `tool` part whose `state` moves from `pending` to `running` to `completed` or `error`. No matching is needed.

| Payload field | Value |
|---|---|
| `call_id` | `callID` |
| `name` | `tool` |
| `input` | `state.input` |
| `output` | `state.output` when `completed`; `state.error` when `error`; else `null` |
| `status` | `completed` → `ok`; `error` → `error`; `pending` or `running` → `pending` |
| `diff` | for `tool == "edit"`: `{path: input.filePath, old: input.oldString, new: input.newString}`; else `null` |
| `child_sessions` | for `tool == "task"`: `[state.metadata.sessionId]` when set; else `[]` |

- opencode truncates long tool output and saves the full text under `<root>/tool-output/`. v1 shows the truncated output with its "Full output saved to" note (§5).

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); field mapping filled in while writing this spec from local data

### 3.6 Child Sessions

A sub-agent run is its own `session` row with `parent_id` set, so its own Raw record and its own Session.

- `parent_native_id` = `parent_id` (legacy: `parentID`).
- `spawning_call_id` = `null`. The child row doesn't record the call. The parent's `task` call links forward through `child_sessions` (§3.5), and the Hub finds the spawning call from there (`hub.md` §4.7).

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9)

### 3.7 Session fields

| Field | `sqlite` | `legacy-json` |
|---|---|---|
| native id | `id` | `id` |
| `title` | `title` | `title` |
| `started_at` | `time_created` | `time.created` |
| `last_activity_at` | `time_updated` | `time.updated` |
| `cwd` | `directory` | `directory` |
| `git_branch` | `null` | `null` |
| `source_version` | `version` | `version` |
| `parent_native_id` | `parent_id` | `parentID` |
| `forked_from_native_id` | `null` | `null` |

When `directory` is absent or empty (the oldest legacy files), `cwd` is the `path.cwd` of the first Message, in id order, that has a non-empty one. Every Message counts, including ones with no parts and ones the Transcript doesn't show. Only when no Message has one either is there a `missing_field` warning, and the Session goes to "No project". This holds in both Layouts.

opencode generates titles for sub-agent runs such as `Synthesize design tickets (@explore subagent)`; they're used as-is.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8)

### 3.8 Ids

- **Message id**: the `msg_` id. **Part id**: the `prt_` id. Both match the Hub's id charset.
- A marker or error Part with no Source id: `<message id>.<index>`.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

## 4. Behavior

- **Database exports are always `replace`** (`protocol.md` §4.2): rows change in place, so an append would be wrong. Each changed export becomes a new version on the Hub and the old one is kept as superseded.
- **Update rate.** A busy Session changes many rows per second. The 2 s debounce and 30 s cap (`collector.md` §4.4) apply to the whole database, and the Hub parses each Session at most every 10 s. A long active Session therefore creates a new version at most every 2–30 s while it runs. Accepted for v1: versions are zstd chunks, and nothing is deleted.
- **Migration.** When opencode migrates a legacy Session into the database, the `db:` record arrives, wins by rank, and the legacy records become `shadow`. The Session is re-parsed from the database.
- **Schema drift.** opencode's Drizzle migrations add columns and tables. The export carries new columns untouched; the parser reports unknown `data` shapes as warnings.
- **Deletes.** A Session deleted in opencode stops appearing in the export. The Hub keeps what it has.
- **`parser_version`** starts at `1`.

Source: [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Source format drift](https://github.com/tedkulp/agent-history/issues/16)

## 5. Out of scope

- Full tool output from `<root>/tool-output/`: the files aren't named by Session, so they can't be mapped to one. Truncated output is shown.
- Channel databases (`opencode-<channel>.db`).
- opencode's HTTP API as a read path.
- `storage/session_diff/`, `storage/project/`, `snapshot/` and patch contents.
- Tables other than `session`, `message`, `part` and `session_message` (projects, permissions, todos, accounts, events).
- Cost.

## 6. Acceptance checklist (after M1)

- [ ] `init` resolves the root from `XDG_DATA_HOME`, and writes `sources.opencode.db` when `OPENCODE_DB` is set.
- [ ] `status` shows the Layouts detected (`legacy-json`, `sqlite`, or both).
- [ ] With opencode running, the Collector reads the database without blocking it, and never creates or checkpoints its WAL.
- [ ] A new message in a live Session ships as a `replace` of `db:<ses>` within about 30 s; exporting an unchanged Session is a no-op on the Hub.
- [ ] Two exports of the same unchanged Session are byte-identical.
- [ ] Legacy session, message and part files ship under the §2.3 keys; each part attaches to its Session.
- [ ] A Session present in both Layouts parses from the database; its legacy records are `shadow`.
- [ ] A Session whose `directory` matches `exclude` is not shipped, and neither are its Child Sessions.
- [ ] A `tool` part shows as one Tool call with its output; an `error` state shows as ✗; an `edit` call renders as a diff.
- [ ] A `task` call links to its Child Session; the child links back to the parent.
- [ ] Reverted Messages don't show.
- [ ] An unknown part type shows as an `unknown` Part plus a warning.
