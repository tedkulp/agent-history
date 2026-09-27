# Collector spec

## 1. Purpose and scope

The Collector is the `agent-history` program installed on each Machine. It finds every Source's history on disk, names each Raw record by its Record key, and ships the bytes to the Hub over the ingestion protocol. It runs as a per-user service and keeps running in the background.

The Collector **never parses** a Source format ([ADR 0001](../adr/0001-hub-parses-collector-ships-raw.md)). It does only three things with Source data:

- discovers files (or database rows) through each Source's Layouts
- reads the starting cwd cheaply, for `exclude` filtering only (§4.6)
- exports opencode's database rows verbatim as JSONL

This spec covers:

- the CLI, config file, state directory and control socket
- the Source adapter interface on the Collector side
- the run loop: startup reconcile, file watching, rescan, debounce, uploads
- the service definitions for launchd and systemd, and how they survive `mise upgrade`
- the Collector side of the release: archives, the mise install, self-restart, stale-service warnings

The wire protocol is in [`protocol.md`](protocol.md). Each Source's Layouts, Record-key rules and cwd extraction are in `adapters/*.md`. The Hub is in [`hub.md`](hub.md).

Source: [Collector design: discovery, config and lifecycle](https://github.com/tedkulp/agent-history/issues/11), [ADR 0001](../adr/0001-hub-parses-collector-ships-raw.md)

## 2. Interfaces

### 2.1 Binary and install

- The Collector binary is **`agent-history`**. The Hub's entrypoint is a separate binary, `agent-history-hub`, shipped only in the Hub image.
- Both live in one Go module and share the `protocol` package (see `protocol.md` §2.1).
- The Collector is installed with mise's `github:` backend:

  ```sh
  mise use -g github:tedkulp/agent-history
  ```

  mise picks the right archive from the GitHub Release (`agent-history_<os>_<arch>.tar.gz`) with no `asset_pattern`, and verifies the build attestation.
- Supported platforms: `darwin` and `linux`, each on `amd64` and `arm64`. The binary is static (`CGO_ENABLED=0`), so one Linux build serves both glibc and musl. opencode's database is read with `modernc.org/sqlite`, so no CGO is needed.

Source: [Installing the Collector via mise and running it as a user service](https://github.com/tedkulp/agent-history/issues/6), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17), [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 2.2 CLI

| Command | What it does |
|---|---|
| `agent-history init --hub <url> [--name <display>] [--offline] [--reset-roots]` | First-run setup, safe to re-run (§4.1) |
| `agent-history run` | Runs the Collector in the foreground. The service invokes this. |
| `agent-history status` | Prints the health report (§2.5) |
| `agent-history sync` | Forces a full manifest reconcile (§4.3) |
| `agent-history set-name <display>` | Changes the Machine's display name (§4.1) |
| `agent-history service install` | Writes the service definition and loads it. Idempotent: rewrites and reloads (§4.8). |
| `agent-history service uninstall` | Stops the service and removes its definition. Config and state are kept. |
| `agent-history service start` / `stop` / `restart` / `status` | Controls the service through `launchctl` or `systemctl --user` |
| `agent-history version` | Prints the version, e.g. `0.3.1` (`0.0.0-dev` without release ldflags), and nothing else. Self-restart relies on this output (§4.9). |

- `status` and `sync` talk to the running service over the control socket (§2.4), so two processes never upload at the same time.
- If no service is running, `sync` does a one-shot reconcile itself, holding the state-dir lock (§3.2) for its duration. `status` prints what it can without a service (config, detected Sources, whether the Hub is reachable) and says "service not running".
- Exit codes: `0` on success, `1` on any error, with the message on stderr.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

### 2.3 Config file

Path: `$XDG_CONFIG_HOME/agent-history/collector.toml`, defaulting to `~/.config/agent-history/collector.toml`. The same path is used on macOS and Linux.

```toml
# Written by `agent-history init`. Safe to edit; restart the service to apply.

machine_id   = "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33"  # never change this
display_name = "work-laptop"                            # defaults to the hostname
hub_url      = "http://hub.vpn:8080"

rescan_interval = "10m"
log_level       = "info"    # debug | info | warn | error

# Glob patterns matched against a Session's starting cwd. Matching Sessions,
# and their Child Sessions, never leave this Machine.
exclude = [
  "/Users/ted/src/client-secret/**",
]

[sources.claude-code]
enabled = true
root    = "/Users/ted/.claude/projects"

[sources.codex]
enabled = true
root    = "/Users/ted/.codex"

[sources.oh-my-pi]
enabled = true
root    = "/Users/ted/.omp/agent"

[sources.opencode]
enabled = false
root    = "/Users/ted/.local/share/opencode"
```

| Key | Required | Default | Meaning |
|---|---|---|---|
| `machine_id` | yes | generated by `init` | The Machine UUID (v4). Lives here, not in state, so wiping state never creates a new Machine. |
| `display_name` | no | hostname | Shown in the Web UI |
| `hub_url` | yes | none | Base URL of the Hub. The Collector appends `/api/v1/...`. |
| `rescan_interval` | no | `10m` | Go duration. Minimum `1m`. |
| `log_level` | no | `info` | |
| `exclude` | no | `[]` | See §4.6 |
| `sources.<id>.enabled` | no | `true` | `false` means the Source is never read |
| `sources.<id>.root` | no | the adapter's default root | Absolute path. `init` writes the resolved value. |
| `sources.opencode.db` | no | `<root>/opencode.db` | Absolute path to opencode's database. `init` writes it only when `OPENCODE_DB` is set (`adapters/opencode.md` §2.1). |

- `<id>` is the Source identifier from `protocol.md` §3.1: `claude-code`, `codex`, `oh-my-pi`, `opencode`.
- Unknown keys are logged at `warn` and ignored.
- `~` and environment variables are **not** expanded in `root`. `init` always writes absolute paths.
- The Collector reads config only at start. Editing it takes effect on `agent-history service restart`.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11); key names and example filled in while writing this spec

### 2.4 State directory and control socket

Path: `$XDG_STATE_HOME/agent-history/`, defaulting to `~/.local/state/agent-history/`, on both macOS and Linux.

| File | Purpose |
|---|---|
| `cache.json` | The local manifest cache (§3.2) |
| `collector.lock` | `flock` held by the process that owns uploads: the service, or a one-shot `sync` |
| `control.sock` | Unix socket for `status` and `sync` (mode `0600`) |
| `collector.log` (+ `.1`, `.2`) | macOS only: launchd redirects stderr here (§4.10) |

Deleting the state directory is always safe. The next start does a full reconcile, and it uploads nothing the Hub already has.

**Control socket.** HTTP/1.1 over the Unix socket, JSON bodies. It is internal to one binary version and not a public API.

| Request | Response |
|---|---|
| `GET /status` | The status report as JSON (§2.5). The CLI formats it. |
| `POST /sync` | Starts a full reconcile and streams progress lines until it finishes. |
| `PUT /name` | Takes `{"display_name": "..."}` from `set-name`, and sends `PUT /machines/{id}` with it (§4.1). |

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11); file names and socket shape filled in while writing this spec

