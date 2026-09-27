# Agent History v1 spec

## 1. Purpose and scope

Agent History gathers coding-agent conversation history from many Machines into one place to browse and search. A **Collector** on each Machine finds every Source's history on disk and ships it, raw, to one **Hub**. The Hub keeps every Raw record verbatim, parses it into Transcripts, and serves a web UI with full-text search.

This directory is the build-ready v1 spec. It has seven documents plus this index. Each document is self-contained: it states its decisions in full, and every section ends with a `Source:` line linking the ticket that holds the reasoning. From handoff on, **the spec is canonical for build detail**; the tickets keep the why.

This index covers:

- what each document covers, and the order to read them in
- the build order: M1, then the remaining Sources
- the release rules that span both components
- the v1 scope boundary, and the M1 acceptance gate

Source: [Spec assembly: fold decisions into the handoff spec](https://github.com/tedkulp/agent-history/issues/18)

## 2. The documents

### 2.1 Reading order

| # | Document | Covers |
|---|---|---|
| 0 | [`CONTEXT.md`](../../CONTEXT.md) | The vocabulary: Hub, Collector, Machine, Source, Session, Project, Child Session, Transcript, Message, Part, Tool call, Raw record, Record key, Layout, Parse warning. Every spec uses these terms. |
| 1 | [`protocol.md`](protocol.md) | The Collector → Hub ingestion protocol: the Raw record and its Record key, the four `/api/v1` endpoints, append / replace, manifest reconcile, versioning and the minimum Collector version, response codes |
| 2 | [`collector.md`](collector.md) | The `agent-history` binary: CLI, config, state dir and control socket, the Collector-side adapter interface, run loop, exclusions, Layout drift, launchd / systemd service, self-restart, Collector release |
| 3 | [`hub.md`](hub.md) | The `agent-history-hub` binary: CLI, env config, HTTP routes, the Hub-side parser interface, SQLite schema, parse queue and Re-parse flow, Project assignment, Parse warnings, search, Web UI, backups, migrations, image and Hub release |
| 4 | [`adapters/claude-code.md`](adapters/claude-code.md) | Claude Code: the M1 Source |
| 5 | [`adapters/codex.md`](adapters/codex.md) | Codex |
| 6 | [`adapters/oh-my-pi.md`](adapters/oh-my-pi.md) | oh-my-pi |
| 7 | [`adapters/opencode.md`](adapters/opencode.md) | opencode, with its two Layouts |

Read the protocol first: it is the seam the other two documents meet at. Each adapter spec fills in both halves of one Source: the Collector adapter (roots, Layouts, Record keys, `StartCwd` / `Parent`) and the Hub parser (`MapKey`, mapping into the Normalized Transcript model).

### 2.2 Shared template

Every document follows the same sections:

1. Purpose and scope
2. Interfaces (CLI / HTTP / files / config). Adapter specs have **Layouts** here instead.
3. Data (schema or record shapes). Adapter specs have **Mapping** here instead: Source record → Messages, Parts, Child Sessions, known-ignored types.
4. Behavior (loops, queues, error cases)
5. Out of scope
6. Acceptance checklist: **M1** for the protocol, Collector, Hub and Claude Code; **after M1** for the other three adapters

### 2.3 Evidence and other references

- **Architecture decision**: [ADR 0001: the Hub parses, the Collector ships raw](../adr/0001-hub-parses-collector-ships-raw.md).
- **Research** behind the adapter specs and the service design, in [`docs/research/`](../research/):
  - [`claude-code-format.md`](../research/claude-code-format.md)
  - [`codex-format.md`](../research/codex-format.md)
  - [`oh-my-pi-format.md`](../research/oh-my-pi-format.md)
  - [`opencode-format.md`](../research/opencode-format.md)
  - [`mise-install-and-service.md`](../research/mise-install-and-service.md)
- **Web UI prototype**: [`docs/prototype/webui/`](../prototype/webui/), variant C (`hub.md` §4.7). It is its own Go module, so the main module's `go test ./...` skips it.
- **Decision history**: the map issue, [Agent History v1: find the way to a build-ready spec](https://github.com/tedkulp/agent-history/issues/1), lists every decision ticket.

Source: [Spec assembly](https://github.com/tedkulp/agent-history/issues/18), [Merge research docs into main](https://github.com/tedkulp/agent-history/issues/19)

## 3. Architecture at a glance

```text
 Machine (macOS / Linux)                               Hub (Docker)
┌────────────────────────────────────┐          ┌──────────────────────────────────┐
│ Source files on disk               │          │ agent-history-hub serve          │
│  ~/.claude/projects, ~/.codex, …   │          │                                  │
│            │ fsnotify + 10 min     │  HTTP    │  /api/v1 ingest ──► raw_records  │
│            ▼ rescan                │ /api/v1  │        (zstd chunks, versions)   │
│ agent-history run (user service)   ├─────────►│            │ parse_queue         │
│  adapters: Layouts → Record keys   │  plain,  │            ▼                     │
│  exclude by starting cwd           │  on VPN  │  parsers → sessions / messages / │
│  append / replace, no spool        │          │            parts / FTS5          │
└────────────────────────────────────┘          │            ▼                     │
                                                │  Web UI: feed, search, Transcript│
                                                │  one SQLite file + backups       │
                                                └──────────────────────────────────┘
```

- **One Go module**, two binaries: `cmd/collector` (`agent-history`) and `cmd/hub` (`agent-history-hub`), sharing the `protocol` package (`protocol.md` §2.1).
- **The Collector never parses** a Source format beyond the cheap starting-cwd read for `exclude`. The Hub parses everything, and can re-parse from the Raw records it keeps ([ADR 0001](../adr/0001-hub-parses-collector-ships-raw.md)).
- **Nothing is ever deleted** on the Hub: not local deletions, not superseded versions, not blobs.

## 4. Build order

### 4.1 M1: Claude Code end to end

M1 builds every seam once, on the Source used most:

1. The `protocol` package: wire types, header names, Source ids, `MinCollectorVersion` (`protocol.md`).
2. The Collector with only the `claude-code` adapter: `init`, the service, reconcile, watch + rescan, `status`, `sync`, `exclude` (`collector.md`, `adapters/claude-code.md` §2).
3. The Hub with only the `claude-code` parser: ingest, storage, parse queue, Re-parse flow, Project assignment, Parse warnings, search, the Web UI, backups, migrations (`hub.md`, `adapters/claude-code.md` §3).
4. The release pipeline: a real `vX.Y.Z` tag publishes the Collector archives and the Hub image (§5).

M1 is done when every M1 checklist passes (§7).

### 4.2 Then the other Sources, one at a time

| Order | Source | Why here |
|---|---|---|
| 2 | Codex | One Layout of plain JSONL. Adds compression, archiving, continuation files and a known-ignored store. |
| 3 | oh-my-pi | One Layout. Adds XDG root search, a branching tree, a title slot rewritten in place (a `replace`), and nested sub-agents. |
| 4 | opencode | Last: two Layouts with ranks and `shadow` records, plus the Collector's read-only SQLite export. |

Each Source is its own milestone: its Collector adapter and Hub parser land together, and its adapter spec's acceptance checklist is the gate. No change to the protocol, the Collector core or the Hub core is expected; if one turns out to be needed, it's a spec change first.

Source: [Spec assembly](https://github.com/tedkulp/agent-history/issues/18)

## 5. Release rules

These span both components. The Collector-side and Hub-side details are in `collector.md` §4.12 and `hub.md` §4.11.

### 5.1 One lockstep tag

- **One `vX.Y.Z` tag releases both** the Collector and the Hub, at the same version. A human pushes it. The tag workflow reruns the tests, checks the version floor against the tag (§5.4), then runs `goreleaser release` (GoReleaser OSS, on GitHub Actions).
- Both binaries get the version through ldflags. `agent-history version` and `agent-history-hub version` print it without the `v`, e.g. `0.3.1`. A build without the release ldflags prints `0.0.0-dev` (`protocol.md` §4.6).
- **Pre-release tags** `vX.Y.Z-rc.N` publish too. The GitHub Release is marked prerelease, and the image gets only the `vX.Y.Z-rc.N` tag. They never move `vX.Y` or `latest`.
- Changelog: GoReleaser's commit list, grouped by `feat:` / `fix:` prefix when present.

### 5.2 What a release publishes

| Artifact | Where | Detail |
|---|---|---|
| Collector archives `agent-history_<os>_<arch>.tar.gz` for `darwin` / `linux` × `amd64` / `arm64`, static (`CGO_ENABLED=0`) | GitHub Release | Plus `checksums.txt`. **Collector archives only**: no Hub binaries, so mise's `github:` backend finds exactly one asset per platform. |
| Hub image `ghcr.io/tedkulp/agent-history-hub`, `linux/amd64` + `linux/arm64` | GHCR | Tags `vX.Y.Z`, `vX.Y`, `latest`. No `vX`, no `edge` or `main` builds. |
| Build provenance | GitHub artifact attestations | For the archives and the image. No cosign, no SBOM in v1. |

### 5.3 CI gates

On every PR and on `main`: `go vet`, `go test ./...`, `goreleaser check`, `goreleaser release --snapshot` (builds everything, including the image, and publishes nothing).

### 5.4 The minimum Collector version

- `protocol.MinCollectorVersion` is a compiled-in constant, shared by both binaries.
- It is bumped **by hand, in the same PR as an incompatible `/api/v1` change**, and at no other time. It is never an upgrade nudge.
- The tag workflow checks that the floor is at most the tag's version before it publishes anything.
- Every comparison with the floor drops pre-release suffixes first, so `0.4.0-rc.1` counts as `0.4.0`. An rc cycle that raises the floor works without hand-editing.
- `0.0.0-dev` builds (no ldflags) skip the floor on a dev Hub and always get `426` from a release Hub.
- Operators can raise the floor, never lower it, with `AGENT_HISTORY_MIN_COLLECTOR_VERSION` on the Hub.
- A Collector below the floor gets `426` on every request, stops uploading, and retries hourly (`protocol.md` §4.6).
- When a release moves the floor, its release notes say **"⚠ requires Collector ≥ X"**, added by hand.
- Changes within `/api/v1` are additive only, and both sides ignore unknown fields. A breaking change gets `/api/v2`.

### 5.5 Upgrading

- **Hub**: pull the new image and restart. The Hub backs up before any migration, then re-parses stale Sessions in the background (`hub.md` §4.1, §4.5).
- **Collector**: on each Machine, replace the binary at the service's path with the new release (`install -m 755`, never `cp` over it), or `mise upgrade` under mise. The running Collector notices the new version within one rescan and restarts itself; `agent-history service restart` restarts at once (`collector.md` §4.9).
- **Order**: upgrade the Hub first. With additive `/api/v1` changes either order works. When the floor moves, old Collectors get `426` until their Machine upgrades, and lose nothing: the files on disk are the queue, and the reconcile after upgrading ships whatever they held back.
- **Rollback** of the Hub: restore the `pre-migrate-*.db` backup and run the old image (`hub.md` §4.8).

Source: [Release pipeline for the Collector binary and Hub image](https://github.com/tedkulp/agent-history/issues/17), [Hub deployment shape](https://github.com/tedkulp/agent-history/issues/15), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); the upgrade order follows from these rules and was filled in while writing this index

## 6. Out of scope for v1

Each document lists its own exclusions. Across the whole system:

- Authentication, authorization and TLS. It runs on a VPN; a reverse proxy is the operator's choice.
- Analytics: token, cost and activity dashboards. Usage is stored per Message but not charted.
- Secret redaction in Transcripts. Raw retention keeps it possible later.
- Mirroring local deletions to the Hub.
- Windows Collectors, Homebrew and distro packages.
- Reading Codex's `thread_history_*.sqlite` store. It is known-ignored while Codex still writes JSONL.

Source: [Agent History v1: find the way to a build-ready spec](https://github.com/tedkulp/agent-history/issues/1)

## 7. M1 acceptance

M1 passes when every item in these four checklists is ticked:

| Checklist | Proves |
|---|---|
| [`protocol.md` §6](protocol.md#6-m1-acceptance-checklist) | Registration, manifest, append / replace / `409`, chunking, durability of the ack, the version floor |
| [`collector.md` §6](collector.md#6-m1-acceptance-checklist) | Manual and mise install, `init`, the service surviving logout and upgrades, reconcile, live shipping, `exclude`, `status` / `sync` |
| [`hub.md` §6](hub.md#6-m1-acceptance-checklist) | The image, migrations, storage, the parse queue and Re-parse flow, Projects, feed, search, Transcript page, backups |
| [`adapters/claude-code.md` §6](adapters/claude-code.md#6-m1-acceptance-checklist) | Claude Code's keys and mapping: branches, compaction, spilled output, sub-agents, titles |

As one end-to-end run, M1 looks like this:

- [ ] Push a `vX.Y.Z` tag. The GitHub Release has four Collector archives, and GHCR has the Hub image for amd64 and arm64.
- [ ] `docker compose up` on the Hub host. The container reports healthy.
- [ ] On a Mac and a Linux Machine with existing Claude Code history: download the release archive into `~/.local/bin` (or `mise use -g github:tedkulp/agent-history`), then `agent-history init --hub <url>`.
- [ ] Within minutes, the Hub's feed shows both Machines' Sessions, grouped by day, with Machine and Project chips.
- [ ] A new prompt in a live Claude Code Session appears on its Transcript page, after a reload, within about 15 s.
- [ ] Searching a word from that prompt finds it, opens the Transcript at that Message, and highlights it.
- [ ] A Session with a sub-agent shows its Tool call cluster with a working "↳ Child Session" link, and the child links back.
- [ ] Tag `vX.Y.(Z+1)`. Replace the binary on a Machine (or `mise upgrade`); its Collector restarts on its own and reports the new version to the Hub.

The after-M1 checklists for [Codex](adapters/codex.md#6-acceptance-checklist-after-m1), [oh-my-pi](adapters/oh-my-pi.md#6-acceptance-checklist-after-m1) and [opencode](adapters/opencode.md#6-acceptance-checklist-after-m1) gate their own milestones (§4.2).

Source: [Spec assembly](https://github.com/tedkulp/agent-history/issues/18)
