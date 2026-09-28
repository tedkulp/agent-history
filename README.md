# Agent History

Gathers your coding-agent conversation history from every machine you use
into one place to browse and search.

A **Collector** runs on each machine. It finds each coding tool's history on
disk and ships it, raw, to one **Hub**. The Hub keeps every record verbatim,
parses it into readable transcripts, and serves a web UI with full-text
search. It never deletes anything.

Supported today: **Claude Code**, **Codex** and **oh-my-pi**. opencode is
specced and planned.

> [!WARNING]
> The Hub has no authentication and no TLS. Run it on a private network
> (a VPN such as Tailscale or WireGuard), or put your own reverse proxy in
> front of it.

## Run the Hub

The Hub is a Docker image, `ghcr.io/tedkulp/agent-history-hub`, for
`linux/amd64` and `linux/arm64`. Run it with Docker Compose or plain
Docker. Both keep the database in `./data` and daily backups in
`./backups`.

### Before you run it

- **Local disk only.** `./data` must not be on NFS, SMB or any other
  network filesystem; SQLite corrupts or deadlocks there.
  [More](docs/hub-deploy.md#storage-local-disk-only)
- **uid 1000.** The Hub runs as uid/gid 1000, so `data` and `backups` must
  be writable by it. On Linux, if `id -u` isn't 1000, then after the
  `mkdir` below either run `sudo chown -R 1000:1000 data backups`, or run
  the Hub as their owner with `user: "<uid>:<gid>"` in `compose.yaml` or
  `--user "$(id -u):$(id -g)"` on `docker run`. Otherwise the Hub exits at
  start-up with a permission error in the logs. Docker Desktop on macOS
  needs neither.
  [More](docs/hub-deploy.md#file-ownership)
- **Settings are env vars.** The ones you're most likely to set: `TZ`
  (Web UI and backup time zone, default UTC), `AGENT_HISTORY_BACKUP_AT`
  (daily backup time, default `03:00`) and `AGENT_HISTORY_BACKUP_KEEP`
  (daily backups kept, default `7`). Pass them under `environment:` or
  with `-e` on `docker run`.
  [All settings](docs/hub-deploy.md#configuration)

### Docker Compose

Save this as `compose.yaml` (it is the [`compose.yaml`](compose.yaml) in
this repo):

```yaml
services:
  hub:
    image: ghcr.io/tedkulp/agent-history-hub:v0.1
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

Then, in the same directory:

```sh
mkdir -p data backups
docker compose up -d
docker compose ps        # STATUS shows "healthy" once the Hub is ready
```

The healthcheck is built into the image, so `compose.yaml` doesn't need
one.

### Plain Docker

```sh
mkdir -p data backups
docker run -d --name agent-history-hub -p 8080:8080 -v "$PWD/data:/data" -v "$PWD/backups:/backups" --restart unless-stopped ghcr.io/tedkulp/agent-history-hub:v0.1
docker ps --filter name=agent-history-hub   # STATUS shows "(healthy)" once the Hub is ready
```

Either way, open `http://<host>:8080/`. That address is the `--hub` you
give each Collector.

### Image tags

| Tag | Points at |
|---|---|
| `vX.Y.Z`, e.g. `v0.1.0` | That exact release |
| `vX.Y`, e.g. `v0.1` | The newest `vX.Y.*` patch release |
| `latest` | The newest release |

Pin `vX.Y`: you get bug fixes with each pull, and move to a new minor
version only when you change the tag.

### Upgrade

Upgrade the Hub before the Collectors. With Compose:

```sh
docker compose pull && docker compose up -d
```

With plain Docker, pull, remove the container, and run the same
`docker run` command again; the data stays in `./data`:

```sh
docker pull ghcr.io/tedkulp/agent-history-hub:v0.1
docker rm -f agent-history-hub
docker run -d --name agent-history-hub -p 8080:8080 -v "$PWD/data:/data" -v "$PWD/backups:/backups" --restart unless-stopped ghcr.io/tedkulp/agent-history-hub:v0.1
```

To move to a new minor version, change the tag first. An upgrade that
changes the database schema backs it up to `backups/pre-migrate-<n>.db`
first; rolling back, backups and TLS are in
[docs/hub-deploy.md](docs/hub-deploy.md).

## Install the Collector

On each macOS or Linux machine, download the latest release into a
directory on your `PATH`, then set it up:

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); case $arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac
curl -fsSL "https://github.com/tedkulp/agent-history/releases/latest/download/agent-history_${os}_${arch}.tar.gz" | tar -xz agent-history
mkdir -p ~/.local/bin && install -m 755 agent-history ~/.local/bin/
agent-history init --hub http://<host>:8080
```

`init` writes `~/.config/agent-history/collector.toml`, registers the
machine with the Hub, and installs a launchd or systemd user service that
runs `~/.local/bin/agent-history` and keeps shipping new history in the
background. Check on it with:

```sh
agent-history status
```

To upgrade, download the new release the same way and replace the file
with `install -m 755` (or `mv`). Never `cp` over it: Linux refuses to
overwrite a running binary ("text file busy"), and on macOS an in-place
overwrite can break it. The running service restarts itself onto the new
version within one rescan.

On macOS, use `curl` as above rather than a browser: a browser download is
quarantined, and Gatekeeper refuses to run the unsigned binary. If you did
use a browser, clear it with `xattr -d com.apple.quarantine agent-history`.

### With mise

[mise](https://mise.jdx.dev) works too:

```sh
mise use -g github:tedkulp/agent-history
agent-history init --hub http://<host>:8080
```

The service then runs mise's shim, and `mise upgrade` upgrades it; the
running service restarts itself onto the new version.

`init` and `service install` pick the mise shim when it exists, otherwise
the binary you ran, so after moving from mise to a manual install, remove
the shim (`mise uninstall github:tedkulp/agent-history`) before running
`init` again. Pass `--exec <path>` to choose another path.

## Develop

You need Go (see `go.mod`) and [just](https://just.systems).

```sh
just check    # gofmt, go vet, go test
just hub      # run a Hub on :8080 with its data in .data/
just collector # ship this Machine's history to that Hub, beside any installed Collector
just build    # build both binaries into bin/
```

`just` lists every recipe. The build-ready design lives in
[docs/spec/](docs/spec/README.md), with the vocabulary in
[CONTEXT.md](CONTEXT.md). Releases are cut by pushing a tag; see
[docs/releasing.md](docs/releasing.md).

## License

[MIT](LICENSE)
