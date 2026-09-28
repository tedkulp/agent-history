# Adapter spec: oh-my-pi

## 1. Purpose and scope

oh-my-pi (`omp`, a fork of pi-mono) is the third Source after M1. This spec defines, for the Source identifier `oh-my-pi`:

- the Collector adapter: root resolution (including XDG on macOS), Layout, Record keys, archive handling, starting-cwd reads for `exclude`
- the Hub parser: `MapKey`, and how session files become a Transcript

oh-my-pi writes one JSONL file per Session. Entries form a parent-pointer tree (`id` / `parentId`), and the schema is versioned (`session.version`, currently `3`). Sub-agents are separate JSONL files in an artifacts directory named after the parent file. The first physical line is a fixed-width title slot that omp rewrites in place.

The adapter interfaces are in [`collector.md`](../collector.md) §2.6 and [`hub.md`](../hub.md) §2.5.

Evidence: [`docs/research/oh-my-pi-format.md`](../../research/oh-my-pi-format.md), plus a local inspection of omp data made while writing this spec (entry and block types, sub-agent file names, the `task` tool result, `parentSession` values).

Source: [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4), [Spec assembly](https://github.com/tedkulp/agent-history/issues/18)

## 2. Layouts

### 2.1 Root

The root is omp's **agent directory**, the one that contains `sessions/`.

`<config>` is `~/` + `$PI_CONFIG_DIR` (a name relative to the home directory), default `~/.omp`. A profile is `OMP_PROFILE` (or the legacy `PI_PROFILE`), unless it is empty or `default`.

`DefaultRoot(env)` takes the first of these that contains a `sessions/` directory:

1. `$PI_CODING_AGENT_DIR`, when no profile is set (omp ignores it under a profile)
2. with a profile: `$XDG_DATA_HOME/omp/profiles/<profile>`, then `<config>/profiles/<profile>/agent`
3. without one: `$XDG_DATA_HOME/omp`
4. without one: `<config>/agent`, i.e. `~/.omp/agent`

omp files `sessions/` under the XDG *data* directory, with no `agent/` segment, and only when `XDG_DATA_HOME` is set: there is no `~/.local/share` fallback. It does this on macOS as well as Linux once `omp config init-xdg` has run. `archive/sessions/` sits next to `sessions/` in either case.

If none contains `sessions/`, the root is omp's default agent directory: `<config>/profiles/<profile>/agent` with a profile, else `<config>/agent` (reported "not detected").

| | |
|---|---|
| `Detect(root)` | `root/sessions` or `root/archive/sessions` is a directory |
| `Version(root)` | empty |

The XDG paths were checked against the compiled `dirs.ts` of omp 18.3.4 (`agentSubdir(…, "sessions", "data")`) when the adapter was built. The research reading (`$XDG_STATE_HOME/omp/agent`, `$XDG_DATA_HOME/omp/agent`) was wrong.

Source: [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4), [Collector design](https://github.com/tedkulp/agent-history/issues/11); the search order filled in while writing this spec

### 2.2 The `jsonl` Layout

One Layout, **`jsonl`**, rank `1`. Record keys have no Layout prefix.

**Files claimed** (relative to the root):

| Path | What it is |
|---|---|
| `sessions/<cwd-dir>/<ts>_<uuid>.jsonl` | A top-level Session |
| `sessions/<cwd-dir>/<ts>_<uuid>/<Name>.jsonl` | A sub-agent run spawned by that Session |
| `sessions/<cwd-dir>/<ts>_<uuid>/<Name>/<Name>.<Sub>.jsonl` | A sub-agent run spawned by sub-agent `<Name>`, and so on down |
| the same paths under `archive/sessions/`, with `.jsonl.gz` in place of `.jsonl` | Sessions archived by `omp gc` |

`<cwd-dir>` is omp's encoding of the starting cwd (e.g. `-src-foo`). `<ts>_<uuid>` is the creation time and Session UUID.

**Record key**: the path relative to `sessions/` (or to `archive/sessions/`), with `.gz` stripped. For example `-src-foo/2026-09-17T17-58-38-183Z_01a0b085-….jsonl` and `-src-foo/2026-09-17T17-58-38-183Z_01a0b085-…/T3SwitchResearch.jsonl`.

- Archiving keeps the same key, so `omp gc` never creates a second record.
- When both a live and an archived copy exist, the adapter reads the live one.

**`KnownIgnored`** (inside the scan paths):

- `**/.*.lock`, `**/.*.lock.os` (omp's lock files)
- `**/*.jsonl.*.bak` (rewrite backups)
- inside a Session's artifacts directory, at any depth: `*.md`, `*.json`, `*.log` (`.read.log`, `.bash.log`, `.bash-original.log`, `.eval.log`), `local/**` and `url-search/**` (sub-agent outputs, tool logs and caches, not history)

**Scan paths**: `sessions/` and `archive/sessions/`. `agent.db`, `history.db`, `models.db`, `stats.db`, `blobs/`, `config.yml` and the rest of the agent directory are outside the scan.

**`WatchPaths(root)`**: `sessions/` and `archive/sessions/`, recursively.

Source: [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); nested sub-agent paths and the known-ignored list filled in while writing this spec from local data

### 2.3 `MapKey`

Every claimed file is its own Session with role `main`. The native id is derived from the key:

| Key | Native id |
|---|---|
| `<cwd-dir>/<ts>_<uuid>.jsonl` | `<uuid>` |
| `<cwd-dir>/<ts>_<uuid>/<rest>.jsonl` | `<uuid>/<rest>`, e.g. `01a0b085-…/T3SwitchResearch` or `01a0b085-…/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch` |
| anything else | not mine |

→ `(<native id>, main, jsonl, 1)`.

A sub-agent's native id is its parent's native id + `/` + its task id, because omp files sub-agent `<id>` at `<parent artifacts dir>/<id>.jsonl`. The parser relies on this (§3.5).

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the native-id rule filled in while writing this spec

### 2.4 `StartCwd` and `Parent` (Collector, for `exclude`)

- **`StartCwd`**: `cwd` from the `session` header, which is the second physical line (the first is the title slot). The Collector reads at most the first 64 KiB.
- **`Parent`** of a sub-agent file: the key with its last path segment removed, plus `.jsonl`. For `…/<ts>_<uuid>/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch.jsonl` that is `…/<ts>_<uuid>/T3SwitchResearch.jsonl`.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11)

## 3. Mapping

### 3.1 Entry types

| `type` | Handling |
|---|---|
| `title` | The title slot (first physical line). Session title (§3.6). |
| `session` | The header. Session fields (§3.6). |
| `session_init` | A sub-agent's header: the agent name, model, task and tools. The `task` text becomes the first `user` Message of the Child Session (§3.5). `systemPrompt` is Raw only. |
| `message` | Messages (§3.3) |
| `model_change` | `marker` (`model_change`), text = `model` |
| `thinking_level_change` | `marker` (`thinking_level`), text = `thinkingLevel` |
| `compaction` | `marker` (`compaction`), text = its summary |
| `branch_summary` | `marker` (`compaction`), text = `Branch summary: ` + its summary, or `Branch discarded` when the summary is empty (omp writes an empty one when it drops an entry) |
| `reset_boundary` | `marker` (`slash_command`), text = `/clear` |
| `custom_message` | If `display` is true: `marker` (`slash_command`), text = `customType` plus `details.name` when present (e.g. `skill-prompt: setup-matt-pocock-skills`). Otherwise Raw only. |
| `title_change`, `credential_pin`, `model_usage`, `custom`, `service_tier_change`, `ttsr_injection`, `mode_change`, `label` | Raw only (bookkeeping, auth hashes, non-chat model calls, extension state) |
| anything else | `unknown` Part + `unknown_type` warning |

A line that isn't valid JSON is a `bad_line` warning and is skipped.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4); marker assignments filled in while writing this spec

### 3.2 Schema versions and the Transcript path

The parser branches on `session.version`:

- **v2 and v3**: every non-header entry has `id` and `parentId`. The **latest leaf** is the last entry in file order with an `id`. The Transcript is the path from the root (`parentId: null`) to that leaf. A `parentId` that names no entry stops the walk with an `orphan` warning.
- **v1**: no tree. The Transcript is all entries in file order. A header with no `version` whose entries have no `id` is v1 too.
- **v2** also used the message role `hookMessage`, which v3 renamed `custom`. Both are treated as `custom` (Raw only).
- **A version above 3**: parsed as v3. Fields the parser can't find become `missing_field` warnings.

Entries off the path (abandoned `/fork` or `/tan` branches) stay in the Raw record only.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4)

### 3.3 Messages and Parts

`message` entries, by `message.role`:

| Role | Handling |
|---|---|
| `user` | A `user` Message. A string content is one `text` Part; blocks as below. |
| `assistant` | An `assistant` Message. `model` = `message.model`, `provider` = `message.provider`. |
| `toolResult` | No Message. Merged into its Tool call (§3.4). |
| `developer`, `custom` | Raw only (injected reminders and extension messages) |
| anything else | `unknown` Part + `unknown_type` warning |

Content blocks:

| Block | Part |
|---|---|
| `text` | `text`. `textSignature` is Raw only. |
| `thinking` | `thinking` with the `thinking` text. Signatures and `providerPayload` are Raw only. A block with no text (only a signature) has no Part. |
| `toolCall` | `tool_call` (§3.4) |
| `image` with inline base64 data | `image` |
| `image` with a `blob:sha256:<hash>` reference | `attachment`, label `image (blob <first 12 hex chars>)`. omp's shared blob store isn't collected in v1. |
| anything else | `unknown` |

- Text ending with omp's `[Session persistence truncated large content]` suffix is kept as-is. omp truncated it on write.
- **Usage** from `message.usage`: `input` → `input`, `output` → `output`, `cacheRead` → `cache_read`, `cacheWrite` → `cache_write`, `reasoningTokens` → `reasoning`. The `cost` breakdown is Raw only.
- Each Message's `timestamp` is the entry's `timestamp`.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); block handling and the blob fallback filled in while writing this spec

