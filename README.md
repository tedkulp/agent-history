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

On each macOS or Linux machine, install with [mise](https://mise.jdx.dev):

```sh
mise use -g github:tedkulp/agent-history
agent-history init --hub http://<host>:8080
```

`init` writes `~/.config/agent-history/collector.toml`, registers the
machine with the Hub, and installs a launchd or systemd user service that
keeps shipping new history in the background. Check on it with:

```sh
agent-history status
```

Upgrade with `mise upgrade`; the running service restarts itself onto the
new version.

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
