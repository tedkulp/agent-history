# Adapter spec: Hermes Agent

## 1. Purpose and scope

Hermes Agent (Nous Research's `hermes-agent`) is the fifth Source. This spec defines, for the Source identifier `hermes`:

- the Collector adapter: root resolution, the one `sqlite` Layout, which Sessions are collected, Record keys, the per-Session database export, starting-cwd reads for `exclude`
- the Hub parser: `MapKey`, and how an export becomes a Transcript

Hermes stores all its history in one live SQLite database, `state.db`, with a `sessions` and a `messages` table. It follows the opencode `sqlite` pattern ([`opencode.md`](opencode.md) §2.2): the Collector exports each Session's rows verbatim as JSONL and never parses them ([ADR 0001](../../adr/0001-hub-parses-collector-ships-raw.md)).

**Two meanings of "source".** Hermes's own `sessions.source` column is the Session's *entry point* (`cli`, `desktop`, `telegram`, `cron`, `subagent`, …), not our **Source**. This spec, and the code, call it the **Hermes entry point**.

Evidence: a local inspection of `state.db` at `schema_version` 30, and Hermes's `hermes_state*.py`.

Source: [Add Hermes Agent as a Source](https://github.com/tedkulp/agent-history/issues/73)

## 2. Layouts

### 2.1 Root

| | |
|---|---|
| Default root | `$HERMES_HOME` if set, else `~/.hermes` |
| Database | `<root>/state.db` |
| `Detect(root)` | the database file exists |
| `Version(root)` | empty: `state.db` records no Hermes version |
| Layouts present | `sqlite` |

### 2.2 The `sqlite` Layout

Rank **`1`**. No key prefix.

**Which Sessions.** A Session is collected when its Hermes entry point is `cli` or `desktop`, or when its entry point is `subagent` and its `parent_session_id` names a collected Session, at any depth. Chat gateways (`mattermost`, `telegram`, `slack`, …), `cron`, and their Child Sessions are not collected.

`cli` and `desktop` Sessions can carry a `parent_session_id` too: Hermes starts a new Session when it compresses one or `/branch`es it. Those aren't Child Sessions. Each is collected, and shown, as its own top-level Session.

**Record key**: the Hermes session `id`, e.g. `20261009_152518_0ba724`. One Raw record per collected `sessions` row, Child Sessions included.

**Content**: the Session's rows as JSONL, one row per line, in the shape of opencode's export (`opencode.md` §2.2):

```jsonl
{"table":"sessions","row":{"id":"20261009_152518_0ba724","source":"desktop",…,"started_at":1791573918.5571687,…,"title":"…",…}}
{"table":"messages","row":{"id":75,"session_id":"20261009_152518_0ba724","role":"user","content":"…",…,"active":1,"compacted":0,…,"display_order":75}}
```

- Lines, in order: the `sessions` row, then its `messages` rows ordered by `id`. Every row, including rewound ones: the Hub decides what the Transcript shows.
- `row` holds **every column** (`SELECT *`), encoded as in `opencode.md` §2.2. The export is deterministic.
- A missing `sessions` or `messages` table fails the Layout's discovery, and `status` shows the error. Other tables (`session_model_usage`, gateway state, full-text indexes) are not exported.

**Finding changed Sessions.** Hermes **rewrites rows in place**: compaction flips `active` and `compacted`, and content can be edited, without adding a row or a newer timestamp. Each record's change signal (`collector.md` §3.2) is therefore:

- **size**: the Session's row count, combined with a fingerprint: per message row, the lengths of `content`, `tool_calls`, `reasoning`, `role`, `tool_call_id` and `display_kind` plus its `active`, `compacted` and `_compressed_summary` flags (weighted so a flip changes the sum), all multiplied by the row's `id` so changes to two rows don't cancel, plus its `display_order`; and the `sessions` row's `title` and `cwd` lengths and `message_count`
- **mtime**: the latest of `started_at`, `ended_at`, `last_activity_at` and the messages' `timestamp`

SQLite has no hash function, so an edit that keeps a value's length (Hermes edits a user message's content in place) goes unseen until the Session next changes in a way the signal catches, such as a new row. Accepted for v1 as a known gap.

A Session whose signal differs from the one last acked is exported; the export ships as a `replace`, so the Hub keeps the earlier content as a superseded version.

**Reading the database**: read-only, exactly as opencode's (`opencode.md` §2.2). Hermes keeps lock files beside the database; the Collector never touches them.

**`WatchPaths`**: `state.db` and `state.db-wal`.

### 2.3 Scan paths and root-level files

**Scan paths**: the root's files matching `state.db*`. Everything else under the root (`config.yaml`, `skills/`, `cron/`, `kanban/`, other databases, `sessions/sessions.json`) is outside the scan.

`KnownIgnored`: `state.db-wal`, `state.db-shm` (read through the database).

### 2.4 `MapKey`

- a key of 1–128 characters from `[A-Za-z0-9_.-]` → `(<key>, main, sqlite, 1)`
- anything else → not mine

### 2.5 `StartCwd` and `Parent` (Collector, for `exclude`)

- **`StartCwd`**: `sessions.cwd`, else `git_repo_root`, read in the discovery query. Empty when neither is set.
- **`Parent`**: for a `subagent` Session, the record of `parent_session_id`. An excluded Session's Child Sessions are therefore excluded too. Any other Session has no parent here.

## 3. Mapping

### 3.1 Loading

Read the record's lines. A line with an unknown `table` is Raw only. A line that isn't JSON is a `bad_line` warning.

### 3.2 Which rows, in what order

- The Transcript shows rows with `active = 1 OR compacted = 1`: active rows, and rows a compaction summarized but kept. **Rewound** rows (`active = 0 AND compacted = 0`) are Raw only.
- Order: `display_order`, then `id`. A row with no `display_order` sorts by its `id`.
- Hermes stores no slash commands as messages, so nothing is Housekeeping.

### 3.3 Rows

| Row | Handling |
|---|---|
| `_compressed_summary = 1` | a `marker` (`compaction`) whose text is the row's content; `user` Message if the row's role is `user`, else `assistant` |
| `display_kind = 'hidden'` | Raw only: model-facing scaffolding Hermes never shows |
| role `session_meta`, `system` | Raw only: metadata, not Messages |
| role `user` | `user` Message from `content` |
| role `assistant` | `assistant` Message: `thinking` from `reasoning` (else `reasoning_content`), `text` from `content`, a `tool_call` per `tool_calls` entry. `model` = `sessions.model`. |
| role `tool` | the result of the call whose id is `tool_call_id` (§3.4); a result with no call in the Transcript is an `orphan` warning |
| any other role | `unknown` Part + `unknown_type` warning |

- `timestamp` is float unix seconds → milliseconds.
- **Content.** Plain text is one `text` Part. Content starting `\x00json:` is Hermes's encoding of a multimodal list: `text` items become `text` Parts, `image_url` items with a base64 `data:` URL become `image` Parts (else an `attachment` labelled `image`), and other items are `unknown`.
- Usage is not mapped: Hermes keeps token counts per Session, not per message.
- A Message left with no Parts is dropped.

### 3.4 Tool calls

An assistant row's `tool_calls` is an OpenAI-style list: `{id, call_id, type: "function", function: {name, arguments}}`. Results are separate `tool` rows.

| Payload field | Value |
|---|---|
| `call_id` | `id`, else `call_id` |
| `name` | `function.name` |
| `input` | `function.arguments` as JSON; arguments that aren't JSON become a JSON string; empty → `{}` |
| `output` | the answering `tool` row's `content`; `null` while there is none |
| `status` | no result → `pending`; a result that is a JSON object with `"success": false`, or with a non-empty string `"error"` and no `"success"` → `error`; else `ok` |
| `diff` | for `patch` calls with `old_string` and `new_string`: `{path, old: old_string, new: new_string}`; else `null` |
| `child_sessions` | `[]`: Hermes's `delegate_task` result doesn't name the Child Session |

### 3.5 Child Sessions

A `subagent` Session is its own record with `parent_native_id` = `parent_session_id`. Any other Session has no `parent_native_id`, even when it has a `parent_session_id` (§2.2). `spawning_call_id` is `null`: the child row doesn't record the `delegate_task` call. The parent's page lists it as a Child Session, and it links back to the parent.

### 3.6 Session fields

| Field | Value |
|---|---|
| native id | `id` |
| `title` | `title` when set; else the Hub's first-user-message rule (Hermes leaves Child Sessions untitled) |
| `started_at` | `started_at` |
| `last_activity_at` | the latest of `started_at`, `ended_at`, `last_activity_at` and every message's `timestamp` |
| `cwd` | `cwd`, else `git_repo_root`; with neither, the Session goes to "No project". A Session with Hermes entry point `desktop` gets no warning: Hermes Desktop sets `cwd` only when the person picks a workspace with a `project_*` tool, so most have none by design. Any other entry point gets a `missing_field` warning |
| `git_branch` | `git_branch` |
| `source_version` | `null` |
| `parent_native_id` | `parent_session_id` for a `subagent` Session; else `null` |
| `forked_from_native_id` | `null` |

### 3.7 Ids

- **Message id**: the `messages.id` integer, as a string.
- **Part id**: `<message id>.<index>`.

## 4. Behavior

- **Exports are always `replace`** (`protocol.md` §4.2): rows change in place. Each changed export becomes a new version on the Hub; the old one is kept as superseded.
- **Update rate** is as for opencode (`opencode.md` §4).
- **Schema drift.** New columns flow through the export untouched; the parser reads the columns it knows.
- **Deletes.** A Session Hermes deletes stops appearing in the export. The Hub keeps what it has.
- **`parser_version`** starts at `1`.

## 5. Out of scope

- Chat-gateway and `cron` Sessions.
- Hermes's other databases (`kanban.db`, `projects.db`, `shared-state.db`) and `sessions/sessions.json`.
- Cost and token fields from `sessions`.
- Several Hermes profiles, or several roots, on one Machine.

## 6. Acceptance checklist

- [ ] With a `state.db` present, the Collector ships one Raw record per `cli`/`desktop` Session and per `subagent` Session whose parent is collected, and nothing for gateway or `cron` Sessions.
- [ ] The Hub parses them into Transcripts with user, assistant, tool-call and tool-result Messages in `display_order`.
- [ ] Rewound rows are absent; compacted rows are present; a compaction summary shows as a marker.
- [ ] A `subagent` Session appears as a Child Session of its parent.
- [ ] When Hermes compacts or edits a shipped Session, the next run ships it as a `replace` and the Hub keeps the earlier content as superseded.
- [ ] A Session whose cwd matches `exclude` is not shipped, and neither are its Child Sessions.
- [ ] Hermes appears in the web UI's Source chips and the MCP endpoint.
