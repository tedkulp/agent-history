# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
One version covers the Collector and the Hub together (see
[docs/releasing.md](docs/releasing.md)).

## [Unreleased]

### Added

- The home feed shows "↑ N Sessions updated" when Sessions it lists get live
  data. The rows stay put until the pill is clicked, which reloads the feed
  with the same chips.
- An open Transcript page updates in place as its Session is parsed from
  live data. It keeps the scroll position and open tool calls, and offers a
  reload when earlier Messages changed.
- `!` shell commands in Claude Code Sessions render as their own block, with
  the command's output behind a toggle.
- Hub backups: a backup before each schema migration, daily scheduled backups
  (`AGENT_HISTORY_BACKUP_AT`, pruned to `BACKUP_KEEP`), and
  `agent-history-hub backup` on demand.
- The Hub shuts down cleanly on SIGTERM or SIGINT: it drains HTTP for up to
  10 s, finishes a save in progress and checkpoints the database.
- Ingest answers `503` while the database stays locked and `507` when the
  disk is full.

### Changed

- The Collector no longer needs mise: install it by downloading the release
  archive into `~/.local/bin`. `init` and `service install` run the mise shim
  when it exists, otherwise the binary you ran. `--shim` is renamed
  `--exec`; `--shim` still works for this release.

### Fixed

- A Message holding only markers no longer draws the marker's pill shape
  behind the whole row.

## [0.1.0-rc.2] - 2026-09-27

### Added

- Collector: ships Claude Code history raw to the Hub, live, with a local
  cache, file watching, debounce, periodic rescans and recovery after a Hub
  outage.
- Collector: `init`, `set-name`, `status` and `sync` commands, a TOML config,
  excluding Sessions by starting directory, and running as a per-user
  service through the mise shim.
- Collector: reports Layout drift (unclaimed and known-ignored paths),
  restarts itself after an upgrade, and rotates its log on macOS.
- Hub: ingestion API with chunking, replaced records and error codes, and a
  minimum Collector version (`426` for an older Collector).
- Hub: a search-first home feed of Sessions from every Machine, with
  Machine, Source, Project and "has warnings" chips and paging.
- Hub: full-text search across every Transcript, with facets and the
  matching Message highlighted.
- Hub: Transcript pages with tool calls, diffs, images, branches, markers,
  Child Sessions and Parse warnings.
- Hub: re-parses Sessions when a parser changes, shows parse failures, and
  reports Machine health.
- Hub container image, compose file, healthcheck and environment config.
- Release pipeline: CI, GoReleaser archives for the Collector and a
  multi-arch Hub image, published from a version tag.

[Unreleased]: https://github.com/tedkulp/agent-history/compare/v0.1.0-rc.2...HEAD
[0.1.0-rc.2]: https://github.com/tedkulp/agent-history/releases/tag/v0.1.0-rc.2
