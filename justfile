# Development tasks for agent-history. Run `just` to list them.

version := env("VERSION", "0.0.0-dev")
ldflags := "-X github.com/tedkulp/agent-history/internal/buildinfo.Version=" + version

# List recipes
default:
    @just --list

# Regenerate templ output
generate:
    go generate ./...

# Build the Collector and Hub into bin/
build: generate
    CGO_ENABLED=0 go build -ldflags "{{ldflags}}" -o bin/agent-history ./cmd/collector
    CGO_ENABLED=0 go build -ldflags "{{ldflags}}" -o bin/agent-history-hub ./cmd/hub

# Build the Hub image for this machine's arch as agent-history-hub:dev
image: generate
    #!/usr/bin/env sh
    set -eu
    arch=$(go env GOARCH)
    rm -rf bin/image
    CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -ldflags "{{ldflags}}" -o bin/image/linux/$arch/agent-history-hub ./cmd/hub
    docker build --platform linux/$arch -f Dockerfile -t agent-history-hub:dev bin/image

# Build every release artifact into dist/ without publishing (needs goreleaser)
snapshot:
    goreleaser release --snapshot --clean

# Run all tests, or one package: just test ./internal/collector/service
test pkg="./...":
    go test {{pkg}}

# Rewrite golden files in one package
golden pkg:
    go test {{pkg}} -update

# Check formatting and vet
lint:
    test -z "$(gofmt -l .)" || (gofmt -l . && exit 1)
    go vet ./...

# Format all Go files
fmt:
    gofmt -w .

# Lint, then test: run before committing
check: lint test

# Run the Hub locally with its database in .data/
hub: generate
    mkdir -p .data
    go run ./cmd/hub serve --listen :8080 --data .data --backup-dir .data/backups --log-level debug

# Run a Collector command (default: run) against the local Hub, beside any installed Collector
collector cmd="run":
    #!/usr/bin/env sh
    set -eu
    # Config and state live in .data/collector, apart from an installed
    # Collector's, so the two don't share collector.lock. The first run writes
    # a config for a new "(dev)" Machine; edit it to change the Hub URL.
    export XDG_CONFIG_HOME="$PWD/.data/collector/config" XDG_STATE_HOME="$PWD/.data/collector/state"
    cfg="$XDG_CONFIG_HOME/agent-history/collector.toml"
    if [ ! -f "$cfg" ]; then
        mkdir -p "$(dirname "$cfg")"
        id=$(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid)
        printf 'machine_id = "%s"\ndisplay_name = "%s (dev)"\nhub_url = "http://localhost:8080"\n' \
            "$(echo "$id" | tr 'A-Z' 'a-z')" "$(hostname)" > "$cfg"
    fi
    go run ./cmd/collector {{cmd}}

# Remove build output and local Hub data
clean:
    rm -rf bin .data
