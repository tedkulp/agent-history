# Adapter spec: Codex

## 1. Purpose and scope

Codex (the OpenAI Codex CLI) is the second Source after M1. This spec defines, for the Source identifier `codex`:

- the Collector adapter: root, Layout, Record keys, compression and archive handling, starting-cwd reads for `exclude`, and the known-ignored SQLite thread store
- the Hub parser: `MapKey`, and how rollout files become a Transcript

Codex writes one append-only JSONL "rollout" file per thread. Each line is an envelope `{timestamp, ordinal, type, payload}`. A thread is a Session. Sub-agents and forks are their own threads, so their own Sessions.

Codex is **mid-migration** to a SQLite thread store (`thread_history_*.sqlite`). v1 reads only the JSONL tree and makes the store visible as known-ignored.

The adapter interfaces are in [`collector.md`](../collector.md) §2.6 and [`hub.md`](../hub.md) §2.5.

Evidence: [`docs/research/codex-format.md`](../../research/codex-format.md), plus a local inspection of Codex CLI 0.156.1 data made while writing this spec (message roles, injected context blocks, output shapes).

Source: [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Spec assembly](https://github.com/tedkulp/agent-history/issues/18)

## 2. Layouts

### 2.1 Root

| | |
|---|---|
| Default root | `~/.codex` |
| Override | `CODEX_HOME` |
| `Detect(root)` | `root/sessions` or `root/archived_sessions` is a directory |
| `Version(root)` | empty. The Hub gets `source_version` (`cli_version`) from each parse. |

Source: [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3), [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 2.2 The `jsonl` Layout

One Layout, **`jsonl`**, rank `1`. Record keys have no Layout prefix.

**Files claimed** (relative to the root):

- `sessions/YYYY/MM/DD/rollout-<ts>-<thread>[_<rollout>].jsonl`
- the same names with `.jsonl.zst` (Codex compresses rollouts older than 7 days)
- the same names under `archived_sessions/` (moved there by `codex archive`), compressed or not

`<ts>` is the creation time (`YYYY-MM-DDThh-mm-ss`), `<thread>` the thread UUID. The optional `_<rollout>` suffix marks a continuation file for the same thread (a reverted thread, or a paginated history file).

**Record key**: the file's **basename**, with `.zst` stripped: `rollout-<ts>-<thread>[_<rollout>].jsonl`.

- The key drops the `sessions/YYYY/MM/DD/` or `archived_sessions/` directory on purpose, so archiving, unarchiving and compressing never change it. Basenames are unique because they contain the thread UUID. This is a deliberately synthesized key, allowed by `protocol.md` §3.3.
- If the same key exists in more than one place, the adapter reads, in order of preference: uncompressed under `sessions/`, compressed under `sessions/`, uncompressed under `archived_sessions/`, compressed under `archived_sessions/`.

**`KnownIgnored`**: `thread_history_*.sqlite`, `thread_history_*.sqlite-wal`, `thread_history_*.sqlite-shm` at the root. `status` lists them with their modification time.

**Scan paths** (for unclaimed-path detection): `sessions/` and `archived_sessions/`. The rest of `~/.codex` (config, logs, caches, other SQLite files, `session_index.jsonl`, `history.jsonl`) is outside the scan and is never reported as unclaimed.

**`WatchPaths(root)`**: `sessions/` and `archived_sessions/`, recursively. New date directories are added as they appear.

Source: [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); the basename key, the preference order and the scan paths filled in while writing this spec

### 2.3 `MapKey`

- `rollout-<ts>-<thread>.jsonl` → `(<thread>, main, jsonl, 1)`
- `rollout-<ts>-<thread>_<rollout>.jsonl` → `(<thread>, attachment, jsonl, 1)`
- anything else, or a `<thread>` that isn't a UUID → not mine

All files of one thread attach to one Session. The parser stitches them (§3.2). A thread that only has continuation files (its original was deleted) has no `main` and is never parsed; its records are kept.

Source: [Source format drift](https://github.com/tedkulp/agent-history/issues/16) (paginated files map to the same Session); the `main` / `attachment` split filled in while writing this spec

### 2.4 `StartCwd` and `Parent` (Collector, for `exclude`)

- **`StartCwd`**: `payload.cwd` of the first line, which is always `session_meta`.
- **`Parent`**:
  - a continuation file → the thread's `main` key, found among the discovered records by thread id
  - a sub-agent thread (its `session_meta` has `parent_thread_id`) → the discovered `main` key of that parent thread
  - otherwise none

  The adapter reads `parent_thread_id` from the same first line it reads `cwd` from.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11)

## 3. Mapping

### 3.1 Line types

| `type` | Handling |
|---|---|
| `session_meta` | Session fields (§3.5). Raw only otherwise, including `base_instructions`. |
| `turn_context` | Sets the current `model` for the following assistant Messages. A change of `model` from the previous `turn_context` → `marker` (`model_change`). A change of `effort` → `marker` (`thinking_level`). The first `turn_context` produces no marker. |
| `response_item` | Messages (§3.3) |
| `token_usage_record` | Usage (§3.3) |
| `compacted` | `marker` (`compaction`), text = its summary message if present |
| `event_msg`, `world_state` | Raw only (UI and telemetry events, sandbox snapshots). `event_msg` `token_count` duplicates `token_usage_record` and is ignored. |
| anything else | `unknown` Part in its own Message (role `assistant`) + `unknown_type` warning |

A line that isn't valid JSON is a `bad_line` warning and is skipped.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3); marker triggers filled in while writing this spec

### 3.2 Order and stitching

- Codex has no branches inside a file: the Transcript is the lines in **`ordinal`** order.
- For a thread with continuation files, the parser orders the files by following `history_base` in `session_meta` (a paginated file names the prefix file and position it continues from). Where `history_base` is absent, it orders files by `<ts>` in the key. Lines from each file are taken up to the position the next file continues from.
- A sub-agent's file starts with inherited parent context. Lines whose `ordinal` is below `session_meta.subagent_history_start_ordinal` are Raw only.
- Duplicate `ordinal` values across stitched files: the later file wins.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); the stitching and inherited-context rules filled in while writing this spec. The exact `history_base` field shape is to be confirmed against `codex-rs/rollout` when the parser is built (§6, first item).

### 3.3 Messages and Parts

`response_item` payloads, by `payload.type`:

| `payload.type` | Handling |
|---|---|
| `message`, role `user` | A `user` Message. Each `input_text` block → `text`, `input_image` → `image`. **Injected context** blocks are Raw only: a block whose whole text is one `<tag>…</tag>` element (e.g. `<environment_context>`, `<user_instructions>`, `<recommended_plugins>`, `<skill>`, `<app-context>`). A message left with no Parts produces no Message. |
| `message`, role `developer` or `system` | Raw only (instructions, like system prompts) |
| `message`, role `assistant` | `output_text` blocks → `text` Parts. Both `phase` values (`commentary`, `final_answer`) are text. |
| `reasoning` | `thinking` Part: the `summary` texts joined by `\n\n`. No Part if the summary is empty. `encrypted_content` is Raw only. |
| `function_call` | `tool_call` (§3.4) |
| `custom_tool_call` | `tool_call` (§3.4) |
| `function_call_output`, `custom_tool_call_output` | Merged into their call (§3.4) |
| anything else (e.g. `local_shell_call`, `web_search_call`) | `unknown` Part + `unknown_type` warning |

**Grouping.** Consecutive assistant-side items (assistant `message`, `reasoning`, tool calls) form **one** assistant Message. A `user` Message, a `turn_context` or a marker ends it.

- `model` = the current `turn_context.model`. `provider` = `session_meta.model_provider`.
- Each Message takes the envelope `timestamp` of its first line.

**Usage.** Each `token_usage_record` belongs to the most recent assistant Message before it. Records are **deduped by `response_id`**; several distinct responses in one Message are summed. From `payload.usage`: `input_tokens` → `input`, `output_tokens` → `output`, `cached_input_tokens` → `cache_read`, `cache_write_input_tokens` → `cache_write`, `reasoning_output_tokens` → `reasoning`.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the injected-context rule, grouping and usage field names filled in while writing this spec

### 3.4 Tool calls

| Payload field | `function_call` | `custom_tool_call` |
|---|---|---|
| `call_id` | `call_id` | `call_id` |
| `name` | `name` | `name` |
| `input` | `arguments` parsed as JSON; if it doesn't parse, the raw string | `input` (a string) |
| `output` | from the matching `*_output` item: a string as-is; a block list → its text blocks joined by `\n` | same |
| `status` | `pending` if no output; `error` if the output is an object with `success: false`; else `ok` | `pending` if no output; `error` if the call's `status` is `failed` or `incomplete`; else `ok` |
| `diff` | `null` | `null` |
| `child_sessions` | `[]` | `[]` |

- Calls and outputs are matched by `call_id`. The Part sits where the call was.
- `input_image` blocks in an output become `image` Parts right after the `tool_call` Part.
- An output with no matching call is an `orphan` warning (`source_type` = the output's `payload.type`).
- Codex's `apply_patch` edits show their patch text as input. v1 renders no diff for them.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3); status rules filled in while writing this spec

### 3.5 Session fields

From the first file's `session_meta` payload:

| Field | Value |
|---|---|
| native id | `id` (the thread id; equals `<thread>` in the key). A mismatch with the key is a `missing_field` warning, and the key wins. |
| `title` | empty (the Hub falls back to the first prompt; Codex keeps thread names outside the rollout, see §5) |
| `started_at` | `session_meta.timestamp` |
| `last_activity_at` | the latest envelope `timestamp` |
| `cwd` | `cwd` |
| `git_branch` | `git.branch`, if present |
| `source_version` | `cli_version` |
| `parent_native_id` | `parent_thread_id` (sub-agents), else `null` |
| `spawning_call_id` | `null`. The rollout doesn't record which call spawned the thread. The Hub links the child to its parent's top (`hub.md` §4.7). |
| `forked_from_native_id` | `forked_from_id`, else `null` |

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8)

### 3.6 Ids

- **Message id**: the decimal `ordinal` of the Message's first line, e.g. `41`. When stitched files repeat an ordinal, the parser uses a hash of the Record key + ordinal instead.
- **Part id**: `<message id>.<index>`.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

## 4. Behavior

- **Write pattern.** Rollouts are appended line by line while a thread is live, so changes ship as `append`s.
- **Compression** (`.jsonl` → `.jsonl.zst` after 7 days) rewrites the file under a new name with the same decompressed bytes. The Record key and content are unchanged, so the Collector ships nothing.
- **Archive / unarchive** moves the file. Same key, same bytes, nothing shipped.
- **`codex delete`** removes the file locally. The Hub keeps it.
- **Thread-store drift.** If Codex stops writing JSONL, the known-ignored `thread_history_*.sqlite` becomes newer than the newest claimed rollout. `status` shows both times (`collector.md` §4.7), so the operator notices. Reading the store is a later adapter change: a new Layout with a higher rank.
- **`parser_version`** starts at `1`.

Source: [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3), [Source format drift](https://github.com/tedkulp/agent-history/issues/16)

## 5. Out of scope

- Reading `thread_history_*.sqlite` directly. It is known-ignored in v1.
- Thread names from `session_index.jsonl`. It spans all threads, so it isn't a per-Session Raw record. Titles fall back to the first prompt.
- `history.jsonl` (prompt recall), logs, memories and other files outside `sessions/` and `archived_sessions/`.
- Linking a sub-agent to the exact Tool call that spawned it.
- Diffs for `apply_patch`.
- Rate-limit and credit data from `event_msg`, and cost.

## 6. Acceptance checklist (after M1)

- [ ] **Build-time fact:** the `history_base` field shape used in §3.2 matches `codex-rs/rollout` in the Codex version being targeted. If it differs, fix §3.2 first.
- [ ] `init` with `CODEX_HOME` set in the shell rc writes that root; without it, `~/.codex`.
- [ ] A live rollout under `sessions/YYYY/MM/DD/` ships as appends under its basename key.
- [ ] Compressing a shipped rollout to `.jsonl.zst`, or moving it to `archived_sessions/`, ships nothing and creates no second record.
- [ ] `thread_history_*.sqlite` shows as known-ignored with its modification time; nothing else in `~/.codex` outside the scan paths shows as unclaimed.
- [ ] A Session whose `session_meta.cwd` matches `exclude` is not shipped, and neither are its sub-agent threads.
- [ ] Injected `<environment_context>` and developer messages don't appear; the user's own prompt does.
- [ ] Assistant commentary, reasoning summaries and tool calls from one turn form one assistant Message; each call shows its output.
- [ ] Usage per assistant Message counts each `response_id` once.
- [ ] A model or effort change between turns shows as a marker.
- [ ] A sub-agent thread becomes a Child Session of its parent thread, without the inherited parent context.
- [ ] A forked thread shows "forked from" its origin.
- [ ] A thread with a continuation file parses as one Session.
- [ ] An unknown `response_item` type shows as an `unknown` Part plus a warning.