### 3.4 Tool calls

A `toolCall` block becomes a `tool_call` Part. Its result is the `toolResult` message with `toolCallId` equal to the block's `id`.

| Payload field | Value |
|---|---|
| `call_id` | `toolCall.id` (e.g. `call_…\|fc_…`) |
| `name` | `toolCall.name` |
| `input` | `toolCall.arguments` |
| `output` | the result's `text` blocks joined by `\n` |
| `status` | `error` if the result's `isError` is true; `ok` if a result exists; `pending` otherwise |
| `diff` | `null` in v1 |
| `child_sessions` | for `name == "task"`: one native id per sub-agent in the result's `details`: `<this Session's native id>/<id>` for each `id` in `details.results`, or in `details.progress` when `results` is empty. Otherwise `[]`. |

- `image` blocks in a result become `image` Parts right after the `tool_call` Part.
- A result with no matching call on the path is an `orphan` warning (`source_type` `toolResult`).
- One `task` call can spawn several sub-agents, so `child_sessions` can hold several ids.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); the `task` result link filled in while writing this spec from local data

### 3.5 Child Sessions

A sub-agent file is its own Session, hidden from top-level browse.

- `parent_native_id` = the native id with its last `/<segment>` removed. It is derived from the key, not from `session.parentSession` (which holds an absolute path on the Machine).
- `spawning_call_id` = `null`. The sub-agent file doesn't record the call id. The parent's `task` call links forward through `child_sessions`, and the Hub finds the spawning call from there (`hub.md` §4.7).
- Its first Message is the `session_init` `task` text, as a `user` Message. omp 18 also writes the task as the next `message` on the path, a `user` message with the same text; that message then stands in for it, so the task shows once.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9); filled in while writing this spec

### 3.6 Session fields

| Field | Value |
|---|---|
| native id | from `MapKey` |
| `title` | the `title` slot's `title`; else `session.title`; else empty. For a Child Session with neither: `session_init.agent` + the last key segment, e.g. `task: T3SwitchResearch`. |
| `started_at` | `session.timestamp` |
| `last_activity_at` | the latest entry `timestamp` |
| `cwd` | `session.cwd` |
| `git_branch` | `null` (omp records none) |
| `source_version` | `schema-<session.version>`, e.g. `schema-3`. omp doesn't write its app version into the file. |
| `forked_from_native_id` | `null` |

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4)

### 3.7 Ids

- **Message id**: the entry `id` (8 hex chars in v2+). v1 entries have none: a hash of native id + line position.
- **Part id**: `<message id>.<index>`.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

## 4. Behavior

- **Appends.** Normal turns are appended one line per entry, so they ship as `append`s.
- **The title slot.** A title change overwrites the first line in place. The length stays the same but the prefix hash changes, so the Collector sends a `replace` (`protocol.md` §4.1). The Hub keeps the old version as superseded. Title changes are rare.
- **Full rewrites** (compaction, pruning, image dropping) replace the whole file atomically. They also ship as a `replace`, and the old content is kept.
- **`omp gc`** gzips old Sessions into `archive/sessions/` and moves their artifacts directories alongside. Keys don't change, so nothing is shipped. omp never deletes Session files on its own.
- **Directory-name churn.** omp 17.2.5–17.2.8 used hashed `<cwd-dir>` names, and later versions migrate them back. A migrated Session file gets a new key and is shipped again as a new record with the same native id, attaching to the same Session. Both records are `main` in the same Layout; the Hub parses the one attached most recently. Accepted for v1.
- **`parser_version`** starts at `1`.

Source: [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

## 5. Out of scope

- Images stored in omp's shared blob store (`blob:sha256:` references). They show as an attachment label.
- `history.db`, `stats.db`, `agent.db`, `models.db`.
- Sub-agent output files (`*.md`, `*.json`) and tool caches in artifacts directories.
- A git branch for omp Sessions.
- Diffs for omp's `edit` tool.
- Cost.

## 6. Acceptance checklist (after M1)

- [ ] **Build-time fact:** the XDG paths in §2.1 match `dirs.ts` in the omp version being targeted (checked against omp 18.3.4 when the adapter was built). If they differ, fix §2.1 first.
- [ ] `init` finds the root through `PI_CODING_AGENT_DIR`, `OMP_PROFILE`, or XDG on macOS when those are set in the shell rc.
- [ ] A Session file and its nested sub-agent files ship under the keys in §2.2; lock files, `.bak` files and artifact outputs are known-ignored.
- [ ] `omp gc` archiving a shipped Session ships nothing and creates no second record.
- [ ] Changing a Session's title makes the Collector send a `replace`; the Hub shows the new title and keeps the old version.
- [ ] A Session whose header `cwd` matches `exclude` is not shipped, and neither are its sub-agent files.
- [ ] The Transcript follows the latest branch after a `/fork`.
- [ ] Each `toolCall` merges with its `toolResult`; `isError` shows as ✗.
- [ ] A `task` call that spawned two sub-agents links to both Child Sessions; each child links back to the parent.
- [ ] Model and thinking-level changes show as markers; `developer` reminders don't show.
- [ ] A v1-schema file (no `id` / `parentId`) parses in file order.
- [ ] An unknown entry type shows as an `unknown` Part plus a warning.
