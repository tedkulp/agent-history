-- Until the first release tag, schema changes extend this migration in place.

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

CREATE TABLE raw_records (
  id          INTEGER PRIMARY KEY,
  machine_id  TEXT NOT NULL REFERENCES machines(id),
  source      TEXT NOT NULL,
  record_key  TEXT NOT NULL,
  session_id  INTEGER REFERENCES sessions(id),  -- NULL = unattached (key not mappable yet)
  role        TEXT,                   -- main | attachment | shadow; NULL when unattached
  layout      TEXT,                   -- from MapKey; NULL when unattached
  layout_rank INTEGER,                -- from MapKey; NULL when unattached
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
  project_cwd             TEXT,     -- NULL = "No project" (hub.md §4.4)
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
CREATE INDEX sessions_feed    ON sessions(last_activity_at DESC) WHERE parent_session_id IS NULL;
CREATE INDEX sessions_project ON sessions(machine_id, project_cwd);
CREATE INDEX sessions_parent  ON sessions(parent_session_id);

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

CREATE TABLE parse_warnings (
  session_id    INTEGER NOT NULL REFERENCES sessions(id),
  kind          TEXT NOT NULL,     -- unknown_type | bad_line | missing_field | orphan
  source_type   TEXT NOT NULL,     -- the Source's type or field name involved, '' if none
  count         INTEGER NOT NULL,
  first_excerpt TEXT,              -- short raw JSON sample, at most 500 bytes
  PRIMARY KEY (session_id, kind, source_type)
);

CREATE TABLE parse_queue (
  session_id  INTEGER PRIMARY KEY REFERENCES sessions(id),
  priority    INTEGER NOT NULL,   -- 0 = live ingest, 1 = re-parse
  enqueued_at INTEGER NOT NULL,
  not_before  INTEGER NOT NULL
);

-- One row per Message (its text Parts plus each Tool call's name and input)
-- and one title row per Session (hub.md §3.7). Rebuilt with each parse.
CREATE VIRTUAL TABLE search USING fts5(
  body,
  kind       UNINDEXED,   -- 'message' | 'title'
  session_id UNINDEXED,
  message_id UNINDEXED,   -- NULL for title rows
  tokenize = 'unicode61 remove_diacritics 2'
);
