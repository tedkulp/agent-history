# Hub spec

## 1. Purpose and scope

The Hub is the single central server. It receives Raw records from every Machine's Collector over the ingestion protocol, keeps them verbatim, parses them into Transcripts, and serves the web interface for browsing and full-text search.

**The Hub parses every Source format** ([ADR 0001](../adr/0001-hub-parses-collector-ships-raw.md)). The Collector only discovers and ships raw bytes. Because the Hub keeps every Raw record, it can re-derive every Transcript when a parser improves.

This spec covers:

- the `agent-history-hub` binary, its CLI and its config
- the Hub-side Source parser interface
- the storage schema: Machines, Raw records, Sessions, Transcripts, Parse warnings, full-text search, the parse queue
- the parse worker, Project assignment, the Re-parse flow and Parse warnings
- the Web UI: home feed, search results, Transcript page
- deployment: image, compose file, backups, migrations
- the Hub side of the release: the GHCR image

The wire protocol is in [`protocol.md`](protocol.md). The Collector is in [`collector.md`](collector.md). How each Source maps into the Normalized Transcript model, and how its Record keys map to Sessions, is in `adapters/*.md`. Release-wide rules are in [`README.md`](README.md).

Source: [Hub language and web UI stack](https://github.com/tedkulp/agent-history/issues/7), [ADR 0001](../adr/0001-hub-parses-collector-ships-raw.md)

## 2. Interfaces

### 2.1 Binary and stack

- The Hub binary is **`agent-history-hub`** (`cmd/hub`). It lives in the same Go module as the Collector (`cmd/collector`) and shares the `protocol` package with it (`protocol.md` §2.1).
- It ships only in the Hub image. There are no standalone Hub binaries on the GitHub Release.
- Stack:
  - Go, stdlib `net/http` with Go 1.22+ routing patterns
  - `modernc.org/sqlite` (pure Go, FTS5 included), built with `CGO_ENABLED=0`
  - server-rendered HTML with **templ** + **htmx**. No SPA, no JS build step.
  - Markdown with **goldmark** (GFM extensions), code highlighting with **chroma**
  - hand-written plain CSS (optionally starting from Pico.css)
  - static assets (CSS, `htmx.min.js`, chroma stylesheets) embedded with `embed.FS`

**Rejected:** Rust (SQLite does the heavy work, and a full re-parse of a few GB in Go takes minutes); a SPA (needs a JS build and an API); Tailwind (needs a build step).

Source: [Hub language and web UI stack](https://github.com/tedkulp/agent-history/issues/7), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

### 2.2 CLI

| Command | What it does |
|---|---|
| `agent-history-hub serve` | Runs the Hub: start-up sequence (§4.1), HTTP server, parse worker, backup scheduler. The image's default command. |
| `agent-history-hub healthcheck` | `GET http://127.0.0.1:<port>/healthz`, using the port from `AGENT_HISTORY_LISTEN`. Exits `0` on `200`, else `1`. Used by the image's `HEALTHCHECK`, so the image needs no curl. |
| `agent-history-hub backup` | Writes one backup now (§4.8) and exits |
| `agent-history-hub reparse --all \| --source <source> \| --session <id>` | Enqueues Sessions at re-parse priority (§4.5) and exits. `<source>` is a Source identifier (`protocol.md` §3.1); `<id>` is the Session id from the Transcript URL. |
| `agent-history-hub version` | Prints the version, e.g. `0.3.1` (`0.0.0-dev` without release ldflags), and nothing else |

- The subcommands other than `serve` run in a second process next to the live Hub, via `docker exec <container> agent-history-hub …`. They open the same database file. WAL mode and `busy_timeout` make that safe.
- Every config env var (§2.3) has a matching flag on `serve`, e.g. `--listen`, `--data`. A flag wins over the env var.
- Exit codes: `0` on success, `1` on any error, with the message on stderr.

Source: [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14); `version` and the second-process rule filled in while writing this spec

### 2.3 Configuration

Env vars only, each with a matching CLI flag. There is no config file.

| Env | Default | Meaning |
|---|---|---|
| `AGENT_HISTORY_LISTEN` | `:8080` | Listen address |
| `AGENT_HISTORY_DATA` | `/data` | Directory holding `hub.db` (plus `hub.db-wal`, `hub.db-shm`) |
| `AGENT_HISTORY_BACKUP_DIR` | `/backups` | Backup target |
| `AGENT_HISTORY_BACKUP_AT` | `03:00` | Daily backup time, container-local (`TZ` is respected). Empty disables scheduled backups. |
| `AGENT_HISTORY_BACKUP_KEEP` | `7` | Number of scheduled backups kept |
| `AGENT_HISTORY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `slog` text to stdout. |
| `AGENT_HISTORY_MIN_COLLECTOR_VERSION` | unset | Raises the minimum Collector version. It can never lower the compiled-in floor (`protocol.md` §4.6). |

An invalid value (unparseable time, negative keep, bad semver) makes `serve` exit `1` with the reason.

Source: [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15)

### 2.4 HTTP routes

All on one listener, plain HTTP.

**Ingestion API** (`/api/v1`, specified in [`protocol.md`](protocol.md) §2.3):

| Route | Hub behavior |
|---|---|
| `PUT /api/v1/machines/{id}` | Upsert the `machines` row (§3.2). If `home_dir` changed, re-run Project assignment for that Machine (§4.4). |
| `GET /api/v1/machines/{id}/manifest` | Current version of every Raw record for the Machine (§3.3) |
| `POST /api/v1/machines/{id}/records` | Ingest one chunk (§4.2) |
| `GET /api/v1/machines/{id}/health` | `{"sessions_with_warnings": N, "sessions_failed": M}`: this Machine's Sessions with at least one `parse_warnings` row, and those with `parse_status = 'failed'` |

**Web UI** (HTML, §4.7):

| Route | Serves |
|---|---|
| `GET /` | Home: the feed, or search results when `q` is set. Query params: `q`, `machine`, `project` (`-` = "No project"), `source`, `warnings=1`, `before` and `before_id` (feed cursor), `offset` (search paging: hits are ranked, not dated). |
| `GET /sessions/{id}` | A Transcript page. Optional `hl=<terms>` highlights search terms. Messages carry anchors `#m-<message id>`. |
| `GET /sessions/{id}/events` | `text/event-stream`: a `changed` event each time the Session is parsed again from live data (§4.7). `404` for a stub or unknown id. |
| `GET /sessions/{id}/messages?after=<message id>&count=<n>` | htmx fragment: what an open Transcript page needs to catch up after a re-parse (§4.7), or `409` when earlier Messages moved |
| `GET /sessions/{id}/parts/{part id}/output` | htmx fragment: the full output of one Tool call |
| `GET /blobs/{sha256}` | An image blob with its stored MIME type and `Cache-Control: public, max-age=31536000, immutable`. Only PNG, JPEG, GIF and WebP are served as themselves; any other type (SVG included) is served as `application/octet-stream` with `X-Content-Type-Options: nosniff`, so a blob can't run script. |
| `GET /static/…` | Embedded CSS and JS |
| `GET /healthz` | `200 ok` once migrations are done and the database answers `SELECT 1`; `503` otherwise |

- The UI routes have no JSON API. htmx requests get HTML fragments.
- Server timeouts: `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s. No write timeout, since a large Transcript page can take a while to stream and an events stream stays open.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Web UI: browse and search screens](https://github.com/tedkulp/agent-history/issues/13), [Hub language and web UI stack](https://github.com/tedkulp/agent-history/issues/7); UI routes filled in while writing this spec (the prototype's `?session=` URL became `/sessions/{id}`)

### 2.5 Source parser interface (Hub side)

Each Source has one parser in the Hub, in its own package. The adapter spec for each Source (`adapters/*.md`) fills in the details. The Hub core never looks inside a Source format itself.

A **parser** provides:

| Member | Meaning |
|---|---|
| `Source()` | The Source identifier (`protocol.md` §3.1) |
| `Version()` | The parser's `parser_version`: an integer compiled into the Hub, starting at `1`. Bumped whenever the parser's output changes for existing data. |
| `MapKey(recordKey)` | Pure function. Returns `(native Session id, role, Layout name, Layout rank)` or "not mine". `role` is `main` or `attachment`. Called on ingest and on start for unattached records. |
| `Parse(input)` | Turns one Session's Raw records into a `Result`. Must be deterministic: the same input gives the same output. |

`Parse` input:

- the Session's native id (from `MapKey`). Some parsers derive Child Session ids from it (e.g. oh-my-pi).
- the Session's `main` Raw record content (current version, decompressed) from the winning Layout (§4.3), with its Record key
- every `attachment` Raw record of that Layout, keyed by Record key
- the Machine's `home_dir` (some Sources encode paths relative to it)

`Parse` output (`Result`):

- **Session fields**: `title` (from the Source, or empty), `started_at`, `last_activity_at`, `cwd` (starting cwd), `git_branch`, `source_version`, `parent_native_id` + `spawning_call_id` (Child Sessions), `forked_from_native_id` (Codex forks)
- **Messages**, in Transcript order, each with its **Parts** (§3.5)
- **Images**: bytes + MIME type for every `image` Part. The Hub hashes and stores them.
- **Parse warnings** (§3.6), aggregated by `(kind, source_type)`
- or an **error**: no Transcript could be produced (a parse failure, §4.5)

Rules every parser follows:

- The Transcript is **linear**: the path from the root to the latest leaf. Abandoned branches stay in the Raw record only.
- Ordering follows the Source's own sequence (linked-list path, `ordinal`, or id order). Timestamps are for display.
- A request and its result merge into one `tool_call` Part, matched by call id, placed where the call happened. Offloaded results (e.g. Claude `tool-results/*.txt` attachments) are stitched back in.
- Nothing is dropped silently. An unrecognised entry or block type becomes an `unknown` Part **and** an `unknown_type` Parse warning.
- Dropped on purpose (Raw only): system prompts / `base_instructions`, telemetry (Codex `event_msg` / `world_state`, Claude `file-history-snapshot`), cost, thinking signatures and encrypted reasoning.
- A panic inside `Parse` is recovered by the worker and treated as a parse failure.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14); the member list filled in while writing this spec; the native id input filled in by [Write the adapter specs](https://github.com/tedkulp/agent-history/issues/23)

### 2.6 Image and compose file

The image, `ghcr.io/tedkulp/agent-history-hub`:

- Base: `alpine`, with `sqlite` (for debugging with the `sqlite3` shell), `ca-certificates` and `tzdata` (so `TZ` works).
- The static `agent-history-hub` binary (`CGO_ENABLED=0`).
- Runs as a baked-in non-root user, uid/gid `1000`.
- `ENTRYPOINT ["agent-history-hub"]`, `CMD ["serve"]`, `EXPOSE 8080`.
- `HEALTHCHECK CMD agent-history-hub healthcheck`.

Sample `compose.yaml`, shipped in the repo:

```yaml
services:
  hub:
    image: ghcr.io/tedkulp/agent-history-hub:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./backups:/backups
    environment:
      TZ: America/New_York
      # AGENT_HISTORY_BACKUP_KEEP: "7"
    # user: "1000:1000"   # match the owner of ./data and ./backups
```

Documented next to it:

- `/data` must be on a **local disk**, not NFS or SMB. SQLite's WAL breaks on network filesystems.
- If the host directories aren't owned by uid 1000, set compose `user: "UID:GID"`.
- TLS is the operator's choice: a Caddy reverse-proxy snippet is in the docs. The Hub itself speaks plain HTTP.
- Each open Transcript page holds one events stream (§4.7). Over plain HTTP/1.1 a browser allows about 6 connections per host, so a 7th tab on the Hub stalls. A reverse proxy speaking HTTP/2 (Caddy does by default) removes the limit.
- Litestream gets a mention as an optional extra. It is not built in.

Source: [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

## 3. Data

### 3.1 Database file and runtime

- **Everything lives in one SQLite file**, `$AGENT_HISTORY_DATA/hub.db`: raw chunks, image blobs, parsed Transcripts, the search index and the queue. One file to back up.
- Pragmas on every connection: `journal_mode=WAL`, `synchronous=FULL` (an ingest ack promises durability), `busy_timeout=5000`, `foreign_keys=ON`.
- **One writer connection**, shared by ingest, the parse worker, Project re-assignment and start-up work. A pool of reader connections (default 4) serves the UI, manifests, the parse worker's reads and backups.
- All timestamps are stored as integer Unix milliseconds, UTC.

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12); timestamp encoding and reader pool size filled in while writing this spec

### 3.2 Machines

```sql
CREATE TABLE machines (
  id                TEXT PRIMARY KEY,   -- Machine UUID from the Collector
  display_name      TEXT,
  hostname          TEXT,
  os                TEXT,
  arch              TEXT,
  home_dir          TEXT,
  collector_version TEXT,
  sources_json      TEXT,               -- the `sources` array from PUT /machines/{id}
  first_seen_at     INTEGER NOT NULL,
  last_seen_at      INTEGER NOT NULL
);
```

- A request for an unknown Machine id creates a row with only `id`, `first_seen_at` and `last_seen_at` (`protocol.md` §2.2).
- `PUT /machines/{id}` replaces every metadata column with the request body and sets `last_seen_at`.
- The UI labels a Machine by `display_name`, falling back to `hostname`, then to the first 8 characters of `id`.

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10)

### 3.3 Raw records

```sql
CREATE TABLE raw_records (
  id          INTEGER PRIMARY KEY,
  machine_id  TEXT NOT NULL REFERENCES machines(id),
  source      TEXT NOT NULL,
  record_key  TEXT NOT NULL,
  session_id  INTEGER REFERENCES sessions(id),  -- NULL = unattached (key not mappable yet)
  role        TEXT,                              -- main | attachment | shadow; NULL when unattached
  layout      TEXT,                              -- from MapKey; NULL when unattached
  layout_rank INTEGER,                           -- from MapKey; NULL when unattached
  UNIQUE (machine_id, source, record_key)
);
CREATE INDEX raw_records_session ON raw_records(session_id);

CREATE TABLE raw_record_versions (
  id           INTEGER PRIMARY KEY,
  record_id    INTEGER NOT NULL REFERENCES raw_records(id),
  version      INTEGER NOT NULL,     -- 1, 2, 3… per record; returned as `version` by the protocol
  is_current   INTEGER NOT NULL,     -- exactly one current version per record
  length       INTEGER NOT NULL,     -- decompressed bytes
  sha256       TEXT NOT NULL,        -- lowercase hex of the finished hash
  sha256_state BLOB NOT NULL,        -- Go's marshalled sha256 state, extended on each append
  created_at   INTEGER NOT NULL,
  UNIQUE (record_id, version)
);
CREATE UNIQUE INDEX raw_record_versions_current ON raw_record_versions(record_id) WHERE is_current = 1;

CREATE TABLE raw_chunks (
  version_id INTEGER NOT NULL REFERENCES raw_record_versions(id),
  offset     INTEGER NOT NULL,       -- decompressed offset of the chunk's first byte
  bytes      BLOB NOT NULL,          -- zstd-compressed
  PRIMARY KEY (version_id, offset)
);
```

- Each accepted `append` adds **one zstd-compressed chunk row**. A version's content is its chunks in `offset` order.
- A `replace` adds a new version, clears `is_current` on the old one and sets it on the new one. The old version stays as **superseded**. Nothing is ever deleted.
- The prefix-hash check never re-reads stored bytes: the Hub unmarshals `sha256_state`, compares its `Sum` with `X-Prefix-Sha256`, writes the new bytes into it, and stores the new state and hash.
- The **manifest** for a Machine is every record's current version: `(source, record_key) → (length, sha256)`.
- Background compaction of many small chunks into one is allowed later. It is not in v1.

**Rejected:** raw bytes as files on the volume (two stores to back up and keep consistent); one blob per version rewritten on each append (every 2 s delta would rewrite the whole record).

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Write the protocol spec](https://github.com/tedkulp/agent-history/issues/20) (nullable `session_id`)

### 3.4 Sessions

```sql
CREATE TABLE sessions (
  id                      INTEGER PRIMARY KEY,   -- used in URLs and `reparse --session`
  machine_id              TEXT NOT NULL REFERENCES machines(id),
  source                  TEXT NOT NULL,
  native_id               TEXT NOT NULL,
  title                   TEXT,
  first_prompt            TEXT,     -- first user text, truncated to 300 chars, for the feed row
  started_at              INTEGER,
  last_activity_at        INTEGER,
  cwd                     TEXT,     -- starting cwd
  project_cwd             TEXT,     -- NULL = "No project" (§4.4)
  git_branch              TEXT,
  source_version          TEXT,
  model                   TEXT,     -- model of the latest assistant Message, for the header
  parent_session_id       INTEGER REFERENCES sessions(id),
  spawning_call_id        TEXT,
  forked_from_id          INTEGER REFERENCES sessions(id),
  usage_json              TEXT,     -- Session totals, derived from Messages
  parse_status            TEXT NOT NULL DEFAULT 'pending',  -- pending | ok | failed
  parse_error             TEXT,
  parsed_at               INTEGER,  -- last successful parse; NULL = never parsed
  parser_version          INTEGER,  -- version of the last successful parse
  parse_attempted_version INTEGER,  -- version of the last attempt, successful or not
  UNIQUE (machine_id, source, native_id)
);
CREATE INDEX sessions_feed    ON sessions(coalesce(last_activity_at, 0) DESC, id DESC) WHERE parent_session_id IS NULL;
CREATE INDEX sessions_project ON sessions(machine_id, project_cwd);
CREATE INDEX sessions_parent  ON sessions(parent_session_id);
```

- Identity is **(Machine, Source, native id)**. A resume that appends to the same Source file continues the same Session.
- A `sessions` row is created the first time a Raw record maps to it, with `parse_status = 'pending'`.
- **Stub rows**: when a parse names a parent or fork origin that has no row yet, the Hub creates one (`parse_status = 'pending'`, no records) so the link always resolves. A stub gets filled when its own records arrive. Stubs are never shown in the feed (§4.7).
- `title`: from the Source if it has one, else `first_prompt` truncated to 80 characters.
- A failed parse **keeps the previous good Transcript**: only `parse_status`, `parse_error` and `parse_attempted_version` change.
- Project is a column, not a table. Projects have no metadata of their own.
- `sessions.unknown_part_count` from the original storage decision is **dropped**: `parse_warnings` replaces it.

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8); `first_prompt`, `model`, `usage_json` and stub rows filled in while writing this spec

### 3.5 Transcripts

```sql
CREATE TABLE messages (
  session_id INTEGER NOT NULL REFERENCES sessions(id),
  id         TEXT NOT NULL,       -- deterministic Message id
  ordinal    INTEGER NOT NULL,    -- position in the Transcript, 0-based
  role       TEXT NOT NULL,       -- user | assistant | tool
  timestamp  INTEGER,
  model      TEXT,                -- assistant only
  provider   TEXT,                -- assistant only
  usage_json TEXT,                -- assistant only: {input, output, cache_read, cache_write, reasoning}
  PRIMARY KEY (session_id, id)
);
CREATE UNIQUE INDEX messages_order ON messages(session_id, ordinal);

CREATE TABLE parts (
  session_id   INTEGER NOT NULL,
  message_id   TEXT NOT NULL,
  id           TEXT NOT NULL,     -- deterministic Part id
  ordinal      INTEGER NOT NULL,  -- position within the Message
  kind         TEXT NOT NULL,     -- text | thinking | tool_call | image | attachment | marker | unknown
  payload_json TEXT NOT NULL,
  PRIMARY KEY (session_id, id),
  FOREIGN KEY (session_id, message_id) REFERENCES messages(session_id, id)
);
CREATE INDEX parts_order ON parts(session_id, message_id, ordinal);

CREATE TABLE tool_outputs (
  session_id INTEGER NOT NULL,
  part_id    TEXT NOT NULL,
  bytes      BLOB NOT NULL,      -- UTF-8 output text, uncompressed
  PRIMARY KEY (session_id, part_id)
);

CREATE TABLE blobs (
  sha256 TEXT PRIMARY KEY,       -- lowercase hex of bytes
  mime   TEXT NOT NULL,
  bytes  BLOB NOT NULL
);
```

**Stable ids.** Message and Part ids are derived from Source ids: Claude `uuid`, Codex ordinal, omp entry id, opencode `msg_` / `prt_`. Where a Source has none, the id is a hex hash of Session native id + position. Ids must match `[A-Za-z0-9_.:-]{1,128}`; a parser hashes anything else. A Part with no Source id gets `<message id>.<ordinal>`. A re-parse reproduces the same ids, so anchors and links stay valid.

**Part payloads** (`payload_json`):

| Kind | Payload |
|---|---|
| `text` | `{"text": "…"}` (Markdown) |
| `thinking` | `{"text": "…"}` (readable thinking or its summary only) |
| `tool_call` | `{"call_id", "name", "input": <JSON value>, "status": "ok"\|"error"\|"pending", "output": "…"\|null, "output_size": N, "output_preview": "…", "child_sessions": ["<native id>", …], "diff": {"path", "old", "new"}\|null}` |
| `image` | `{"sha256", "mime", "alt": "…"}` |
| `attachment` | `{"label": "…"}`: a file reference or opencode snapshot/patch, no bytes |
| `marker` | `{"marker": "compaction"\|"model_change"\|"thinking_level"\|"slash_command"\|"shell_command", "text": "…", "output": "…"}`. `output` is set on a `shell_command` with output only, and is stored inline (no `tool_outputs` split). |
| `unknown` | `{"source_type": "…", "excerpt": "…"}`: the Source type name and the first 500 bytes of raw JSON |

- **Tool output over 16 KB** (16,384 bytes) goes to `tool_outputs`; the payload's `output` is `null`. Smaller output stays inline in `output`. `output_size` is always set.
- `output_preview` is the first 200 characters of the output, set whenever `output_size` is over 4 KB. The collapsed stub in the UI shows it (§4.7).
- `diff` is set by the parser for file-editing tools (e.g. Claude `Edit`), so the UI can render a diff without knowing Source tool names.
- `child_sessions` lists the native ids of the Child Sessions a call spawned, usually one; an oh-my-pi `task` call can spawn several. `[]` for ordinary calls.
- `blobs` are content-addressed, so the same image stored twice is one row. Blobs are never deleted, even if a re-parse stops referencing them.
- **Usage** is kept per assistant Message. `sessions.usage_json` is the sum. Codex's duplicate usage records are deduped by its parser.

Source: [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Web UI: browse and search screens](https://github.com/tedkulp/agent-history/issues/13); payload field names, the id charset and the preview filled in while writing this spec; `child_sessions` as a list filled in by [Write the adapter specs](https://github.com/tedkulp/agent-history/issues/23)

### 3.6 Parse warnings

```sql
CREATE TABLE parse_warnings (
  session_id    INTEGER NOT NULL REFERENCES sessions(id),
  kind          TEXT NOT NULL,     -- unknown_type | bad_line | missing_field | orphan
  source_type   TEXT NOT NULL,     -- the Source's type or field name involved, '' if none
  count         INTEGER NOT NULL,
  first_excerpt TEXT,              -- short raw JSON sample, at most 500 bytes
  PRIMARY KEY (session_id, kind, source_type)
);
```

| Kind | Meaning |
|---|---|
| `unknown_type` | An entry or block type the parser doesn't know. It also becomes an `unknown` Part. |
| `bad_line` | A line that isn't valid JSON. The Collector never ships a trailing partial line, so it never counts. |
| `missing_field` | An expected field is absent (e.g. `cwd`, a tool call id). The parser falls back and continues. |
| `orphan` | A tool result with no matching call, or a parent / Child Session link that resolves to nothing |

- The rows are deleted and rebuilt in the same transaction as the Session's parse, so they always describe the latest parse.
- An unseen `source_version` is **not** a warning. The warning display shows the Session's `source_version` next to each warning.
- A **parse failure** is not a Parse warning. It lives in `sessions.parse_status` / `parse_error`.
- Totals per Source and per `source_version` are a `GROUP BY` over `parse_warnings` joined to `sessions`. There are no stored rollups.

Source: [Source format drift](https://github.com/tedkulp/agent-history/issues/16)

### 3.7 Full-text search

```sql
CREATE VIRTUAL TABLE search USING fts5(
  body,
  kind       UNINDEXED,   -- 'message' | 'title'
  session_id UNINDEXED,
  message_id UNINDEXED,   -- NULL for title rows
  tokenize = 'unicode61 remove_diacritics 2'
);
```

- **One row per Message**, so every hit links to a Message anchor. The body is the Message's `text` Parts joined by newlines, plus, for each `tool_call` Part, its name and its input as compact JSON.
- **Not indexed:** tool output, thinking, markers, `unknown` Parts, attachments. File dumps and logs would drown the signal.
- **One `title` row per Session**, with the title as body, so title hits rank higher.
- Rows are deleted and rebuilt with the Session's parse, in the same transaction.

**Query translation.** The search box is plain text, not FTS5 syntax. The Hub splits the input on whitespace, keeps `"quoted phrases"` together, turns FTS5 operator characters into spaces (so `foo-bar` is the phrase `"foo bar"`, as the tokenizer splits it), drops terms with no letter or digit, wraps each term in double quotes, and ANDs them. The last term gets a `*` prefix match. Input that leaves no terms shows the feed.

**Ranking:** `ORDER BY bm25(search) * CASE kind WHEN 'title' THEN 2.0 ELSE 1.0 END` (bm25 is negative; lower is better, so title hits count double).

**Snippets:** `snippet(search, 0, '', '', '…', 24)`. The Hub HTML-escapes the result, then swaps the two private-use sentinels for `<mark>` and `</mark>`, so Transcript text can never inject HTML.

**Rejected:** indexing tool output (can be revisited); one row per Session (a hit couldn't link to its Message).

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Normalized Transcript model](https://github.com/tedkulp/agent-history/issues/9), [Web UI: browse and search screens](https://github.com/tedkulp/agent-history/issues/13); tokenizer, query translation, ranking expression and snippet escaping filled in while writing this spec

### 3.8 Parse queue

```sql
CREATE TABLE parse_queue (
  session_id  INTEGER PRIMARY KEY REFERENCES sessions(id),
  priority    INTEGER NOT NULL,   -- 0 = live ingest, 1 = re-parse
  enqueued_at INTEGER NOT NULL,
  not_before  INTEGER NOT NULL
);
```

- Keyed by Session, so repeated appends collapse into one job.
- **Live enqueue** (on ingest): insert with `priority = 0`, `enqueued_at = now`, `not_before = max(now, sessions.parsed_at + 10 s)`. On conflict: set `priority = 0` and `enqueued_at = max(now, enqueued_at + 1)`, keep `not_before`. `enqueued_at` always moves forward, so the worker's unchanged check (§4.5) sees data that arrived in the same millisecond as its pick. A live append to a Session already queued at priority 1 therefore bumps it to 0, and the one job covers both. Each Session is parsed at most once every **10 s**.
- **Re-parse enqueue** (start-up check, `reparse` CLI, unattached-record mapping): insert with `priority = 1`, `not_before = now`. On conflict: leave the row alone (it's either already priority 1 or has the higher priority 0).

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14); the exact upsert rules filled in while writing this spec

### 3.9 Migrations

- Numbered `.sql` files (`0001_init.sql`, `0002_…`) embedded in the binary. No migration library.
- The current version is `PRAGMA user_version`. Each migration runs in its own transaction and sets `user_version` at its end.
- **Forward-only.** No down-migrations.
- A migration that changes the shape of parsed tables sets `parser_version = NULL` and `parse_attempted_version = NULL` on the affected Sessions. The start-up check (§4.5) then re-queues them. There is no separate rebuild path.

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15)

## 4. Behavior

### 4.1 Start-up and shutdown

`serve` starts in this order:

1. Parse config. Invalid config → exit `1`.
2. Open `hub.db` (creating it if missing) and set the pragmas.
3. If `user_version` is **higher** than the newest migration this binary knows, exit `1`: "database is from a newer Hub; restore a pre-migrate backup to roll back". This stops an old image from running against a newer schema.
4. If migrations are pending and the database is not new (`user_version > 0`), write `VACUUM INTO $AGENT_HISTORY_BACKUP_DIR/pre-migrate-<user_version>.db`. If that fails, exit `1` without migrating.
5. Apply pending migrations.
6. **Map unattached records**: for every `raw_records` row with `session_id IS NULL`, call `MapKey` on its Source's parser. On success, attach it (§4.3) and enqueue its Session at priority 1.
7. **Re-parse check** (§4.5): enqueue stale Sessions at priority 1.
8. Start the parse worker, the backup scheduler and the HTTP server. `/healthz` returns `200` from here on.

Steps 6 and 7 are single `INSERT … SELECT` style passes on the writer. They log counts at `info`.

On `SIGTERM` / `SIGINT`: stop accepting connections, end open events streams, let in-flight requests finish (up to 10 s), let the worker finish its current write transaction (it drops any parse still running outside the transaction; the queue row stays), run `PRAGMA wal_checkpoint(TRUNCATE)`, close the database, exit `0`.

Source: [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Write the protocol spec](https://github.com/tedkulp/agent-history/issues/20) (unattached records); ordering, the newer-schema guard and shutdown filled in while writing this spec

### 4.2 Ingest

The request handling for `POST /records` is in `protocol.md` §4.4. On the Hub side, inside the one write transaction:

1. Check the minimum Collector version from `User-Agent` (`protocol.md` §4.6). Below it → `426`.
2. Look up or create the `raw_records` row. On creation, call `MapKey`:
   - mapped → look up or create the Session row, set `session_id`, `layout`, `layout_rank`, and a role (§4.3)
   - not mapped (unknown Source or unknown Layout prefix) → leave `session_id` and `role` NULL. The bytes are still stored and acked.
3. Apply the append or replace to the current version (§3.3).
4. If the record is attached with role `main` or `attachment`, live-enqueue its Session (§3.8). A `shadow` record does not enqueue.
5. Commit, then signal the parse worker.

**The `200` ack means the raw bytes and the queue row are committed.** Parsing happens afterwards. A parser failure or panic never loses raw data and never blocks ingest.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12)

### 4.3 Record → Session mapping and Layout ranks

- Each parser's `MapKey` is a pure function of the Record key. Example: Claude `<cwd>/<session>.jsonl` is `main`; `<cwd>/<session>/tool-results/*.txt` is `attachment`; a Child Session's file maps to its **own** Session.
- The Collector never sends a Session id. Upload order doesn't matter: an `attachment` that arrives before its `main` waits, and the Session parses once a `main` exists.
- **Layout ranks.** When one native Session id has Raw records in two Layouts (e.g. an opencode Session migrated into the database while its legacy JSON files remain), all of them attach to the same Session. The Session's **winning Layout** is the highest `layout_rank` among its records whose `MapKey` role is `main`. Records from any other Layout get role **`shadow`**: they are kept Raw-only and never parsed.
- Roles are recomputed whenever a record attaches to a Session. If the winner changes (a higher-ranked Layout appears), the old winner's records become `shadow` and the Session is live-enqueued.
- If the winning Layout holds more than one `main` for the Session (e.g. an oh-my-pi file re-shipped under a new key after omp renamed its directory), the one with the highest `raw_records.id` is parsed and the others become `shadow`.

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); the several-`main` rule filled in by [Write the adapter specs](https://github.com/tedkulp/agent-history/issues/23)

### 4.4 Project assignment

A **Project** is **(Machine, starting cwd)**. No git probing, no merging across Machines.

`project_cwd` is computed from the parsed `cwd` and the Machine's `home_dir`, after cleaning both paths (`path.Clean`, no trailing slash):

| Starting cwd | `project_cwd` |
|---|---|
| empty or unknown | `NULL` ("No project") |
| equal to the Machine's `home_dir` | `NULL` |
| `/tmp`, `/private/tmp`, `/var/tmp`, or under `/tmp/`, `/private/tmp/`, `/var/tmp/`, `/var/folders/`, `/private/var/folders/` | `NULL` |
| anything else | the cwd |

- The key is the cwd the Session **started** in. A later `cd` never moves a Session.
- The same path on two Machines is two Projects. Search still spans all Machines.
- Monorepo sub-directories and worktrees are separate Projects. Accepted for v1.
- It's set at parse time. When `PUT /machines/{id}` changes `home_dir`, the Hub recomputes `project_cwd` for every Session of that Machine in one transaction, with no re-parse.
- The Collector doesn't send `$TMPDIR`. On macOS it lives under `/var/folders/`, and on Linux it's normally `/tmp`, so the prefix list covers the usual cases.
- Display name: the cwd's basename, qualified by Machine where more than one Machine is in view. The full path is shown on hover.

**Rejected:** keying by git remote or root-commit SHA. Only Codex records the remote, so the other Sources would need a Collector-side git probe.

Source: [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12); the exact temp-dir list filled in while writing this spec

### 4.5 Parse worker and Re-parse flow

**One worker goroutine** drains `parse_queue`.

Picking a job:

- Only rows with `not_before <= now`.
- All priority 0 rows first, oldest `not_before` first.
- Then priority 1 rows, the Session with the most recent `last_activity_at` first, so Sessions people are likely to open refresh early.
- When nothing is due, the worker sleeps until the earliest `not_before`, a signal from ingest, or 5 s, whichever comes first. The 5 s poll picks up rows written by a `reparse` CLI in another process.

Running a job:

1. On a reader connection, load the Session's current `main` and `attachment` records from the winning Layout, decompressing their chunks. If there's no `main`, delete the queue row and stop: the `main`'s arrival will enqueue it again.
2. Call `Parse` **outside any write transaction**.
3. In one write transaction:
   - **Success**: delete the Session's `messages`, `parts`, `tool_outputs`, `search` rows and `parse_warnings`; insert the new ones; insert new `blobs` (`INSERT OR IGNORE`); update the Session fields, `project_cwd` (§4.4), `usage_json`, `parse_status = 'ok'`, `parse_error = NULL`, `parsed_at = now`, `parser_version` and `parse_attempted_version` = current; create stub rows for an unknown parent or fork origin (§3.4).
   - **Failure** (error or recovered panic): set `parse_status = 'failed'`, `parse_error`, `parse_attempted_version` = current. If `last_activity_at` is NULL (never parsed), set it to now, so the Session sorts in the feed. Leave the previous Transcript, search rows and warnings untouched.
   - Delete the queue row **only if its `enqueued_at` is unchanged** since step 1. If new data arrived meanwhile, keep the row and set its `not_before = now + 10 s`.
4. After a Session's **first** successful parse, re-enqueue at priority 1 any Session that names it as parent and has an `orphan` warning, so a Child Session parsed before its parent clears its warning.
5. After a successful save of a **priority 0** job, publish the Session, and its parent if it is a Child Session (whose Child Session list may have gained it), to the in-process broadcaster, which tells their open Transcript pages (§4.7). Priority 1 re-parses publish nothing, so a re-parse storm after a `parser_version` bump doesn't hit open tabs. Publishing never blocks the worker: each subscriber has a buffered channel of one, events coalesce, and a subscriber that isn't reading misses only duplicates.

Only the delete+insert holds the single writer. An ingest ack waits for at most one Session's write, never for the backlog.

**Versioning.** Each Source parser has its own `parser_version` integer. A Session needs a re-parse when its stored `parser_version` **differs** from the running parser's (not just lower), so rolling back to an older image re-derives with the parser that image knows.

**Triggers.**

- **Automatic on start** (§4.1 step 7): enqueue at priority 1 every Session whose `parser_version` differs from current (including `NULL`), **except** Sessions with `parse_status = 'failed'` and `parse_attempted_version` equal to current. A failing Session doesn't re-fail on every restart. Upgrading the image is the only action needed.
- **Manual**: `agent-history-hub reparse --all | --source <source> | --session <id>` enqueues at priority 1 and ignores the failure-skip rule.
- **Live data**: a new append retries a failed Session.

**Throttling.** Priority only. No rate limit: at under 10 Machines and a few GB, a full backlog finishes in minutes.

**Rejected:** incremental parsing (every parser would need resumable state, and a new latest leaf can reshuffle earlier Messages); one global parser version (a fix to one Source would re-parse all four); lazy re-parse on open (search would stay stale); a rate limit on top of priority; a UI re-parse button.

Source: [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); the `enqueued_at` race rule, the 5 s poll and the orphan re-queue filled in while writing this spec

### 4.6 Rendering rules

These apply wherever Transcript content is shown.

- `text` Parts render as Markdown with goldmark (GFM: tables, strikethrough, autolinks, task lists). **Raw HTML in Markdown is escaped, never rendered.** Transcripts contain arbitrary tool output and pasted HTML.
- Fenced code blocks are highlighted with chroma using CSS classes (not inline styles), with one light and one dark stylesheet selected by `prefers-color-scheme`.
- Tool input renders as pretty-printed JSON. Tool output renders as preformatted text, not Markdown.
- A `diff` payload renders as a line-level unified diff (`github.com/sergi/go-diff` line mode), with the file path as its heading.
- `image` Parts render inline from `/blobs/{sha256}`, scaled down to the column width, opening full size on click.
- Times show in the container's `TZ`.

Source: [Hub language and web UI stack](https://github.com/tedkulp/agent-history/issues/7), [Web UI: browse and search screens](https://github.com/tedkulp/agent-history/issues/13); HTML escaping, chroma classes and the diff library filled in while writing this spec

### 4.7 Web UI

The UI is **search-first**: a search box over a feed of recent Sessions from every Machine. Machine, Source and Project are **filter chips**, not pages. The throwaway prototype is in [`docs/prototype/webui/`](../prototype/webui/): run `go -C docs/prototype/webui run .`, open http://127.0.0.1:8791/?v=C. Variant C is the chosen design.

#### Home (`/`, no `q`)

- A large search box at the top, focused on load. It searches every Transcript on every Machine.
- **Chips**, each toggling a URL param. All state lives in the URL (`machine`, `project`, `source`, `warnings`, `q`); chips combine and toggle off.
  - A **Machine** row: one chip per Machine.
  - A **Source** row: one chip per Source that has Sessions.
  - Picking a Machine reveals that Machine's **Project** chips with Session counts (`GROUP BY project_cwd`), plus "No project". This is the Machine → Project → Session browse path.
  - A **"has warnings"** chip: Sessions with any `parse_warnings` row, or with `parse_status = 'failed'`.
- **Feed**: top-level Sessions (`parent_session_id IS NULL`) that have been parsed (`parsed_at IS NOT NULL`) with at least one Message, **or** have failed (`parse_status = 'failed'`), matching the chips, newest `last_activity_at` first, grouped by day: "Today", "Yesterday", then the date.
  - A parsed Session with no Messages (a launch where nothing happened, such as only `/model`) is left out of the feed and its chip counts. Its page still opens by URL.
  - A Session whose parse failed and that never parsed successfully has no title or first prompt. Its row shows the native id and a **"parse failed"** badge, and links to its Transcript page, which shows only the failure note. So a parser bug can't hide a Session.
- Each row: last-activity time, title, `first_prompt` (one line, truncated), a Source badge, and Machine › Project. The row links to `/sessions/{id}`.
- 50 rows per page. A "Load more" button fetches the next page with htmx (`before=<last_activity_at of the last row>&before_id=<its id>`; the id breaks ties so paging neither skips nor repeats rows).
- **Banners** above the feed:
  - **Re-parse**: "Re-parsing N Sessions…" while priority 1 rows remain in `parse_queue`. N is the current count.
  - **Drift**: one banner per `(source, source_version)` with warnings, e.g. "Codex 0.52: 14 Sessions have unrecognised data". It shows only when at least one Session active in the last **7 days** has warnings for that pair, so old noise fades. It links to `/?warnings=1&source=<source>`.

#### Search results (`/?q=…`)

- A flat list of hits, 50 per page with "Load more" (`offset=`). Each hit shows the Session title (terms marked), a snippet around the match (§3.7), and Source · Machine › Project · the Message's role.
- A hit links to `/sessions/{id}?hl=<q>#m-<message id>`. A title hit links to the top of the Transcript.
- Hits in Child Sessions are included and labelled "↳ child of <parent title>".
- A **facet sidebar** shows hit counts per Machine, Source and Project. Clicking a count adds that chip.
- Active chips apply to the search too. Ordering is bm25 with title boost (§3.7).

#### Transcript (`/sessions/{id}`)

- **Header**: title, Source, Machine and Project (both link to the feed filtered by them), git branch, model, start time, and "↰ child of …" (linking to the spawning call's anchor in the parent) for a Child Session, or "forked from …" for a fork.
  - The spawning call is `spawning_call_id` when the parser set it. Otherwise the Hub looks in the parent's `tool_call` Parts for one whose `child_sessions` contains this Session's native id. If neither finds a call, the link goes to the top of the parent.
- **Notes** under the header, collapsed by default:
  - Parse warnings: e.g. "12 items not understood (unknown_type: `foo_event` ×10, …)", with `source_version` and each `first_excerpt` on expand.
  - Parse failure: "parse failed" with `parse_error`, shown above the last good Transcript (or alone, if there never was one).
- **Chat bubbles**: user Messages on the right, assistant Messages on the left, the timestamp under each. Every Message has the anchor `id="m-<message id>"`. All Messages render on one page (Ctrl-F works); there's no pagination.
- **Tool calls**: each run of consecutive `tool_call` Parts folds into one collapsed **cluster**, labelled like `⚙ 3 tool calls · Read, Edit, Bash`. A failed call is marked ✗ in the label. Opening the cluster lists each call as its own collapsible row. An open row shows the input and the output, or the rendered diff (§4.6).
- **Large output**: output over 4 KB stays collapsed as a stub showing its size and `output_preview`. Clicking loads `/sessions/{id}/parts/{part id}/output` with htmx. The endpoint serves `tool_outputs` when the output was split out, else the inline `output`.
- `thinking` Parts are collapsed (💭). `marker` Parts are centred pills. A `shell_command` reads the whole `$ <command>` in monospace, line breaks kept, as a left-aligned block with slightly rounded corners rather than a round pill, so a long command stays readable. With `output`, the pill is a `<details>` that opens below to show the output as preformatted text, never Markdown, HTML-escaped like tool output. Output over 4 KB is cut to its first 4 KB with a "… N KB more" note; there's no lazy-load endpoint, since the output lives in the payload. `unknown` Parts are a visible warning bubble with the Source type. `attachment` Parts are a 📎 label.
- **Child Sessions**: a cluster containing a call that spawned one starts open, and that call shows a "↳ Child Session" link. The child's header links back to the spawning call.
- A **sticky left outline** lists the user's prompts (first line of each), each linking to its anchor. The Child Sessions are listed below the prompts.
- **Live updates**: the page opens an `EventSource` on `/sessions/{id}/events` and updates in place when its Session is parsed again from live data.
  - The stream sends `event: changed` after each live parse (§4.5 step 5), a heartbeat comment every 30 s, flushes on every write and sets `X-Accel-Buffering: no`. Once the Session is found it reads nothing from the database, so an idle open page costs no queries, and the page itself runs no timers or polling. It ends when the client disconnects or the Hub shuts down.
  - On `changed`, and whenever the stream (re)opens, the page fetches `/sessions/{id}/messages?after=<its last Message id>&count=<its Message count>`. Both count every stored Message, including ones with nothing renderable yet.
  - If the Message at ordinal `count-1` still has id `after`, the fragment holds that Message re-rendered (it may have grown, or a tool call may have finished) plus every later one, their outline entries, and the header and Child Session list afresh. The page swaps Messages and outline entries in by id and appends new ones. Open `<details>` stay open, the scroll position stays put, and the page follows new Messages only when the reader was already at the bottom.
  - Otherwise (a rewind, a new branch or a compaction reshuffled earlier Messages) the endpoint answers `409`, and the page shows a "Transcript changed — reload" banner instead of patching.
  - An update costs one Message query from `count-1` on plus the header, well under a full page render.
- **Arriving from search**: the target Message flashes briefly, and each `hl` term is wrapped in `<mark>` in the rendered text, case-insensitive. Highlighting touches text nodes only, never tags or attributes.

**Rejected:** drill-down pages (a page per level is slow for the most common action, "find that conversation"); a three-pane reader (tool calls always expanded bury the conversation, and three panes are cramped on narrow screens).

Source: [Web UI: browse and search screens](https://github.com/tedkulp/agent-history/issues/13), [Hub language and web UI stack](https://github.com/tedkulp/agent-history/issues/7), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Source format drift](https://github.com/tedkulp/agent-history/issues/16); page sizes, the `before` cursor and the Child Session search label filled in while writing this spec; the spawning-call lookup filled in by [Write the adapter specs](https://github.com/tedkulp/agent-history/issues/23)

### 4.8 Backups

- **Scheduled**: every day at `AGENT_HISTORY_BACKUP_AT`, the Hub runs `VACUUM INTO` on a reader connection, so ingest keeps going. It writes to `hub-YYYYMMDD.db.tmp` in `AGENT_HISTORY_BACKUP_DIR` and renames it to `hub-YYYYMMDD.db` when done.
- **Retention**: after each scheduled backup, delete all but the newest `AGENT_HISTORY_BACKUP_KEEP` files matching `hub-????????.db`. `pre-migrate-*.db` and manual backups are never pruned automatically.
- **Manual**: `agent-history-hub backup` writes `hub-YYYYMMDD-HHMMSS.db` the same way.
- **Pre-migration**: `pre-migrate-<user_version>.db` before any migration (§4.1).
- A failed backup logs at `error` and is retried at the next scheduled time. It never stops the Hub.
- The host's own backup tool picks up `/backups`; every file there is a consistent SQLite database.
- **Rollback** of a bad upgrade: stop the Hub, copy the `pre-migrate-*.db` over `hub.db` (and delete `hub.db-wal` / `-shm`), run the old image.

**Rejected:** Litestream built in (a docs mention only); volume snapshots (not consistent under WAL without care).

Source: [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15); the temp-file rename, the retention glob and the manual filename filled in while writing this spec

### 4.9 Logging

- `log/slog` text to stdout at `AGENT_HISTORY_LOG_LEVEL`.
- `info`: start-up steps with counts, migrations, backups, Machine registration, each `426`.
- `debug`: each ingest chunk and each parse with its duration.
- `warn`: parse failures (Session id, Source, error), `409`s, `4xx` responses.
- `error`: backup failures, database errors.

Source: [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15); level assignments filled in while writing this spec

### 4.10 Error cases

| Situation | Behavior |
|---|---|
| Parser returns an error or panics | Session `failed` with `parse_error`, previous Transcript kept, queue row deleted. Retried on new data or `reparse`, and not on restart (§4.5). |
| Record key not mappable | Stored and acked with `session_id` NULL; retried on every start (§4.1) |
| Attachment with no `main` yet | Stored and attached; the Session parses when `main` arrives |
| Database locked beyond `busy_timeout` | Ingest returns `503`; the Collector backs off (`protocol.md` §4.5) |
| Disk full | Ingest returns `507` with `{"error": "insufficient_storage"}`; the Collector backs off like a `5xx`. Logged at `error`. |
| `hub.db` from a newer Hub | `serve` exits `1` with a message (§4.1) |
| Pre-migration backup fails | `serve` exits `1` without migrating |
| Scheduled backup fails | Logged at `error`; retried next day |
| `/sessions/{id}` for an unknown id, or a stub | `404` page |

Source: [Hub storage schema](https://github.com/tedkulp/agent-history/issues/12), [Re-parse flow when a parser changes](https://github.com/tedkulp/agent-history/issues/14), [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15); `503` / `507` filled in while writing this spec

### 4.11 Release (Hub side)

- One lockstep `vX.Y.Z` tag, pushed by a human, releases both the Collector and the Hub. GoReleaser (OSS), driven by GitHub Actions, builds the image and injects the version via ldflags.
- Image: `ghcr.io/tedkulp/agent-history-hub`, `linux/amd64` + `linux/arm64`, tagged **`vX.Y.Z`, `vX.Y` and `latest`**. No `vX` tag, no `edge` or `main` builds in v1.
- Pre-release tags (`vX.Y.Z-rc.N`) publish the `vX.Y.Z-rc.N` image tag only, and never move `vX.Y` or `latest`.
- The image gets a GitHub artifact attestation (`actions/attest-build-provenance`). No cosign, no SBOM in v1.
- CI on PRs and `main` runs `goreleaser release --snapshot`, which builds the image without publishing it.
- The compiled-in `MinCollectorVersion` and its bump rule are in `protocol.md` §4.6. Release-wide rules are in [`README.md`](README.md).

Source: [Release pipeline](https://github.com/tedkulp/agent-history/issues/17), [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15)

## 5. Out of scope

- Authentication, authorization and TLS (VPN only; a reverse proxy is the operator's choice).
- Analytics: token, cost and activity dashboards. Usage is stored per Message but not charted.
- Secret redaction in Transcripts. Raw retention keeps it possible later.
- Deleting anything: Sessions removed on a Machine, superseded versions and unreferenced blobs are all kept.
- Merging Projects across Machines, or keying Projects by git remote.
- Incremental parsing.
- Indexing tool output or thinking for search.
- A re-parse button, a failed-Sessions page, or any admin UI.
- A JSON API for the Web UI.
- Live updates of an open Transcript page (polling or push). A reload shows new Messages.
- Reading Codex's `thread_history_*.sqlite` store. Its records never reach the Hub in v1.
- Chunk compaction and any storage reclaim.
- Litestream or other replication built into the Hub.

## 6. M1 acceptance checklist

M1 is Claude Code end to end. Only the `claude-code` parser needs to exist.

- [ ] `docker compose up` with the sample `compose.yaml` starts the Hub as uid 1000, creates `/data/hub.db`, and the container reports healthy.
- [ ] `agent-history-hub version` prints the ldflags-injected version; the image is published to GHCR for amd64 and arm64 by a `vX.Y.Z` tag.
- [ ] Migrations run on first start; a second start with a newer migration writes `pre-migrate-<n>.db` first; a database from a newer Hub makes `serve` exit.
- [ ] A Collector's uploads produce Raw records whose current version's decompressed chunks are byte-identical to the Source files.
- [ ] Each Claude Code Session file becomes a Session; its `tool-results/*.txt` records attach to it as `attachment`; Child Session files become their own Sessions linked to the parent and the spawning Tool call.
- [ ] A Raw record with an unknown Source is stored and acked with `session_id` NULL.
- [ ] A live append is parsed within about 10 s; a burst of appends to one Session causes at most one parse per 10 s.
- [ ] Re-parsing a Session reproduces the same Message and Part ids.
- [ ] A parser panic marks the Session `failed`, keeps its previous Transcript, and does not affect ingest acks.
- [ ] A Session whose first parse fails appears in the feed with a "parse failed" badge, matches the "has warnings" chip, and counts in `sessions_failed` on `GET /health`.
- [ ] Bumping the `claude-code` `parser_version` and restarting re-queues every Claude Code Session at priority 1; the home banner counts them down; live ingest is still parsed first.
- [ ] A Session that failed at the current `parser_version` is not re-queued on restart, but `reparse --session <id>` retries it.
- [ ] Sessions started in the home dir or `/tmp` land in "No project"; changing `home_dir` via `PUT /machines/{id}` reassigns Projects without a re-parse.
- [ ] Home shows the day-grouped feed of top-level Sessions across Machines; Machine, Project, Source and "has warnings" chips filter it and live in the URL.
- [ ] Search finds a word from a user prompt, an assistant reply, and a tool call's input, but not from tool output or thinking; a title match ranks above a body match.
- [ ] A search hit opens the Transcript at the right Message, flashes it, and highlights the term.
- [ ] Search input containing FTS5 syntax (`"`, `*`, `AND`, `(`) never causes an error.
- [ ] The Transcript shows bubbles, folded tool-call clusters with ✗ on failures, a rendered diff for an Edit, collapsed thinking, and the prompt outline.
- [ ] Tool output over 4 KB is collapsed and loads on click; output over 16 KB is served from `tool_outputs`.
- [ ] HTML in a Transcript (in text or tool output) is shown as text, never rendered.
- [ ] An unknown Claude Code entry type appears as an `unknown` Part, a Parse warning note on the Transcript, and in `GET /api/v1/machines/{id}/health`.
- [ ] The daily backup writes `hub-YYYYMMDD.db` into `/backups` and keeps only the newest 7; `agent-history-hub backup` writes one on demand; each opens cleanly in `sqlite3`.
- [ ] `SIGTERM` stops the Hub cleanly; the next start loses no acked data.
