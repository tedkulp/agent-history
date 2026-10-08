# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
One version covers the Collector and the Hub together (see
[docs/releasing.md](docs/releasing.md)).

## [Unreleased]

## [0.7.0] - 2026-10-08

### Changed

- Clicking an image in a Transcript opens it full size in an overlay on the page instead of a new tab. Ctrl/Cmd-click or middle-click still opens a new tab.

## [0.6.0] - 2026-10-07

### Added

- A "Hide tool calls" toggle in the Transcript header hides every Tool call, and Messages holding only Tool calls. The choice is remembered in the browser for every Session; a link or search hit to a hidden Tool call still shows just that one.

## [0.5.1] - 2026-09-30

### Fixed

- Claude Code's `continued-in` line (a Session carrying on in another one) no longer raises an unknown-type Parse warning or shows an unknown Part.
- The oldest opencode Sessions, whose session file has no directory, take their Project from their Messages' working directory instead of going to "No project" with a `missing_field` warning.

## [0.5.0] - 2026-09-30

### Added

- An oh-my-pi Tool call shows its intent on its row ("Reading the spec"), with what it acts on beneath it, like a described Claude Code or opencode call.
- A Claude Code `/loop` wakeup or scheduled task firing shows in the Transcript as a pill ("⏰ Claude resuming /loop wakeup"), with the prompt it resumed with behind a toggle.

## [0.4.0] - 2026-09-29

### Added

- A Tool call with a description (Claude Code, opencode) shows the description on its row, with what it acts on (its command, file, pattern or URL) in monospace beneath it.

### Fixed

- Claude Code's Remote Control status line ("/remote-control is active") no longer raises an unknown-type Parse warning.

## [0.3.0] - 2026-09-28

### Added

- Every push to `main` publishes the Hub image as `ghcr.io/tedkulp/agent-history-hub:dev`, for trying unreleased changes. A dev Hub accepts any Collector version.

### Changed

- A Session with no Source title and no typed prompt before its first slash command is titled by that command (`/implement-next 42`) instead of its id. Housekeeping commands such as `/clear`, `/resume` or `/model` are skipped. Claude Code and oh-my-pi Sessions pick this up when they re-parse.
- The "Re-parsing N Sessions…" banner counts down every 3 s instead of only on reload, and says "Re-parse finished · reload" when it's done.
- The feed updates in place instead of showing the "↑ N Sessions updated" pill: a Session parsed from live data moves to the top (or appears there), and the rows you're looking at stay put.

## [0.2.0] - 2026-09-28

### Added

- opencode support: the Collector ships opencode history from `~/.local/share/opencode` (or `XDG_DATA_HOME`, and `OPENCODE_DB` for the database, written by re-running `agent-history init`): each Session exported from opencode's database, read-only without blocking opencode, plus any leftover pre-2026 JSON files. The Hub shows it as Transcripts with Child Sessions, preferring the database when a Session is in both.
- oh-my-pi support: the Collector ships oh-my-pi history from `~/.omp/agent` (or `PI_CODING_AGENT_DIR`, `OMP_PROFILE` or `XDG_DATA_HOME`, written by re-running `agent-history init`), including archived Sessions, and the Hub shows it as Transcripts following the latest branch, with sub-agents as Child Sessions.
- Codex support: the Collector ships Codex history from `~/.codex` (or `CODEX_HOME`, written by re-running `agent-history init`), and the Hub shows it as Transcripts with Child Sessions.
- A forked Session shows "forked from …" in its Transcript header, linking to the Session it came from.
- A Transcript shows a "↑ Top" link once you scroll past its header; it returns to the top and focuses the heading without adding a browser history entry.
- Background task and sub-agent notifications (`<task-notification>`) show in the Transcript as a compact marker with status, summary and usage, linked to the tool call that started the task, instead of raw text in a user bubble. Existing Sessions pick this up when they re-parse.

### Changed

- The sample `compose.yaml` pins the Hub image to its minor version (`v0.2`) instead of `latest`, so `docker compose pull` takes patch releases but not a new minor version.
- Jumping within a Transcript (outline prompts, Tool call and Child Session links) no longer adds a browser history entry per click, so Back leaves the page. The address bar still shows the anchor.

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

[Unreleased]: https://github.com/tedkulp/agent-history/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/tedkulp/agent-history/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/tedkulp/agent-history/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/tedkulp/agent-history/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/tedkulp/agent-history/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/tedkulp/agent-history/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/tedkulp/agent-history/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/tedkulp/agent-history/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/tedkulp/agent-history/releases/tag/v0.1.0
