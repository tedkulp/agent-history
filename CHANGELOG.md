# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
One version covers the Collector and the Hub together (see
[docs/releasing.md](docs/releasing.md)).

## [Unreleased]

### Added

- Codex support: the Collector ships Codex history from `~/.codex` (or `CODEX_HOME`, written by re-running `agent-history init`), and the Hub shows it as Transcripts with Child Sessions.
- A forked Session shows "forked from …" in its Transcript header, linking to the Session it came from.
- A Transcript shows a "↑ Top" link once you scroll past its header; it returns to the top and focuses the heading without adding a browser history entry.
- Background task and sub-agent notifications (`<task-notification>`) show in the Transcript as a compact marker with status, summary and usage, linked to the tool call that started the task, instead of raw text in a user bubble. Existing Sessions pick this up when they re-parse.

### Changed

- The sample `compose.yaml` pins the Hub image to `v0.1` instead of `latest`, so `docker compose pull` takes patch releases but not a new minor version.

## [0.1.0] - 2026-09-27

### Added

- Collector: ships Claude Code history raw to the Hub, live, with a local
  cache, file watching, debounce, periodic rescans and recovery after a Hub
  outage.
- Collector: `init`, `set-name`, `status` and `sync` commands, a TOML config,
  excluding Sessions by starting directory, and running as a per-user
  launchd or systemd service.
- Collector: install by downloading the release archive into `~/.local/bin`,
  or with mise. The service runs the mise shim when it exists, otherwise the
  binary you ran; `--exec <path>` picks another.
- Collector: reports Layout drift (unclaimed and known-ignored paths),
  restarts itself after an upgrade, and rotates its log on macOS.
- Hub: ingestion API with chunking, replaced records and error codes, and a
  minimum Collector version (`426` for an older Collector). Ingest answers
  `503` while the database stays locked and `507` when the disk is full.
- Hub: a search-first home feed of Sessions from every Machine, with
  Machine, Source, Project and "has warnings" chips and paging. It shows
  "↑ N Sessions updated" when listed Sessions get live data.
- Hub: full-text search across every Transcript, with facets and the
  matching Message highlighted.
- Hub: Transcript pages with tool calls, diffs, images, branches, markers,
  Child Sessions, Parse warnings and `!` shell commands. An open page
  updates in place as its Session gets live data.
- Hub: re-parses Sessions when a parser changes, shows parse failures, and
  reports Machine health.
- Hub: backups before each schema migration, daily scheduled backups
  (`AGENT_HISTORY_BACKUP_AT`, pruned to `BACKUP_KEEP`), and
  `agent-history-hub backup` on demand.
- Hub: clean shutdown on SIGTERM or SIGINT, draining HTTP for up to 10 s and
  checkpointing the database.
- Hub container image, compose file, healthcheck and environment config.
- Release pipeline: CI, GoReleaser archives for the Collector and a
  multi-arch Hub image, published from a version tag.

[Unreleased]: https://github.com/tedkulp/agent-history/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/tedkulp/agent-history/releases/tag/v0.1.0
