# Protocol spec: Collector → Hub ingestion

## 1. Purpose and scope

The Collector on each Machine ships Raw records to the Hub over this protocol. The Hub stores them durably and parses them into Transcripts later. The Collector never parses a Source format ([ADR 0001](../adr/0001-hub-parses-collector-ships-raw.md)). It only discovers Raw records, names them by Record key, and ships their bytes.

This spec covers:

- the unit of transfer (the Raw record) and its Record key
- the four HTTP endpoints under `/api/v1`
- the append / replace rules and the manifest reconcile that keeps the Collector and the Hub in agreement
- identity, versioning, retries, and error handling

Record-key rules for each Source are in the adapter specs (`adapters/*.md`). Storage of what arrives is in [`hub.md`](hub.md). Discovery, the watch loop, and the local cache are in [`collector.md`](collector.md).

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

## 2. Interfaces

### 2.1 Transport

- Plain HTTP on the VPN. No TLS, no authentication. The operator can put a reverse proxy in front of the Hub.
- All endpoints live under `/api/v1`. JSON bodies are UTF-8 with `Content-Type: application/json`.
- The shared Go package `protocol` holds the wire types, the header names, and the `MinCollectorVersion` constant. Both binaries import it.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

### 2.2 Headers on every request

| Header | Value |
|---|---|
| `X-Machine-Id` | The Machine UUID from the Collector's config. Must equal `{id}` in the path; otherwise `400`. |
| `User-Agent` | `agent-history-collector/<version>`, where `<version>` is the Collector's semver without the `v`, e.g. `agent-history-collector/0.3.1`. The Hub checks it against the minimum version (§4.6). |

A request for an unknown Machine id registers that Machine automatically. The Hub creates a `machines` row with only the id and `first_seen_at`. The Collector always sends `PUT /machines/{id}` first, so in practice the row gets filled in at once.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

### 2.3 Endpoints

| Method and path | Purpose |
|---|---|
| `PUT /api/v1/machines/{id}` | Register the Machine or update its metadata |
| `GET /api/v1/machines/{id}/manifest` | What the Hub holds for this Machine: the current version of every Raw record |
| `POST /api/v1/machines/{id}/records` | Append to a Raw record, or replace it |
| `GET /api/v1/machines/{id}/health` | Hub-side health for this Machine, shown by Collector `status` |

#### `PUT /api/v1/machines/{id}`

Sent on Collector start, from `init`, and whenever a field changes (display name, detected Sources). Request body:

```json
{
  "display_name": "work-laptop",
  "hostname": "tk-mbp.local",
  "os": "darwin",
  "arch": "arm64",
  "home_dir": "/Users/ted",
  "collector_version": "0.3.1",
  "sources": [
    {
      "source": "claude-code",
      "detected": true,
      "version": null,
      "root": "/Users/ted/.claude/projects",
      "layouts": ["jsonl"]
    },
    {
      "source": "opencode",
      "detected": true,
      "version": "1.18.31",
      "root": "/Users/ted/.local/share/opencode",
      "layouts": ["legacy-json", "sqlite"]
    }
  ]
}
```

- `home_dir` is required. The Hub uses it to put home-directory Sessions in the "No project" bucket.
- `version` is the Source's version if the Collector can read it cheaply, otherwise `null`.
- The Hub replaces the stored metadata with the body, sets `last_seen_at`, and returns `204`. If `home_dir` changed, the Hub re-runs Project assignment for that Machine (see `hub.md`).

#### `GET /api/v1/machines/{id}/manifest`

Returns the **current** version of every Raw record the Hub holds for this Machine. Superseded versions are not listed.

```json
{
  "records": [
    {
      "source": "claude-code",
      "record_key": "-Users-ted-src-app/5f1c…e2.jsonl",
      "length": 184223,
      "sha256": "9b0e…"
    }
  ]
}
```

- `length` is in bytes of the decompressed content. `sha256` is lowercase hex over those bytes.
- The Hub compresses the response if the request sends `Accept-Encoding: zstd` or `gzip`.
- An optional `?source=<source>` query limits the list to one Source.

#### `POST /api/v1/machines/{id}/records`

Carries one chunk of one Raw record. The body is the raw bytes, zstd-compressed.

