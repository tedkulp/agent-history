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
  session_id  INTEGER,                -- NULL = unattached; references sessions(id) once that table exists
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