### 2.5 `status` output

`status` reports:

- **Collector**: version, Machine id, display name, Hub URL, and whether the service is running.
- **Hub**: reachable or not, time of the last successful sync, last error, the "upgrade Collector (Hub requires ≥ X)" line after a `426`, and "Hub: N Sessions with parse warnings, M failed to parse" from `GET /api/v1/machines/{id}/health`.
- **Uploads**: the number of pending uploads.
- **Service**: a warning "service definition outdated, run `agent-history service install`" when the installed definition's template version is older than this binary's (§4.8).
- **Per Source**:
  - whether it's detected, its root, and whether it's enabled
  - **Layouts detected**, e.g. opencode `legacy-json` + `sqlite`
  - Raw record count, and how many are excluded
  - **Known-ignored** paths with their last-modified time, e.g. Codex `thread_history_1.sqlite`: a count plus the 5 most recently modified
  - **Unclaimed paths**: a count plus the first 5 paths
  - the last error for this Source, if any

Example:

```text
agent-history 0.3.1   machine 3f6c2a4e…  "work-laptop"   service: running
Hub      http://hub.vpn:8080  reachable, last sync 12s ago
         Hub: 3 Sessions with parse warnings, 0 failed to parse
Uploads  0 pending

claude-code  detected   /Users/ted/.claude/projects
             layouts: jsonl   records: 1,284 (12 excluded)
codex        detected   /Users/ted/.codex
             layouts: jsonl   records: 311
             ignored: thread_history_1.sqlite (modified 2026-09-25 18:02)
oh-my-pi     not detected   /Users/ted/.omp/agent
opencode     disabled
```

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

