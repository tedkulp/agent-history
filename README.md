# Agent History

Gathers your coding-agent conversation history from every machine you use
into one place to browse and search.

A **Collector** runs on each machine. It finds each coding tool's history on
disk and ships it, raw, to one **Hub**. The Hub keeps every record verbatim,
parses it into readable transcripts, and serves a web UI with full-text
search. It never deletes anything.

Supported today: **Claude Code**. Codex, oh-my-pi and opencode are specced
and planned.

> [!WARNING]
> The Hub has no authentication and no TLS. Run it on a private network
> (a VPN such as Tailscale or WireGuard), or put your own reverse proxy in
> front of it.

## Run the Hub

The Hub is a Docker image, `ghcr.io/tedkulp/agent-history-hub`. With the
[`compose.yaml`](compose.yaml) from this repo:

```sh
mkdir -p data backups
docker compose up -d
```

Open `http://<host>:8080/`. Storage, file ownership, configuration and
backups are covered in [docs/hub-deploy.md](docs/hub-deploy.md).

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
the binary you ran. Pass `--exec <path>` to choose another path.

## Develop

You need Go (see `go.mod`) and [just](https://just.systems).

```sh
just check    # gofmt, go vet, go test
just hub      # run a Hub on :8080 with its data in .data/
just build    # build both binaries into bin/
```

`just` lists every recipe. The build-ready design lives in
[docs/spec/](docs/spec/README.md), with the vocabulary in
[CONTEXT.md](CONTEXT.md). Releases are cut by pushing a tag; see
[docs/releasing.md](docs/releasing.md).

## License

[MIT](LICENSE)