| Header | Value |
|---|---|
| `X-Source` | Source identifier (§3.1) |
| `X-Record-Key` | The Record key, UTF-8, percent-encoded as in RFC 3986 |
| `X-Mode` | `append` or `replace` |
| `X-Offset` | Byte offset of the first body byte in the decompressed record. Must be `0` for `replace`. |
| `X-Prefix-Sha256` | For `append`: lowercase hex sha256 of the record's bytes `[0, offset)`. At offset 0 it is the sha256 of the empty string, and the header may be left out. Not used for `replace`. |
| `Content-Encoding` | `zstd` (required) |

The Collector keeps each decompressed body to **8 MiB** (8,388,608 bytes), except for a single oversized line (§4.3). The Hub rejects any decompressed body over **64 MiB** with `413`.

Success returns `200` with the record's new current state:

```json
{ "length": 8388608, "sha256": "4a7c…", "version": 3 }
```

`version` is the Hub's version number for the record; it only changes on a `replace`.

For the full list of responses, see §4.7.

#### `GET /api/v1/machines/{id}/health`

```json
{ "sessions_with_warnings": 14 }
```

Counts this Machine's Sessions whose latest parse recorded any Parse warning. Collector `status` prints it as "Hub: N Sessions with parse warnings". Fields may be added later.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8)

## 3. Data

### 3.1 Source identifiers

| Source | `source` value |
|---|---|
| Claude Code | `claude-code` |
| Codex | `codex` |
| oh-my-pi | `oh-my-pi` |
| opencode | `opencode` |

### 3.2 Raw record

A Raw record is identified by **(Machine, Source, Record key)**. Its content is one of:

- **File Layouts** (Claude Code, Codex, oh-my-pi, opencode `legacy-json`): the bytes of one Source file. Compressed files (`.jsonl.zst`, `.gz`) are shipped **decompressed**, so the Hub always stores what the Source originally wrote.
- **opencode `sqlite` Layout**: one Session's rows from the `session`, `message`, `part` and `session_message` tables, exported verbatim as JSONL. Each line is one row, tagged with its table name. This is an export, not a parse, so ADR 0001 holds. The exact line shape is in `adapters/opencode.md`.

**JSONL content** is a record whose Record key ends in `.jsonl`, plus every opencode `db:` export. For JSONL content, the Collector ships only up to the **last complete `\n`**. A half-written line is never sent. Chunk boundaries also fall on line boundaries (§4.3). Any other file (e.g. Claude Code `tool-results/*.txt`, images, `.meta.json`, opencode legacy `.json`) is shipped whole, as it is on disk, and its chunks split at 8 MiB.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

### 3.3 Record key

The Source's adapter on the Collector defines the Record key. The protocol requires:

- **Stable.** The key stays the same when the Source compresses, moves, or archives the file. For example, Codex `.jsonl` → `.jsonl.zst`, Codex `archived_sessions/`, and oh-my-pi `archive/` all map back to the original key. They never create a second Raw record.
- **Relative to the Source root**, not an absolute path. Moving the root (for example `CLAUDE_CONFIG_DIR`) does not change keys. A key need not be a literal path: an adapter may synthesize one to keep it stable or to name its Session (Codex keys are the bare file name; opencode legacy part keys insert the Session id).
- **Layout-prefixed** when a Source has more than one Layout (e.g. `json:` / `db:` for opencode), so two Layouts never collide.
- Non-empty UTF-8, at most 1024 bytes.

The Collector never sends a Session id. **The Hub derives the owning Session** from `(Source, Record key)` with the Source parser's pure function `MapKey`: `record key → (native Session id, role, Layout name, Layout rank)`, where the role is `main` or `attachment`. The Hub itself demotes records outside a Session's winning Layout to a third role, `shadow` (see `hub.md` §4.3).

