# Running the Hub

The Hub ships as the image `ghcr.io/tedkulp/agent-history-hub`. The sample [`compose.yaml`](../compose.yaml) in the repo root runs it:

```sh
mkdir -p data backups
docker compose up -d
docker compose ps        # STATUS shows "healthy" once the Hub is ready
```

Open `http://<host>:8080/` and point each Collector's `hub_url` at the same address.

## Storage: local disk only

`/data` holds `hub.db` and its `hub.db-wal` / `hub.db-shm` files. Put it on a **local disk**. Do not use NFS, SMB or any other network filesystem: SQLite's WAL mode needs shared memory and working file locks, and it corrupts or deadlocks without them.

`/backups` gets the daily backups. Your host's own backup tool can copy it anywhere; every file in it is a consistent SQLite database.

## File ownership

The image runs as uid/gid `1000`. The host directories mounted at `/data` and `/backups` must be writable by that user. Either:

- `sudo chown -R 1000:1000 data backups`, or
- set `user:` in `compose.yaml` to the owner of those directories, e.g. `user: "501:20"` (see `id -u` and `id -g`).

If the Hub can't write `/data`, `serve` exits at start-up with a permission error in `docker compose logs`.

## Configuration

Everything is set with env vars. There is no config file. Each has a matching `serve` flag (`--listen`, `--data`, …) that wins over the env var.

| Env | Default | Meaning |
|---|---|---|
| `AGENT_HISTORY_LISTEN` | `:8080` | Listen address |
| `AGENT_HISTORY_DATA` | `/data` | Directory holding `hub.db` |
| `AGENT_HISTORY_BACKUP_DIR` | `/backups` | Backup target |
| `AGENT_HISTORY_BACKUP_AT` | `03:00` | Daily backup time in the container's `TZ`. Empty disables scheduled backups. |
| `AGENT_HISTORY_BACKUP_KEEP` | `7` | Number of scheduled backups kept |
| `AGENT_HISTORY_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `AGENT_HISTORY_MIN_COLLECTOR_VERSION` | unset | Raises the minimum Collector version the Hub accepts |
| `TZ` | UTC | Time zone for the Web UI and the backup schedule |

An invalid value stops `serve` with exit code 1 and names the setting, e.g. `agent-history-hub: AGENT_HISTORY_BACKUP_AT: "3am" is not a time like 03:00`.

If you change `AGENT_HISTORY_LISTEN`, change the port mapping in `compose.yaml` to match. The image's healthcheck reads the same variable.

## Health

`GET /healthz` returns `200 ok` once the Hub has migrated its database and the database answers, and `503` otherwise. The image's `HEALTHCHECK` runs `agent-history-hub healthcheck`, which calls it, so the image needs no curl.

## Backups

Every file the Hub writes to `/backups` is a complete, consistent SQLite database that opens on its own in `sqlite3`. Your host's backup tool can copy the directory at any time.

| File | Written | Pruned |
|---|---|---|
| `hub-YYYYMMDD.db` | Daily at `AGENT_HISTORY_BACKUP_AT` | Only the newest `AGENT_HISTORY_BACKUP_KEEP` are kept (never fewer than the one just written) |
| `hub-YYYYMMDD-HHMMSS.db` | On demand, by `agent-history-hub backup` | Never |
| `pre-migrate-<n>.db` | Before an upgrade migrates the database from schema `n` | Never |

Take a backup now:

```sh
docker compose exec hub agent-history-hub backup
```

Ingest keeps running while a backup is written. A failed daily backup is logged at `error` and tried again the next day. It never stops the Hub. The Hub won't migrate the database if it can't write the `pre-migrate` backup first; `serve` exits with the reason instead.

Delete old manual and `pre-migrate` backups yourself once you no longer need them.

## Rolling back an upgrade

An upgrade that changes the schema writes `pre-migrate-<n>.db` before it migrates. An older image refuses to start on the migrated database, with `database is from a newer Hub; restore a pre-migrate backup to roll back`. To go back:

1. Stop the Hub: `docker compose down`
2. Copy the backup over the database: `cp backups/pre-migrate-<n>.db data/hub.db`, where `<n>` is the newest number
3. Delete the WAL files: `rm -f data/hub.db-wal data/hub.db-shm`
4. Set `image:` in `compose.yaml` to the old version's tag, e.g. `ghcr.io/tedkulp/agent-history-hub:v0.3.1`
5. Start it: `docker compose up -d`

The Hub loses whatever it ingested after the upgrade. Each Collector's next reconcile against the Hub's manifest re-sends what is still in its Sources on disk.

## Stopping

`docker compose stop` sends `SIGTERM`. The Hub stops accepting connections, gives in-flight requests up to 10 s, lets the parse worker finish its current write, folds the WAL back into `hub.db`, and exits `0`. Every chunk the Hub acked is kept. The default compose stop timeout of 10 s is usually enough; raise `stop_grace_period` if your Hub is busy.

## TLS

The Hub speaks plain HTTP. For TLS, put a reverse proxy in front of it. With [Caddy](https://caddyserver.com/), which fetches a certificate on its own:

```caddyfile
history.example.com {
	reverse_proxy hub:8080
}
```

Run Caddy in the same compose project so it reaches the Hub as `hub`, and drop the Hub's `ports:` mapping so only Caddy is exposed. Collectors then use `hub_url = "https://history.example.com"`.

## Litestream (optional)

The built-in daily backups are enough for most setups. If you want continuous replication of `hub.db` to S3-compatible storage, [Litestream](https://litestream.io/) works alongside the Hub as a sidecar container that mounts the same `/data` volume. It is not built into the image.

## Building the image locally

```sh
just image                                   # builds agent-history-hub:dev for this machine's arch
```

Then set `image: agent-history-hub:dev` in `compose.yaml`. The `Dockerfile` does not compile Go: it copies a prebuilt `linux/<arch>/agent-history-hub` from the build context, which is how GoReleaser builds the published multi-arch image.