### 2.6 Source adapter interface (Collector side)

Each Source has one Go adapter in the Collector. It is a **ranked list of Layouts**. The adapter spec for each Source (`adapters/*.md`) fills in the details. The Collector core never looks inside a Source format itself.

A **Source adapter** provides:

| Member | Meaning |
|---|---|
| `ID` | The Source identifier (`protocol.md` §3.1) |
| `DefaultRoot(env)` | The root derived from the given environment (e.g. `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `XDG_DATA_HOME`). `init` calls it with the interactive shell's environment (§4.1). |
| `Detect(root)` | Whether the Source is present under `root` |
| `Version(root)` | The Source's version if it can be read cheaply, else empty. Sent to the Hub in `PUT /machines/{id}`. |
| `Layouts` | The ranked list below |
| `KnownIgnored` | Path globs, relative to the root, that are recognized but deliberately not read |
| `ScanPaths(root)` | The directories (and root-level file globs) checked for unclaimed paths (§4.7). Codex, oh-my-pi and opencode keep much unrelated state under their roots, so they scan only their history directories. |

Each **Layout** provides:

| Member | Meaning |
|---|---|
| `Name` | e.g. `jsonl`, `legacy-json`, `sqlite`. Reported in `status` and `PUT /machines/{id}`. |
| `Rank` | Higher wins when one Session appears in two Layouts. The Hub applies this (see `hub.md`); the Collector only reports it. |
| `Discover(root)` | Lists the Layout's Raw records: each with its Record key, a way to read its content, and its change signal (size + mtime, or `time_updated` for a database) |
| `Claims(path)` | Whether a path under the root belongs to this Layout. Used to find unclaimed paths. |
| `WatchPaths(root)` | The directories (or files) to watch with fsnotify |
| `StartCwd(record)` | The Session's starting cwd, read cheaply from metadata. Used only for `exclude` (§4.6). |
| `Parent(record)` | For a Child Session's record: the Record key of its parent. For an attachment record: the Record key of the `main` it belongs to. Only when knowable from the path or metadata. Used only for `exclude`. |

Rules every adapter follows (from `protocol.md` §3.3):

- Record keys are stable across compression, moves and archiving. For example, Codex `archived_sessions/` and oh-my-pi `archive/` map back to the original key.
- Record keys are relative to the Source root, and Layout-prefixed when a Source has more than one Layout.
- Compressed files (`.zst`, `.gz`) are read decompressed.
- When a compressed copy and an uncompressed copy with the same Record key both exist, the adapter reads the uncompressed one.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Source format drift](https://github.com/tedkulp/agent-history/issues/16), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); the member list filled in while writing this spec

### 2.7 Default roots

`init` resolves each root from the interactive shell's environment. The adapter spec holds the exact rule; in short:

| Source | Default root | Env vars `init` reads |
|---|---|---|
| Claude Code | `~/.claude/projects` | `CLAUDE_CONFIG_DIR` (root becomes `$CLAUDE_CONFIG_DIR/projects`) |
| Codex | `~/.codex` | `CODEX_HOME` |
| oh-my-pi | `~/.omp/agent` | `PI_CODING_AGENT_DIR`, `OMP_PROFILE`, `XDG_DATA_HOME`, `XDG_STATE_HOME` (these relocate it on macOS too) |
| opencode | `~/.local/share/opencode` | `XDG_DATA_HOME`, `OPENCODE_DB` |

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Claude Code on-disk history format](https://github.com/tedkulp/agent-history/issues/2), [Codex on-disk history format](https://github.com/tedkulp/agent-history/issues/3), [oh-my-pi on-disk history format](https://github.com/tedkulp/agent-history/issues/4), [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5)

## 3. Data

### 3.1 Machine identity

- The Machine is identified by a UUID (v4) generated by the first `init` and stored in `collector.toml` as `machine_id`.
- Re-running `init` never regenerates it. Copying the config file to another Machine copies its identity, so the docs warn against it.
- The display name defaults to the hostname (`os.Hostname()`, without a `.local` suffix) and can be changed at any time.
- The Collector sends `hostname`, `os`, `arch`, `home_dir`, its version and the detected Sources in `PUT /machines/{id}` (`protocol.md` §2.3).

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [What a project is across Machines](https://github.com/tedkulp/agent-history/issues/8)

### 3.2 Local manifest cache

`cache.json` in the state dir. It records the last state the Hub acknowledged for each Raw record, so steady-state work never re-hashes unchanged files.

```json
{
  "version": 1,
  "hub_url": "http://hub.vpn:8080",
  "records": {
    "claude-code\u0000-Users-ted-src-app/5f1c…e2.jsonl": {
      "length": 184223,
      "sha256": "9b0e…",
      "src_size": 184311,
      "src_mtime": "2026-09-26T14:02:11.123Z"
    }
  },
  "opencode_last_time_updated": 1790000000000,
  "unclaimed_seen": ["codex\u0000sessions/2026/09/26/notes.bin"]
}
```

- Keys are `source` + NUL + Record key.
- `length` and `sha256` are what the Hub acknowledged: decompressed bytes, cut at the last complete line for JSONL content (`protocol.md` §3.2).
- `src_size` and `src_mtime` are the on-disk file's stat at the time of that ack. When both are unchanged, the record is skipped without reading it.
- `opencode_last_time_updated` is the highest `session.time_updated` already exported (§4.5).
- `unclaimed_seen` holds unclaimed paths already logged, as `source` + NUL + path relative to the root, so each one is logged only once (§4.7).
- The file is written atomically (write a temp file, then rename), at most once every 5 s and on shutdown.
- If `hub_url` in the cache differs from config, or the file is missing or unreadable, the cache is discarded and rebuilt by the next reconcile.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); the file format filled in while writing this spec

## 4. Behavior

### 4.1 First run: `init`

`agent-history init --hub <url> [--name <display>] [--offline] [--reset-roots]`:

1. **Machine id.** Generate a UUID if the config has none. Never replace an existing one.
2. **Resolve Source roots.** Service managers can't see shell environment variables, so `init` reads them once from the user's interactive shell. It runs `$SHELL -i -c 'env -0'` with a 10 s timeout, and falls back to its own environment if that fails. For each Source, it calls the adapter's `DefaultRoot(env)`. An existing `root` in config is kept unless `--reset-roots` is passed.
3. **Write config** atomically, keeping any keys the user has added.
4. **Register.** `PUT /api/v1/machines/{id}`. If the Hub is unreachable, `init` fails with a clear message and writes nothing further. `--offline` skips this step.
5. **Install and start the service** (§4.8). On Linux, also run `loginctl enable-linger $USER` so the service keeps running after logout. If that fails, print the command for the user to run.
6. Print the `status` report.

`set-name <display>` updates `display_name` in config and, if the service is running, sends `PUT /machines/{id}` through the service. Editing config and restarting has the same effect.

A Source whose root doesn't exist is written to config anyway and reported "not detected". It's re-checked on every rescan, so installing a Source later needs no action.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Installing the Collector via mise and running it as a user service](https://github.com/tedkulp/agent-history/issues/6); the shell-env command filled in while writing this spec

### 4.2 Run loop

`agent-history run`:

1. Take the `collector.lock` flock. If another process holds it, exit `1` with "already running".
2. Load config and cache. Open the control socket.
3. **Startup reconcile** (§4.3).
4. Start an fsnotify watch on every enabled, detected Source's `WatchPaths`.
5. Every `rescan_interval` (10 min), run a **rescan**:
   - re-detect every Source (so a newly installed Source starts being read)
   - discover all records and compare each against the cache by `src_size` + `src_mtime`, shipping any that changed (§4.4)
   - list unclaimed paths (§4.7)
   - run the self-restart version check (§4.9)
6. On `SIGTERM` / `SIGINT`: stop taking new work, wait up to 10 s for in-flight uploads, write the cache, exit `0`.

There is **no separate backfill mode**. On a new Machine, the startup reconcile finds that the Hub has nothing and ships everything as appends from offset 0.

**Watch failures.** If a watch can't be added (for example, the Linux inotify limit is reached), log one `warn` naming the root and the limit, and rely on the rescan for that root. The Collector keeps running.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

### 4.3 Reconcile

The full reconcile follows `protocol.md` §4.1: `PUT /machines/{id}`, `GET /manifest`, then compare every discovered, non-excluded Raw record against the Hub's entry and `append`, `replace` or skip. Afterwards the cache is rewritten from the results.

It runs:

- on start
- when the Hub becomes reachable again after an outage (`protocol.md` §4.5)
- on `agent-history sync`
- after a successful hourly retry following a `426` (`protocol.md` §4.6)

During a reconcile the Collector hashes every record that the Hub lists, since the cache may be stale. Records the Hub doesn't list start from offset 0 without hashing first.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 4.4 Shipping a change

1. A watch event or a rescan marks a record dirty.
2. **Debounce**: the record is shipped 2 s after its last event. A record that keeps changing is still shipped at least every 30 s, so a long-running Session reaches the Hub while it's active.
3. Read the content, decompressing if needed. For JSONL content (`protocol.md` §3.2), cut it at the last complete `\n`; any other file is read whole.
4. Decide against the cache entry with the `protocol.md` §4.1 table: skip, `append` from the cached length, or `replace`. Checking the prefix means hashing `L[0:cached length]`.
5. Upload in chunks of at most 8 MiB, each ending on a line boundary for JSONL content (`protocol.md` §4.3).
6. After each `200`, check the returned sha256 against the local hash, then update the cache entry.

- At most **4 uploads** in flight across all records. One record never has two requests in flight.
- Responses and retries follow `protocol.md` §4.7. A `409` is re-decided against the state it returns.
- Records that disappear from disk are not reported. Their cache entries are dropped on the next rescan.

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10); the 30 s cap filled in while writing this spec

### 4.5 opencode's database

opencode's `sqlite` Layout is not a file per Session, so it's handled differently:

- The Collector watches `opencode.db-wal` (and `opencode.db`). A change starts the debounce for the whole database.
- It opens the database **read-only** (`mode=ro`, never creating or checkpointing the WAL) and queries Sessions changed since `opencode_last_time_updated`: the Session's own `time_updated`, or that of any of its rows, is newer (`adapters/opencode.md` §2.2).
- For each such Session it exports that Session's `session`, `message`, `part` and `session_message` rows as table-tagged JSONL. The exact line shape is in `adapters/opencode.md`. Each export is sent as a `replace` (`protocol.md` §4.2), which the Hub treats as a no-op when nothing changed.
- `opencode_last_time_updated` moves forward only after every exported Session is acked.
- If the database is locked or busy, retry on the next event or rescan.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11), [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [opencode on-disk history format](https://github.com/tedkulp/agent-history/issues/5)

### 4.6 Exclusions

- `exclude` holds glob patterns (`**` allowed, matched against the whole path) compared with a Session's **starting cwd**.
- For each record, the Collector asks the Layout for `StartCwd`, which reads only what it needs: Claude Code's first-line `cwd`, Codex's `session_meta`, the oh-my-pi header, opencode's `session.directory`. This reads metadata for filtering only. It isn't Transcript parsing, so ADR 0001 holds.
- A matching Session is never read further and never shipped. **Its Child Sessions follow the parent**: a record whose `Parent` is excluded is excluded too, whatever its own cwd.
- If the cwd can't be read yet (the file is still empty), the record waits until it can.
- Exclusion decisions are cached in memory by Record key. Changing `exclude` takes effect on restart. Records already shipped stay on the Hub; the Hub never deletes.
- `status` shows the excluded count per Source.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 4.7 Layout drift: unclaimed and known-ignored paths

On every rescan, for each detected Source:

- Every regular file under the adapter's `ScanPaths` is checked against each Layout's `Claims` and the adapter's `KnownIgnored` globs.
- A file that matches neither is **unclaimed**. The first time each unclaimed path is seen, it's logged at `warn`, and `status` lists the count and the first 5 paths. Unclaimed files are not shipped.
- **Known-ignored** files are listed in `status` with their modification time. For Codex, this is how the operator notices if Codex stops writing JSONL: the ignored `thread_history_*.sqlite` gets newer than the newest claimed record.

Source: [Source format drift](https://github.com/tedkulp/agent-history/issues/16); `ScanPaths` filled in by [Write the adapter specs](https://github.com/tedkulp/agent-history/issues/23)

### 4.8 Service definitions

The Collector writes its own service definition with a small built-in template. It does **not** use `kardianos/service`, whose default path resolution uses `os.Executable()`. That call resolves the mise shim through its symlink to the `mise` binary itself, which would break the service.

**The service always runs the mise shim**, never a versioned install path:

- Shim path: `$MISE_DATA_DIR/shims/agent-history`, defaulting to `~/.local/share/mise/shims/agent-history`. The `--shim <path>` flag on `service install` overrides it.
- If the shim doesn't exist, `service install` fails with a message explaining how to install through mise, unless `--shim` is given.
- The shim stays at the same path across `mise upgrade`, so the definition never needs rewriting for an upgrade.

Each definition carries a **template version** (an integer in the binary, starting at `1`). It's written as a comment on macOS and as an `X-` key on Linux.

**macOS**: `~/Library/LaunchAgents/com.tedkulp.agent-history.plist`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- agent-history service template 1 -->
<plist version="1.0">
<dict>
  <key>Label</key>              <string>com.tedkulp.agent-history</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/ted/.local/share/mise/shims/agent-history</string>
    <string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>             <string>/Users/ted</string>
  </dict>
  <key>RunAtLoad</key>          <true/>
  <key>KeepAlive</key>          <true/>
  <key>ThrottleInterval</key>   <integer>10</integer>
  <key>StandardOutPath</key>    <string>/Users/ted/.local/state/agent-history/collector.log</string>
  <key>StandardErrorPath</key>  <string>/Users/ted/.local/state/agent-history/collector.log</string>
</dict>
</plist>
```

Loaded with `launchctl bootstrap gui/$UID <plist>`. Stopped with `launchctl bootout`. Restarted with `launchctl kickstart -k gui/$UID/com.tedkulp.agent-history`.

**Linux**: `$XDG_CONFIG_HOME/systemd/user/agent-history.service`, defaulting to `~/.config/systemd/user/agent-history.service`

```ini
[Unit]
Description=Agent History Collector
X-AgentHistoryTemplate=1

[Service]
ExecStart=%h/.local/share/mise/shims/agent-history run
Environment=HOME=%h
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

`ExecStart` is written as the resolved absolute shim path (`%h` is shown here for the default). Installed with `systemctl --user daemon-reload`, `systemctl --user enable agent-history.service`, then `systemctl --user restart agent-history.service`, so a reinstall also restarts a running service on the new definition.

- **Environment.** Besides `HOME`, the definition carries `MISE_DATA_DIR`, `XDG_CONFIG_HOME` and `XDG_STATE_HOME` when they are set where `init` or `service install` runs, so the service finds the same shim, config and state directories.

- **`Restart=always` / `KeepAlive=true`** matter: the self-restart (§4.9) exits `0`, and the service manager must start it again.
- **`service install` is idempotent**: it rewrites the file and reloads the service.
- **Stale definition.** On start and in `status`, the Collector reads the installed definition's template version. If it's older than its own, `status` warns "service definition outdated, run `agent-history service install`". The Collector never rewrites the definition on its own.

Source: [Installing the Collector via mise and running it as a user service](https://github.com/tedkulp/agent-history/issues/6), [Release pipeline](https://github.com/tedkulp/agent-history/issues/17); label, unit name, `ThrottleInterval`, `RestartSec` and the template-version markers filled in while writing this spec

### 4.9 Upgrades and self-restart

1. The user runs `mise upgrade`. mise installs the new version next to the old one. The shim now resolves to the new binary. The running process is still the old one.
2. On each rescan, the Collector runs `<shim> version` with a 10 s timeout.
3. If the output differs from its own version, it logs `info` "new version X found, restarting", stops taking new work, drains in-flight uploads (up to 30 s), writes the cache, and **exits `0`**.
4. launchd `KeepAlive` or systemd `Restart=always` starts it again, now running the new binary.

- If running the shim fails, log a `warn` and carry on. It is checked again on the next rescan.
- `agent-history service restart` restarts immediately.
- The Collector never downloads or installs anything itself. mise does that.

Source: [Release pipeline](https://github.com/tedkulp/agent-history/issues/17), [Installing the Collector via mise and running it as a user service](https://github.com/tedkulp/agent-history/issues/6)

### 4.10 Logging

- Structured text logs (`log/slog`) go to stderr. The level comes from `log_level` in config (`info` by default).
- Linux: systemd sends stderr to journald (`journalctl --user -u agent-history`).
- macOS: the plist sends stderr to `~/.local/state/agent-history/collector.log`. launchd doesn't rotate, so the Collector does: on start and every rescan, if the file is over 10 MB, it rotates to `.1` and `.2` (3 files in all), then reopens its stderr.
- `status` shows the last error per Source and for the Hub connection.

Source: [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 4.11 Error cases

| Situation | Behavior |
|---|---|
| Hub unreachable or `5xx` | Back off 1 s doubling to 5 min with jitter, then reconcile (`protocol.md` §4.5). No spool: the files on disk are the queue. |
| `426` from the Hub | Stop uploading, retry `PUT /machines/{id}` hourly, `status` says "upgrade Collector" (`protocol.md` §4.6) |
| `400` / `413` / `415` | Log `error`, skip that record until it next changes (`protocol.md` §4.7) |
| Config missing or invalid | `run` exits `1` with the reason. The service manager restarts it, throttled; `status` shows the error. |
| Source root disappears | Source becomes "not detected". Its cache entries stay. Nothing is reported to the Hub as deleted. |
| File unreadable (permissions) | Log `warn` once per path, skip it, retry on the next rescan |
| A single line over 64 MiB | Stop shipping that record at that line and log `warn` (`protocol.md` §4.3) |
| inotify limit reached | Log `warn`, rely on the rescan for that root (§4.2) |

Source: [Collector → Hub ingestion protocol](https://github.com/tedkulp/agent-history/issues/10), [Collector design](https://github.com/tedkulp/agent-history/issues/11)

### 4.12 Release (Collector side)

- One lockstep `vX.Y.Z` tag, pushed by a human, releases both the Collector and the Hub. Pre-release tags (`vX.Y.Z-rc.N`) are marked prerelease on GitHub.
- GoReleaser builds `agent-history_<os>_<arch>.tar.gz` for the four platforms, plus `checksums.txt`, and injects the version through ldflags. The GitHub Release holds only Collector archives, so mise's `github:` backend finds exactly one match per platform.
- Archives get GitHub artifact attestations (`actions/attest-build-provenance`). No cosign, no SBOM in v1.
- CI on PRs and `main`: `go vet`, `go test ./...`, `goreleaser check`, `goreleaser release --snapshot`.
- The Hub image and the minimum Collector version are covered in [`hub.md`](hub.md) and [`protocol.md`](protocol.md) §4.6. Release-wide rules are in [`README.md`](README.md).

Source: [Release pipeline](https://github.com/tedkulp/agent-history/issues/17)

## 5. Out of scope

- Parsing any Source format beyond the cheap starting-cwd read for `exclude`.
- A spool or offline queue. The files on disk are the queue.
- Reporting local deletions to the Hub.
- Self-upgrade. mise upgrades the binary.
- Automatically rewriting a stale service definition.
- Reading Codex's `thread_history_*.sqlite` store. It's known-ignored in v1.
- Windows, and install methods other than mise (Homebrew, distro packages).
- Authentication and TLS (VPN only).
- System-wide (root) services, or collecting from other users' home directories.

## 6. M1 acceptance checklist

M1 is Claude Code end to end. Only the `claude-code` adapter needs to exist; the others may be absent.

- [ ] `mise use -g github:tedkulp/agent-history` installs the binary on macOS arm64 and Linux amd64 from a real GitHub Release.
- [ ] `agent-history version` prints the ldflags-injected version.
- [ ] `agent-history init --hub <url>` generates a Machine UUID, writes `collector.toml` with the resolved Claude Code root, registers with the Hub, installs and starts the service.
- [ ] Re-running `init` keeps the same `machine_id` and any user-added keys.
- [ ] `init` against an unreachable Hub fails clearly; `--offline` succeeds.
- [ ] `CLAUDE_CONFIG_DIR` set in the shell rc (not in the service's environment) is picked up by `init` and used by the service.
- [ ] The installed plist / unit points at the mise shim path, not a versioned path and not the `mise` binary.
- [ ] On Linux, the service keeps running after logout (linger enabled).
- [ ] On a fresh Machine with existing Claude Code history, the startup reconcile ships every Session and Child Session file; the Hub manifest matches disk.
- [ ] A new message in an active Claude Code Session reaches the Hub within about 5 s of being written.
- [ ] Rescan picks up a change when the watch is disabled.
- [ ] A Session whose starting cwd matches `exclude` is never shipped, and neither are its Child Session files.
- [ ] `agent-history status` shows the service, Hub reachability, last sync, pending uploads, and the Claude Code root, Layout and record count.
- [ ] A file under the Claude Code root that no Layout claims shows up as unclaimed in `status` and is logged once.
- [ ] `agent-history sync` with the service running goes through the socket; with the service stopped, it reconciles itself. Two `sync`s never upload at once.
- [ ] Deleting the state directory and restarting uploads nothing the Hub already has.
- [ ] After `mise upgrade` to a newer release, the running Collector restarts itself within one rescan and reports the new version to the Hub.
- [ ] Bumping the template version makes `status` warn about an outdated service definition; `service install` clears it.
- [ ] `SIGTERM` during an upload drains it and writes the cache before exit.
- [ ] macOS: `collector.log` rotates at 10 MB, keeping 3 files.
