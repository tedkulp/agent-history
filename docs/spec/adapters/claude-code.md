# Adapter spec: Claude Code

## 1. Purpose and scope

Claude Code is the M1 Source. This spec defines, for the Source identifier `claude-code`:

- the Collector adapter: root, Layout, Record keys, discovery, watch paths, starting-cwd reads for `exclude`
- the Hub parser: `MapKey`, and how Raw records become a Transcript (Messages, Parts, Child Sessions, Session fields)

Claude Code writes one plaintext JSONL file per Session and appends to it per completed turn. Sub-agent runs are separate JSONL files next to it, and large tool output can spill into separate files. All of these are Raw records.

The Collector never parses these files beyond the starting-cwd read ([ADR 0001](../../adr/0001-hub-parses-collector-ships-raw.md)). The adapter interfaces are in [`collector.md`](../collector.md) §2.6 and [`hub.md`](../hub.md) §2.5.

Evidence: [`docs/research/claude-code-format.md`](../../research/claude-code-format.md), plus a local inspection of Claude Code 2.1.283 data made while writing this spec (line types, block types, spill-file names, sub-agent nesting).

Source: [Claude Code on-disk history format](https://github.com/tedkulp/agent-history/issues/2), [Spec assembly](https://github.com/tedkulp/agent-history/issues/18)

## 2. Layouts

### 2.1 Root

| | |
|---|---|
| Default root | `~/.claude/projects` |
| Override | `CLAUDE_CONFIG_DIR` set → `$CLAUDE_CONFIG_DIR/projects` |
| `Detect(root)` | `root` is a directory |
| `Version(root)` | empty. The Hub gets `source_version` from each parse. |

`init` resolves the root once from the interactive shell (`collector.md` §4.1).

Source: [Claude Code on-disk history format](https://github.com/tedkulp/agent-history/issues/2), [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 2.2 The `jsonl` Layout

Claude Code has one Layout, **`jsonl`**, rank `1`. Record keys have no Layout prefix.

Paths below are relative to the root. `<project>` is the starting cwd with `/` replaced by `-` (e.g. `-Users-ted-src-app`). `<session>` is the Session UUID.

| Path | Record key | Role | Owning Session (native id) |
|---|---|---|---|
| `<project>/<session>.jsonl` | the path | `main` | `<session>` |
| `<project>/<session>/tool-results/<name>` (any file, including sub-directories such as `p…/page-1.jpg`) | the path | `attachment` | `<session>` |
| `<project>/<session>/custom-title.json` | the path | `attachment` | `<session>` |
| `<project>/<session>.orphaned-<ts>-<suffix>.jsonl` | the path | `attachment` | `<session>` |
| `<project>/<session>.jsonl.superseded-<ts>` | the path | `attachment` | `<session>` |
| `<project>/<session>/subagents/agent-<agentId>.jsonl` | the path | `main` | `<session>/agent-<agentId>` (a Child Session) |
| `<project>/<session>/subagents/agent-<agentId>.meta.json` | the path | `attachment` | `<session>/agent-<agentId>` |

- The `orphaned` and `superseded` files are transcripts Claude Code set aside instead of overwriting. They are shipped and kept Raw-only. v1 never parses them.
- Nested sub-agents (a sub-agent spawning another) are filed **flat** in the top-level Session's `subagents/` directory, so every Child Session's key is under the top-level `<session>/`.

**`KnownIgnored`**: `*/memory/**` (Claude's auto-memory notes, not history).

**Scan paths** (for unclaimed-path detection, `collector.md` §4.7): the whole root.

**`WatchPaths(root)`**: the root and every directory under it, recursively. New directories are added to the watch as they appear.

**Compression**: none. Claude Code never compresses or archives Session files.

Source: [Claude Code on-disk history format](https://github.com/tedkulp/agent-history/issues/2), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); the key table, the `custom-title.json`, `orphaned` and `superseded` rows, and flat nesting filled in while writing this spec

### 2.3 `MapKey`

A pure function of the Record key, following the table above:

- `<project>/<session>.jsonl` → `(<session>, main, jsonl, 1)`
- `<project>/<session>/subagents/agent-<agentId>.jsonl` → `(<session>/agent-<agentId>, main, jsonl, 1)`
- `<project>/<session>/subagents/agent-<agentId>.meta.json` → `(<session>/agent-<agentId>, attachment, jsonl, 1)`
- any other key under `<project>/<session>/`, and the `orphaned` / `superseded` files → `(<session>, attachment, jsonl, 1)`
- anything else → not mine

`<session>` must look like a UUID; otherwise the key is not mine.

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9)

### 2.4 `StartCwd` and `Parent` (Collector, for `exclude`)

- **`StartCwd`** of a `main` record: the `cwd` field of the first line that has one. The Collector reads lines from the start until it finds it, at most the first 64 KiB. If none is found yet, the record waits (`collector.md` §4.6).
- **`Parent`**:
  - a sub-agent file or its `.meta.json` → `<project>/<session>.jsonl`
  - any other `attachment` → its owning `main`, `<project>/<session>.jsonl`

  An excluded Session therefore takes its Child Sessions and attachments with it.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11)

## 3. Mapping

### 3.1 Line types

Every line is a JSON object with a `type`. Conversation lines also carry `uuid`, `parentUuid`, `sessionId`, `timestamp`, `cwd`, `gitBranch`, `version` and `isSidechain`.

| `type` | Handling |
|---|---|
| `user`, `assistant` | Messages (§3.3) |
| `system` | By `subtype`: `compact_boundary` → `marker` (`compaction`). `turn_duration`, `local_command`, `away_summary`, `stop_hook_summary`, `informational`, `api_error`, `bridge_status` → Raw only. Any other subtype → `unknown`. |
| `attachment` | Raw only (context Claude Code injects: reminders, tool listings, environment), with one exception: subtype `queued_command` whose `attachment.isMeta` is not `true` becomes a user Message with `attachment.prompt` as its text (a block list gives its `text` and `image` blocks), or a `task_notification` marker (§3.3) when its `commandMode` is `task-notification` or its prompt is a string starting with `<task-notification>` |
| `summary` | Raw only; used for the title fallback (§3.6) |
| `custom-title`, `ai-title` | Raw only; used for the title (§3.6) |
| `file-history-snapshot`, `file-history-delta`, `last-prompt`, `mode`, `permission-mode`, `queue-operation`, `progress`, `atis-latch`, `bridge-session`, `cost-state`, `agent-name`, `pr-link`, `frame-link`, `artifact-autoreact-ledger`, `artifact-comment-monitor` | Raw only (bookkeeping) |
| anything else | `unknown_type` Parse warning. If the line has a `uuid` and sits on the Transcript path, it also becomes an `unknown` Part in its own Message (role `assistant`). |

- An unknown `system` subtype's `source_type` is `system:<subtype>`, so drift in subtypes is told apart from new line types.

- Lines without a `uuid` are never on the path, so they never produce Parts.
- A line that isn't valid JSON is a `bad_line` warning and is skipped.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); the type list filled in while writing this spec from local data

### 3.2 The Transcript path

Lines form a tree through `uuid` → `parentUuid`. The Transcript is the path from the root to the **latest leaf**.

1. The **latest leaf** is the last line in file order that has a `uuid` and (in a top-level Session's file) `isSidechain` not `true`. In a sub-agent file every line is a sidechain, so the flag is ignored there.
2. Walk `parentUuid` back to a line whose `parentUuid` is `null`.
3. If that root line has a `logicalParentUuid` (a compaction boundary), continue the walk from that uuid. The `compact_boundary` line becomes a `compaction` marker at that point.
4. If a `parentUuid` names a uuid that isn't in the file, stop there and record an `orphan` warning (`source_type` `parentUuid`).
5. Reverse the collected lines. That is the Transcript order.
6. Splice in **side results**. Claude Code chains parallel tool calls one onto the next, so the result of every call but the last hangs off the path as a side branch. A line not on the path that holds only `tool_result` blocks, and whose `parentUuid` is on the path, goes right after that parent. The exception is when every call it answers already has a result on the path.

Other lines not on the path (abandoned branches after `/rewind` or a retry, old inline sidechains) stay in the Raw record only.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the leaf rule and the compaction walk filled in while writing this spec

### 3.3 Messages and Parts

Walking the path in order:

**`user` lines**

| Content | Result |
|---|---|
| `isMeta: true` | Raw only (injected context) |
| `isCompactSummary: true` | `marker` Part (`compaction`), text = the summary |
| Only `tool_result` blocks | No Message. Each result merges into its Tool call (§3.4). |
| A string starting with `<command-name>`, or with `<command-message>` (a skill invocation, followed by `<command-name>`) | `marker` Part (`slash_command`), text = the command name plus `<command-args>` if non-empty, e.g. `/review 42` |
| A string starting with `<bash-input>` (a `!` shell command) | `marker` Part (`shell_command`), text = `$ ` plus the tag body (to the end of the string when there is no closing tag) with surrounding whitespace trimmed, e.g. `$ git status -sb` |
| A string starting with `<bash-stdout>` or `<bash-stderr>` that directly follows a `<bash-input>` line on the path | No Message. It is that marker's `output`: the `<bash-stdout>` body, then the `<bash-stderr>` body, each only when non-empty, joined with a newline. Claude Code writes both bodies HTML-escaped (`-&gt;`), so they are unescaped; the `<bash-input>` body is written raw. A command with no output line, or with both bodies empty, has no `output`. |
| A string starting with `<bash-stdout>` or `<bash-stderr>` with no command before it | Raw only |
| A string starting with `<local-command-stdout>`, `<local-command-stderr>` or `<local-command-caveat>` | Raw only |
| A string, with `origin.kind: "task-notification"` or starting with `<task-notification>` (a background task or sub-agent finished). Block-list content is an ordinary `user` Message. | `marker` Part (`task_notification`), see **Task notifications** below |
| Anything else | A `user` Message |

**Task notifications**

Claude Code tells the model a background task (an agent, a background shell command, a Monitor) has something to report with a `<task-notification>` block, written either as a `user` line or as a `queued_command` attachment (§3.1). Both become one `marker` Part with `marker: "task_notification"`:

| Tag | Payload field |
|---|---|
| `<summary>` | `text`, e.g. `Agent "Spec review" finished` |
| `<status>` | `task.status`: `completed`, `failed`, `killed`, or absent (a Monitor event) |
| `<tool-use-id>` | `task.tool_use_id`. When a `tool_call` Part in the Session has that `call_id`, `task.call_part` is its Part id, and that call's `notifications` lists this marker's Part id. |
| `<result>` | `task.result`, trimmed (Markdown) |
| `<event>` | `output`, trimmed: a Monitor event's text, shown preformatted like a `shell_command`'s output |
| `<usage>` | `task.tokens` (`<subagent_tokens>`), `task.tool_uses` (`<tool_uses>`), `task.duration_ms` (`<duration_ms>`), each only when present and a whole number |

`<task-id>`, `<note>`, `<output-file>`, `<worktree>`, text outside the tags, and any tag not listed are Raw only. `<result>` and `<event>` are free text that can quote the block's own tags, so each runs from its first opening tag to its last closing tag, the block ends at its last `</task-notification>`, the other tags are read only before them, and `<usage>` only after them. A block without its closing `</task-notification>` or without a non-empty `<summary>` is malformed: it is a Parse warning (`missing_field`, source type `task-notification`) and the line keeps its ordinary rendering (a `user` Message with the text).

In a `user` Message: a string content is one `text` Part. In a block list, `text` → `text`, `image` with a `base64` source → `image` (bytes decoded, MIME from `media_type`), `document` → `attachment` (label = its title or MIME type), any other block type → `unknown`. `tool_result` blocks mixed in still merge into their calls.

**`assistant` lines**

Claude Code writes **one line per content block**, all sharing the same `message.id`. Consecutive path lines with the same `message.id` form **one** assistant Message. A user line holding only `tool_result` blocks doesn't break the run: with parallel tool calls, the results sit between the `tool_use` lines (§3.2 step 6). Any other user line does.

| Block | Part |
|---|---|
| `text` | `text` |
| `thinking` | `thinking` with the `thinking` text. The `signature` is Raw only. A block with empty `thinking` (Claude Code often keeps only the signature) gives no Part. |
| `redacted_thinking` | Raw only |
| `tool_use` | `tool_call` (§3.4) |
| `image` | `image` |
| any other | `unknown` |

- `model` = `message.model`. `provider` = `anthropic`.
- **Usage** from the group's last line's `message.usage`: `input_tokens` → `input`, `output_tokens` → `output`, `cache_read_input_tokens` → `cache_read`, `cache_creation_input_tokens` → `cache_write`, `output_tokens_details.thinking_tokens` → `reasoning`. Missing fields are `0`.
- An `isApiErrorMessage` line is kept as an ordinary assistant Message (its text is the error).

**Timestamps**: each Message takes the `timestamp` of its first line.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the per-block grouping, the `user` content table and the usage field names filled in while writing this spec

### 3.4 Tool calls

A `tool_use` block (`id`, `name`, `input`) becomes a `tool_call` Part where it appears. Its result is the `tool_result` block with `tool_use_id` equal to `id`, found on a later path line.

| Payload field | Value |
|---|---|
| `call_id` | `tool_use.id` |
| `name` | `tool_use.name` |
| `input` | `tool_use.input`, as-is |
| `output` | `tool_result.content`: a string as-is; a block list → its `text` blocks joined by `\n`, and each `tool_reference` block as `[tool reference: <name>]` |
| `status` | `error` if `tool_result.is_error` is true; `ok` if a result exists; `pending` if none is on the path |
| `diff` | For `name == "Edit"`: `{path: input.file_path, old: input.old_string, new: input.new_string}`. Otherwise `null`. |
| `child_sessions` | For `name` `Agent` or `Task`: `["<session>/agent-<agentId>"]`, where `agentId` comes from the result line's `toolUseResult.agentId`. Otherwise `[]`. |

- `image` blocks inside a `tool_result` become `image` Parts placed right after the `tool_call` Part, in the same Message.
- A `tool_result` whose `tool_use_id` matches no call on the path is an `orphan` warning (`source_type` `tool_result`) and is otherwise dropped.

**Spilled output.** Large results are replaced in the JSONL by a marker:

```text
<persisted-output>
Output too large (43.3KB). Full output saved to: /Users/ted/.claude/projects/-Users-ted-src-app/<session>/tool-results/bdqdsmwk4.txt

Preview (first 2KB):
…
```

- The parser takes the path after `Full output saved to: `, keeps the part from `<session>/tool-results/` on, and looks for the attachment with Record key `<project>/<session>/tool-results/<name>`. If there's no such line in the marker, it tries `tool-results/<tool_use_id>.txt`.
- Found → `output` is that file's content (UTF-8, invalid bytes replaced).
- Not found → `output` keeps the marker text, which includes the preview. No warning: a Child Session's spills land in the top-level Session's `tool-results/`, which the child's parse doesn't receive.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Claude Code on-disk history format](https://github.com/tedkulp/agent-history/issues/2); the marker format, the `toolUseResult.agentId` link and the `Edit` diff filled in while writing this spec

### 3.5 Child Sessions

A sub-agent file `<project>/<session>/subagents/agent-<agentId>.jsonl` is its own Session, native id `<session>/agent-<agentId>`, hidden from top-level browse.

- `parent_native_id` = `<session>`.
- `spawning_call_id` = `toolUseId` from the `.meta.json` attachment. If the attachment is missing, `null` and a `missing_field` warning (`source_type` `meta.json`).
- The parent links forward through its `tool_call` Part's `child_sessions` (§3.4).
- **Nested sub-agents**: a sub-agent spawned by another sub-agent is filed in the same flat `subagents/` directory, and its files don't name the sub-agent that spawned it. Its `parent_native_id` is therefore the **top-level** Session. The forward link from the spawning sub-agent's `tool_call` is still correct. Accepted for v1.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the native-id form and the nesting rule filled in while writing this spec

### 3.6 Session fields

| Field | Value |
|---|---|
| native id | `<session>` (or `<session>/agent-<agentId>`), from `MapKey` |
| `title` | The last `custom-title` line's `customTitle`; else `customTitle` from the `custom-title.json` attachment; else the last `ai-title` line's `aiTitle`; else the last `summary` line's `summary`; else empty (the Hub falls back to the first prompt). For a Child Session: the `.meta.json` `description`. |
| `started_at` | Earliest `timestamp` in the file |
| `last_activity_at` | Latest `timestamp` in the file |
| `cwd` | `cwd` of the first line that has one |
| `git_branch` | `gitBranch` of the last line that has one (the branch where the Session is now) |
| `source_version` | `version` of the last line that has one |
| `forked_from_native_id` | `null` (Claude Code has no forks as separate Sessions) |

A missing `cwd` is a `missing_field` warning; the Session goes to "No project".

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8); title precedence filled in while writing this spec

### 3.7 Ids

- **Message id**: the `uuid` of the Message's first line. A marker-only Message uses its line's `uuid`.
- **Part id**: `<message id>.<index>`, where `index` is the Part's 0-based position in the Message.
- Ids that don't match `[A-Za-z0-9_.:-]{1,128}` are hashed (`hub.md` §3.5). Claude Code uuids always match.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

## 4. Behavior

- **Write pattern.** Claude Code appends a whole turn at a time. Almost every change ships as an `append`. The Collector's 2 s debounce and 30 s cap (`collector.md` §4.4) keep an active Session current on the Hub.
- **Resume** (`claude --resume`, `--continue`) appends to the same file, so it continues the same Session.
- **Local retention.** Claude Code deletes Session files, sub-agent files and tool results after `cleanupPeriodDays` (default 30). The Collector runs as a service and ships within seconds, so this doesn't lose data. Files deleted locally stay on the Hub.
- **Format drift.** The per-line schema has grown by adding fields and line types. New line types show up as `unknown_type` warnings; adding them to §3.1 and bumping `parser_version` re-parses every Claude Code Session (`hub.md` §4.5).
- **`parser_version`** starts at `1`.

Source: [Claude Code on-disk history format](https://github.com/tedkulp/agent-history/issues/2), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Source format drift](https://github.com/tedkulp/agent-history/issues/16)

## 5. Out of scope

- Parsing the `orphaned` / `superseded` set-aside transcripts. They are shipped and kept Raw-only.
- Turning old inline sidechains (`isSidechain: true` lines inside a top-level file) into Child Sessions.
- Linking a nested sub-agent to the sub-agent that spawned it (it links to the top-level Session).
- `~/.claude/history.jsonl` (prompt recall), `~/.claude/file-history/` (edit checkpoints), `memory/` notes, and everything else outside `projects/`.
- Diffs for `MultiEdit`, `Write` and `NotebookEdit`: their input shows as JSON.
- Cost: Raw only, as for every Source.

## 6. M1 acceptance checklist

Claude Code is M1: this adapter and parser must be complete.

- [ ] `init` with `CLAUDE_CONFIG_DIR` set in the shell rc writes `root = "$CLAUDE_CONFIG_DIR/projects"`; without it, `~/.claude/projects`.
- [ ] Every file in the §2.2 table is discovered and shipped under the stated Record key; `memory/` files are known-ignored; any other file shows up as unclaimed.
- [ ] `MapKey` maps a Session file, its `tool-results/` files, `custom-title.json`, and a sub-agent file with its `.meta.json` as in §2.3, and returns "not mine" for a non-UUID `<session>`.
- [ ] A Session whose first-line `cwd` matches `exclude` is not shipped, and neither are its sub-agent files, `tool-results/` files or `custom-title.json`.
- [ ] An assistant turn written as several lines (thinking, text, tool_use) becomes one assistant Message with Parts in order, model and usage set.
- [ ] Each `tool_use` merges with its `tool_result` into one `tool_call` Part; `is_error` shows as ✗; a call with no result is `pending`.
- [ ] A spilled result's output is stitched in from its `tool-results/` file, so the UI shows the full output, not the `<persisted-output>` marker.
- [ ] An `Edit` call renders as a diff.
- [ ] After `/rewind`, the Transcript follows the newest branch; the abandoned branch is not shown.
- [ ] A compacted Session shows the pre-compaction Messages, a compaction marker, then the rest.
- [ ] A sub-agent file becomes a Child Session linked to the parent and its `Agent` call; the call shows the "↳ Child Session" link; the child isn't in the feed.
- [ ] `/clear` and other slash commands show as `slash_command` markers; `isMeta` lines and `attachment` lines don't show.
- [ ] A pasted image shows inline.
- [ ] The title follows `custom-title` → `custom-title.json` → `ai-title` → `summary` → first prompt.
- [ ] An unknown line type on the path shows as an `unknown` Part plus a warning; an unknown line type off the path gives a warning only.
- [ ] Re-parsing the same file gives the same Message and Part ids.
