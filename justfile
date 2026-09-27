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
    go run ./cmd/hub serve --listen :8080 --data .data --log-level debug

# Run the Collector in the foreground
collector:
    go run ./cmd/collector run

# Remove build output and local Hub data
clean:
    rm -rf bin .data