If the Hub cannot map a key (an unknown Source, or a Layout prefix this Hub's parser doesn't know), it still **stores the bytes and acks**. The record stays unattached to any Session until a Hub with a matching parser maps it on start (see the Re-parse flow in `hub.md`). Raw data is never refused for being unfamiliar.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); synthesized keys filled in by [Write the adapter specs](https://github.com/tedkulp/agent-history/issues/23)

### 3.4 Versions on the Hub

Each Raw record has one **current** version and any number of **superseded** versions.

- `append` extends the current version.
- `replace` starts a new current version. The old one becomes superseded and is kept. The Hub never deletes.
- Only the current version is listed in the manifest and parsed.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

## 4. Behavior

### 4.1 Reconcile

**The Hub is the source of truth** for what it has received. The Collector reconciles:

- on start
- after reconnecting from a Hub outage
- on `agent-history sync`

Reconcile steps:

1. `PUT /machines/{id}` with current metadata.
2. `GET /machines/{id}/manifest`.
3. For every Raw record discovered on disk (after `exclude` filtering), compare the local content `L` (for JSONL content, cut at the last complete line, §3.2) with the Hub's entry `H`:

| Case | Action |
|---|---|
| No `H` | `append` from offset 0 |
| `H.length == L.length` and `H.sha256 == sha256(L)` | Nothing |
| `H.length < L.length` and `sha256(L[0:H.length]) == H.sha256` | `append` from `H.length` |
| Anything else (shorter, same length but different content, diverged prefix) | `replace` |

4. Update the local cache with the resulting `(length, sha256)` per record.

The local cache (in the Collector's state dir) records the last acked `(length, sha256)` per record. It keeps steady-state operation cheap, so the Collector does not re-hash files the Hub already has. If the cache is lost or the Hub is restored from backup, the next reconcile repairs the difference.

Records that disappear from disk are **not reported**. The Hub keeps them.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Collector design: discovery, config and lifecycle](https://github.com/tedkulp/agent-history/issues/11)

### 4.2 Live changes

- A file change that the watcher or the 10-minute rescan detects is **debounced 2 s** after the last write (a record that keeps changing still ships at least every 30 s, see `collector.md` §4.4), then shipped with the same decision table as §4.1, using the local cache in place of the manifest.
- opencode `sqlite` exports are always sent as `replace`. The Session's rows can change in place, so an append would be wrong.
- At most **4 uploads** are in flight at once. Uploads for one record are **serialized**: a record never has two requests in flight.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

### 4.3 Chunking

- Chunks are at most 8 MiB decompressed. A larger upload is a series of requests. There is no separate backfill code path: backfilling a new file is a series of `append`s from offset 0.
- A large `replace` is sent as one `replace` carrying the first chunk, then `append`s for the rest.
- For JSONL content, each chunk ends on a line boundary, so every intermediate state on the Hub is a valid prefix. A single line longer than 8 MiB goes in a chunk of its own, and that chunk may be up to the Hub's 64 MiB limit. A line longer than that is not shipped: the Collector stops that record at the line and logs a `warn`.
- The Hub may parse an intermediate state. The 10 s parse coalescing (see `hub.md`) usually absorbs this, and a later parse replaces the result anyway.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

### 4.4 How the Hub handles a `records` request

All in one transaction on the Hub's single writer connection:

1. Validate headers and decompress the body.
2. **`append`**: look up the current version. If the record doesn't exist, it counts as length 0 with the empty-string hash. Accept only if `X-Offset` equals the current length **and** `X-Prefix-Sha256` equals the current sha256. Otherwise return `409` (below). The Hub checks the prefix against its stored hash state and never re-reads stored bytes.
3. **`replace`**: if the body's length and sha256 equal the current version's, this is a no-op: return `200` with the current state. This makes retried replaces safe. Otherwise create a new current version with the body as its first chunk and mark the old one superseded.
4. Store the chunk, update the version's length, sha256 and hash state, and upsert the Session's `parse_queue` row (when the key maps to a Session).
5. Commit with `synchronous=FULL`, then return `200`.

**The `200` ack means the raw bytes are durable.** Parsing into a Transcript happens asynchronously afterwards. A parser failure never loses raw data and never blocks ingestion.

A `409` body carries the Hub's current state for that record:

```json
{ "error": "offset_mismatch", "length": 184223, "sha256": "9b0e…" }
```

The Collector applies the §4.1 decision table against this state. It then appends from the Hub's length, or sends a `replace`. This also resolves the case where an earlier append was committed but its ack was lost: the retry gets a `409`, the prefix matches, and the Collector carries on from the Hub's length.

After every `200`, the Collector checks that the returned `sha256` equals its own hash of `L[0:length]`. On a mismatch it marks the record for `replace` on the next reconcile.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

### 4.5 Hub unreachable

- The Collector keeps **no spool**. The files on disk are the queue.
- On a connection error or `5xx`, the Collector retries with exponential backoff: 1 s, doubling, capped at **5 min**, with jitter.
- When the Hub answers again, the Collector runs a full reconcile (§4.1) instead of replaying individual changes.
- Collector `status` shows whether the Hub is reachable, the time of the last successful sync, and the last error.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Collector design: discovery, config and lifecycle](https://github.com/tedkulp/agent-history/issues/11)

### 4.6 Versioning

- Changes within `/api/v1` are **additive only**. Both sides ignore unknown JSON fields and unknown headers.
- `/api/v2` is introduced only for a breaking change.
- **Minimum Collector version.** The effective minimum is the higher of:
  - the compiled-in `protocol.MinCollectorVersion`
  - the `AGENT_HISTORY_MIN_COLLECTOR_VERSION` env var, which can only raise it
- `MinCollectorVersion` is bumped by hand, in the same PR as an incompatible `/api/v1` change, and never as an upgrade nudge. A unit test asserts the floor is at most the version being built.
- The Hub checks the version from `User-Agent` on **every** `/api/v1` request. If it is below the minimum or can't be parsed as semver, the Hub returns `426 Upgrade Required` with:

  ```json
  { "error": "collector_too_old", "min_collector_version": "0.4.0" }
  ```

- On `426`, the Collector stops uploading and logs the minimum version. `status` shows "upgrade Collector (Hub requires ≥ 0.4.0)". The Collector retries hourly with `PUT /machines/{id}`, and reconciles once that succeeds.
- The Collector never upgrades itself; mise does that. After `mise upgrade`, the Collector restarts on its own version check (see `collector.md`) and sends the new version.
- Collector and Hub are released in lockstep under one `vX.Y.Z` tag. When the floor moves, the release notes say "⚠ requires Collector ≥ X".

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

### 4.7 Response codes

| Code | When | Collector does |
|---|---|---|
| `200` / `204` | Success | Update local cache |
| `400` | Missing or invalid header, `X-Machine-Id` ≠ path id, `replace` with non-zero offset, bad percent-encoding | Log at `error`, skip the record until it next changes |
| `409` | `append` offset or prefix hash mismatch | Re-decide against the returned state (§4.4) |
| `413` | Body over the size limit | Log at `error` (it's a Collector bug) |
| `415` | `Content-Encoding` not `zstd`, or the body fails to decompress | Log at `error`, retry once, then skip until the next change |
| `426` | Collector below the minimum version | Stop uploading, retry hourly (§4.6) |
| `5xx`, connection error | Hub trouble | Back off and reconcile (§4.5) |

Error bodies are JSON with at least an `error` code string.

Source: filled in while writing this spec, following [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

## 5. Out of scope

- Authentication, authorization and TLS (VPN only).
- Reporting deletions: Raw records removed locally stay on the Hub.
- A Collector-side spool or offline queue.
- Collector self-upgrade.
- The Collector sending Session ids, Project assignments, or anything parsed.
- Any API for the Web UI. The UI is server-rendered by the Hub and is not part of this protocol.
- Windows Collectors.

## 6. M1 acceptance checklist

M1 is Claude Code end to end.

- [ ] `protocol` package exists with the wire types, header names, Source identifiers, and `MinCollectorVersion`, imported by both binaries.
- [ ] `PUT /machines/{id}` registers a new Machine and updates an existing one; `home_dir` and `sources` are stored.
- [ ] A request from an unknown Machine id auto-registers it.
- [ ] `X-Machine-Id` mismatching the path id returns `400`.
- [ ] `GET /manifest` lists the current `(source, record_key, length, sha256)` for every Raw record of the Machine, and nothing for other Machines.
- [ ] `append` at the right offset and prefix hash returns `200` with the new length and sha256. Wrong offset or wrong hash returns `409` with the Hub's state.
- [ ] A retried append whose first attempt committed ends in the same Hub state as a single append.
- [ ] `replace` creates a new current version and keeps the old one as superseded. An identical `replace` is a no-op.
- [ ] Bodies over 64 MiB decompressed return `413`; non-zstd bodies return `415`.
- [ ] A Claude Code Session file over 8 MiB is shipped as several appends, each ending on a line boundary, and the Hub's sha256 matches the file's.
- [ ] A half-written trailing line is not shipped until it is complete.
- [ ] Killing the Hub mid-upload and restarting it: the Collector backs off, reconciles, and ends with the Hub's manifest matching disk.
- [ ] Deleting the Collector's local cache and restarting: reconcile uploads nothing new for records the Hub already has.
- [ ] Rewriting a shipped file with different content triggers a `replace`; the Hub keeps both versions.
- [ ] `200` is returned only after the chunk and the parse-queue row are committed; a parser that panics does not affect the ack.
- [ ] A Collector whose `User-Agent` version is below the effective minimum gets `426` with `min_collector_version`, and `status` shows it.
- [ ] `AGENT_HISTORY_MIN_COLLECTOR_VERSION` raises the floor but cannot lower it below the compiled-in value.
- [ ] `GET /health` returns `sessions_with_warnings` for the Machine.
