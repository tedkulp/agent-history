#!/usr/bin/env sh
# Regenerate the README screenshots in docs/images/ from fake history:
# build both binaries, start a throwaway Hub, sync two fake Machines to it,
# and screenshot the web UI with headless Firefox. Run from the repo root
# (`just screenshots`). Needs go, python3, curl and firefox.
set -eu

port=${PORT:-18080}
hub_url="http://127.0.0.1:$port"
work=$(mktemp -d)
trap 'kill "$hub_pid" 2>/dev/null || true; rm -rf "$work"' EXIT INT TERM

go build -o "$work/hub" ./cmd/hub
go build -o "$work/collector" ./cmd/collector
python3 scripts/screenshots/gen.py "$work/homes" internal/collector/source/hermes/testdata/schema.sql

mkdir -p "$work/data"
TZ=America/New_York "$work/hub" serve --listen "127.0.0.1:$port" --data "$work/data" \
    --backup-dir "$work/data/backups" >"$work/hub.log" 2>&1 &
hub_pid=$!
until curl -fs "$hub_url/healthz" >/dev/null; do sleep 0.2; done

# Each Machine sees only its fake home: no real history reaches the Hub.
for machine in laptop:alex-macbook desktop:alex-desktop; do
    dir="$work/homes/${machine%%:*}"
    mkdir -p "$dir/.config/agent-history"
    printf 'machine_id = "%s"\ndisplay_name = "%s"\nhub_url = "%s"\n' \
        "$(python3 -c 'import uuid; print(uuid.uuid4())')" "${machine#*:}" "$hub_url" \
        >"$dir/.config/agent-history/collector.toml"
    env -i PATH="$PATH" HOME="$dir" XDG_CONFIG_HOME="$dir/.config" XDG_STATE_HOME="$dir/.state" \
        "$work/collector" sync
done

# The Hub parses in the background; wait until every Session has a page.
until [ "$(curl -fs "$hub_url/" | grep -o 'href="/sessions/' | wc -l)" -ge 8 ]; do sleep 0.2; done
transcript=$(curl -fs "$hub_url/?q=sqlite" | grep -o 'href="/sessions/[0-9]*' | head -1 | cut -d'"' -f2)

shot() { # file url height
    firefox --headless --no-remote --profile "$work/firefox" --window-size="1400,$3" \
        --screenshot "$PWD/docs/images/$1" "$2" >/dev/null 2>&1
    echo "wrote docs/images/$1"
}
mkdir -p "$work/firefox" docs/images
shot feed.png "$hub_url/" 820
shot search.png "$hub_url/?q=orders" 620
shot transcript.png "$hub_url$transcript" 800
