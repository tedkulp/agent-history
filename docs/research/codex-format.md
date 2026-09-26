# Codex CLI on-disk history format

Research for [issue #3](https://github.com/tedkulp/agent-history/issues/3) (wayfinder ticket, part of the map issue #1). Answers what a **Collector** needs to read Codex's **Raw records** to reconstruct a **Session**'s **Transcript**.

**Primary sources**: the `openai/codex` GitHub repository, cloned shallow at commit `6e1ab4d294cdf1d2606a91bf1acf6de0f0959c7a` (2026-09-26), specifically the `codex-rs/rollout`, `codex-rs/protocol`, `codex-rs/core`, `codex-rs/cli`, and `codex-rs/utils/home-dir` crates. Cross-checked against a live `~/.codex` directory on macOS (Codex CLI `0.156.1`). No secondary write-ups were used as sources of fact; blog posts / Stack Overflow were not consulted.

## Location and overrides

- Default root: `~/.codex` (`$HOME/.codex`), resolved via the `dirs` crate's `home_dir()` — same logic on macOS and Linux, no XDG dirs involved.
  - Source: `codex-rs/utils/home-dir/src/lib.rs::find_codex_home()`.
- Override: the `CODEX_HOME` env var. If set, it **must** already exist and be a directory, or Codex errors out; the path is canonicalized.
  - Source: same file, `find_codex_home_from_env()`; also documented in a doc-comment on `codex_core::config::find_codex_home` (`codex-rs/core/src/config/mod.rs:4906-4916`).
- Session (in this repo's vocabulary, one Codex "thread") history lives under `$CODEX_HOME/sessions/`, one level below that per `YYYY/MM/DD` (UTC) based on the session's creation timestamp:
  ```
  ~/.codex/sessions/YYYY/MM/DD/rollout-YYYY-MM-DDThh-mm-ss-<uuid>.jsonl
  ```
  - Source: `codex-rs/rollout/src/list.rs:438` (doc comment), confirmed against a live directory listing (`~/.codex/sessions/2026/09/24/rollout-2026-09-24T09-07-32-01a0d387-4530-74a2-8c7a-d5f10cc15942.jsonl`).
  - Constants `SESSIONS_SUBDIR = "sessions"` and `ARCHIVED_SESSIONS_SUBDIR = "archived_sessions"` are defined in `codex-rs/rollout/src/lib.rs:86-87`. Archiving a session (`codex archive`) moves its file under `archived_sessions/` rather than deleting it (`codex-rs/tui/src/session_archive_commands.rs`, which also backs `codex delete` and `codex unarchive`).
- A parallel SQLite metadata store also lives under `$CODEX_HOME` (see "Format stability" below) — `thread_history_1.sqlite` was observed locally alongside the JSONL tree.

## File format: JSONL, one record per line

Each Session's Raw record is a `.jsonl` file: one JSON object per line, envelope shape:

```json
{"timestamp": "<RFC3339>", "ordinal": <u64>, "type": "<record-kind>", "payload": {...}}
```

- Source: `codex-rs/rollout/src/recorder.rs` (module doc: *"Persist Codex session rollouts (.jsonl) so sessions can be replayed or inspected later"*, and it explicitly recommends inspecting them with `jq`/`fx`).
- Observed `type` values in one real 576-line session (counts in parentheses): `session_meta` (1, always first), `turn_context` (per turn), `response_item` (232 — the actual model/tool conversation items), `event_msg` (254 — UI/telemetry events, e.g. `task_started`, `item_completed`, `token_count`, `task_complete`, `thread_settings_applied`), `token_usage_record` (76 — one per model response), and `world_state` (12, agent/sandbox state snapshots).
- `response_item.payload.type` values seen: `message`, `reasoning`, `function_call`, `function_call_output`, `custom_tool_call`, `custom_tool_call_output`. These are effectively the Responses-API item shapes Codex sends the model, replayed verbatim.

### Appended live, not rewritten

The recorder is an append-only writer: `RolloutRecorder` opens the file once per Session and streams new lines to it as the turn progresses (`codex-rs/rollout/src/recorder.rs`, `RolloutWriterTask`/`mpsc` channel design). A file only gets *replaced* by two background maintenance jobs — both of which use a rename-over-existing-path pattern guarded by a process-wide lock (`codex-rs/rollout/src/maintenance.rs`, `rollout-maintenance.lock` under `$CODEX_HOME/.tmp/`):
1. **Compression** of cold files to `.jsonl.zst` (see below).
2. **Rollout→SQLite migration** (see below).

A per-thread writer lock also exists (`codex-rs/rollout/src/writer_lock.rs`) to keep a live append and a maintenance rewrite from touching the same file concurrently.

## Session identity

- `SessionMeta.id: ThreadId` — the stable thread ID, constant across the thread's life including forks/reverts.
- `SessionMeta.session_id: SessionId` — equal to the *root* thread's ID (i.e., for a forked/reverted thread this points at the original ancestor, while `id` is this thread's own ID).
- The `.jsonl` filename embeds the thread ID (and, only for a reverted thread, a second distinct "rollout ID" after an underscore — a reverted thread keeps writing under a new physical file but keeps its logical thread ID stable): `rollout-<timestamp>-<thread_id>[_<rollout_id>].jsonl`.
  - Source: `codex-rs/rollout/src/rollout_file_name.rs` (`RolloutFileName::parse`/`render`) and `codex-rs/protocol/src/protocol.rs:3121-3193` (`SessionMeta` struct + doc comments).
- Forking/threading fields also present: `forked_from_id`, `forked_from_ordinal_exclusive`, `parent_thread_id`.

## Metadata captured

All in the first line's `session_meta` payload (`SessionMeta`, `codex-rs/protocol/src/protocol.rs:3121`), plus an optional sibling `git` object (`SessionMetaLine`, same file, line 3234):

| Field | Notes |
|---|---|
| `cwd`, `runtime_workspace_roots` | working directory / workspace root(s) at creation |
| `originator`, `cli_version`, `source` | e.g. `codex-tui`, semver string, `cli`/other |
| `model_provider` | e.g. `openai` |
| `base_instructions` | system prompt text used, when not overridden |
| `memory_mode`, `history_mode` | `history_mode` is `legacy` or `paginated` (see stability section) |
| `creator_user_id`, `creator_account_id` | ChatGPT account identity, when available |
| `git.commit_hash`, `git.branch`, `git.repository_url` | `repository_url` is passed through a `SanitizedGitUrl` deserializer (credentials stripped) |

Per-turn context (`turn_context` records) additionally carries `model`, `effort` (reasoning effort), `approval_policy`, `sandbox_policy`, `permission_profile`, `collaboration_mode`, `current_date`/`timezone`, and a `comp_hash`.

**Token usage** is recorded twice, redundantly:
- A `token_usage_record` line after every model response, with `usage`, `turn_token_usage`, and `thread_token_usage`, each `{input_tokens, cached_input_tokens, cache_write_input_tokens, output_tokens, reasoning_output_tokens, total_tokens}`.
- An `event_msg` of type `token_count` with the same shape plus `model_context_window` and provider rate-limit info (`rate_limits.primary/secondary.used_percent`, `resets_at`, credit balance).

Timestamps: an RFC3339 `timestamp` on every envelope line, plus a monotonically increasing `ordinal` (line sequence number within the file) used for cursoring/pagination.

## Tool calls and results

Tool invocations are `response_item` records interleaved with model `message`/`reasoning` items, in call order:
- `function_call`: `{type, id, name, arguments (JSON string), call_id, internal_chat_message_metadata_passthrough: {turn_id, create_time}}`
- `function_call_output`: `{type, id, call_id, output: [{type, text}], internal_chat_message_metadata_passthrough}` — matched to its call via `call_id`.
- `custom_tool_call` / `custom_tool_call_output`: same shape, used for Codex's "custom tool" (non-function-calling) protocol path; in the sampled session these outnumbered plain `function_call`s (65 vs 10), so a parser must handle both.

There is no separate "tool result" top-level record type — results live in the transcript stream as ordinary items, ordered by their position in the file (their `ordinal`/line position is authoritative, not any embedded index).

## Sub-agents

Codex represents a sub-agent as **its own separate Session (own `.jsonl` rollout file)**, linked back to the parent rather than nested inline in the parent's transcript:
- `SessionMeta.parent_thread_id` — the parent thread's ID.
- `SessionSource::SubAgent(parent_thread_id)` / `ThreadSource::Subagent` — classifies the session as a sub-agent and carries the parent id redundantly.
- `agent_nickname`, `agent_role` (aliased from an older `agent_type` field name), `agent_path` — identify which "AgentControl"-spawned sub-agent this is.
- `subagent_history_start_ordinal` — the first ordinal in *this* file that is the sub-agent's own conversation; earlier ordinals are inherited/shared parent context rather than the sub-agent's own turns.
- Source: `codex-rs/protocol/src/protocol.rs:3153-3190` and the `ThreadSource`/`SessionSource` enums (~lines 2853-3015 in the same file).

Practical implication for a Collector: reconstructing a full multi-agent Session tree requires joining sibling rollout files by `parent_thread_id`, not just parsing one file.

## Local cleanup / retention

No automatic deletion was found. Two automatic/opt-in behaviors exist:
- **Compression**: a best-effort background job compresses rollout files older than 7 days to `<name>.jsonl.zst` (`MIN_ROLLOUT_AGE = 7 days`, `codex-rs/rollout/src/compression.rs:336`), triggered at startup or via RPC. A Collector must be able to decompress `.zst` rollout files, not just plain `.jsonl`.
- **User-initiated**: `codex archive` moves a rollout file to `$CODEX_HOME/archived_sessions/`; `codex delete` removes it; `codex unarchive` reverses the move (`codex-rs/tui/src/session_archive_commands.rs`).

No config-level "don't persist history" flag was found in this snapshot (a `history_mode`/`ThreadHistoryMode` field exists but controls a storage *layout* — legacy single-file vs. paginated — not whether history is kept at all).

## Format stability: actively in flux, JSONL is not the long-term plan

This is the most important finding for other wayfinder tickets: **the on-disk format is mid-migration, not stable.**

- JSONL rollout files remain the canonical/authoritative format today (`recorder.rs` doc comment calls them the persisted source of truth, recommending `jq`/`fx` for inspection).
- But the repo now ships a full SQLite-backed metadata/history layer: `codex-rs/thread-store`, `codex-rs/state` (with numbered SQL migrations, e.g. `codex-rs/state/migrations/0047_rollout_migration_state.sql` and `codex-rs/state/thread_history_migrations/0003_turn_rollout_positions.sql`), and a `codex migrate-rollouts` CLI subcommand (`codex-rs/cli/src/migrate_rollouts.rs`) that reads existing `.jsonl` rollouts and writes them into a `thread_history_*.sqlite` database, with `--apply`/dry-run, per-thread selection, throughput limiting, and progress reporting.
- Locally, a live `~/.codex` already has a populated `thread_history_1.sqlite` (958 KB) sitting next to the `sessions/` JSONL tree — consistent with this migration having run (automatically, at startup — see `metadata::backfill_sessions` in `codex-rs/rollout/src/state_db.rs:150-158`) even without the user invoking the CLI command explicitly.
- `SessionMeta.history_mode: ThreadHistoryMode` already distinguishes `legacy` (one monolithic JSONL) from `paginated` (chunked, with `history_base`/`HistoryPosition` pointing at a byte offset + ordinal in a prior "prefix file") — i.e., even the *file-per-session* assumption is not guaranteed to hold for newer sessions.
- The `SessionMeta` struct itself carries scars of past schema changes: a doc comment notes an `instructions` field "used to be" on `SessionMeta` and was moved to `TurnContext`; `agent_role` has a serde `alias = "agent_type"` for backward-compat with an older field name.

**Practical takeaway for the Collector design**: build the Codex reader against the JSONL `response_item`/`event_msg`/`session_meta` shapes described above (still true today), but do not assume the file layout is permanent — watch for `history_mode: "paginated"` sessions (multi-file, needs `history_base` stitching) and for `.jsonl.zst` compressed files, and expect that a future Codex version may make the SQLite thread-history DB the primary read path instead of the JSONL tree.

## Tiny redacted sample

First line of a real rollout file (`session_meta`), with all real paths/IDs/prompt text replaced by placeholders — structure and field names are real, values are not:

```json
{"timestamp":"2026-09-24T13:08:26.379Z","ordinal":0,"type":"session_meta","payload":{"session_id":"<uuid>","id":"<uuid>","timestamp":"2026-09-24T13:07:32.785Z","cwd":"<redacted-path>","runtime_workspace_roots":["<redacted-path>"],"originator":"codex-tui","cli_version":"0.156.1","source":"cli","thread_source":"user","model_provider":"openai","base_instructions":{"text":"<redacted-system-prompt>"}}}
```

A `function_call` / `function_call_output` pair, shape-only (arguments/output text redacted):

```json
{"timestamp":"...","ordinal":41,"type":"response_item","payload":{"type":"function_call","id":"<id>","name":"<tool-name>","arguments":"<redacted-json-string>","call_id":"call_abc123","internal_chat_message_metadata_passthrough":{"turn_id":"<id>","create_time":1758000000.0}}}
{"timestamp":"...","ordinal":42,"type":"response_item","payload":{"type":"function_call_output","id":"<id>","call_id":"call_abc123","output":[{"type":"text","text":"<redacted-output>"}],"internal_chat_message_metadata_passthrough":{"turn_id":"<id>","create_time":1758000000.0}}}
```

## Open questions / not covered here

- Exact SQLite schema of `thread_history_*.sqlite` (table names/columns) was not fully enumerated — only its existence, purpose (rollout migration target), and the migration CLI were confirmed from source. A Collector that wants to read the DB directly (instead of the JSONL tree) needs a follow-up pass through `codex-rs/state/thread_history_migrations/*.sql`.
- Windows was out of scope per the ticket (macOS/Linux only); `dirs::home_dir()` behaves the same on both via `$HOME`.
