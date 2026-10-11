"""Write fake coding-agent history for the README screenshots.

usage: gen.py <out-dir> <hermes-schema.sql>

Each directory under <out-dir> is the home directory of one fake Machine,
holding Claude Code, Codex and Hermes Agent history in their native layouts.
"""

import datetime as dt
import json
import os
import sqlite3
import sys
import uuid

OUT, HERMES_SCHEMA = sys.argv[1], sys.argv[2]
NOW = dt.datetime(2026, 10, 10, 14, 0, tzinfo=dt.timezone.utc)
CLAUDE_MODEL = "claude-opus-5-5"


def ts(t):
    return t.strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def home(machine):
    return os.path.join(OUT, machine)


# Turns, shared by every Source:
#   ("user", text)                   a prompt
#   ("text", text)                   an assistant reply
#   ("tool", name, input, output)    a tool call and its result


def claude_code(machine, cwd, start, turns, branch="main"):
    sid = str(uuid.uuid4())
    d = os.path.join(home(machine), ".claude", "projects", cwd.replace("/", "-"))
    os.makedirs(d, exist_ok=True)
    base = dict(sessionId=sid, cwd=cwd, gitBranch=branch, version="2.1.283", isSidechain=False)
    lines, parent, t = [], None, start

    def add(typ, msg):
        nonlocal parent, t
        u = str(uuid.uuid4())
        t += dt.timedelta(seconds=10 + 3 * len(lines))
        lines.append(dict(base, type=typ, uuid=u, parentUuid=parent, timestamp=ts(t), message=msg))
        parent = u

    def assistant(content):
        add("assistant", {"id": "msg_" + uuid.uuid4().hex[:12], "role": "assistant", "model": CLAUDE_MODEL, "content": content})

    for turn in turns:
        if turn[0] == "user":
            add("user", {"role": "user", "content": turn[1]})
        elif turn[0] == "text":
            assistant([{"type": "text", "text": turn[1]}])
        else:
            _, name, inp, out = turn
            tid = "toolu_" + uuid.uuid4().hex[:12]
            assistant([{"type": "tool_use", "id": tid, "name": name, "input": inp}])
            add("user", {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tid, "content": out}]})
    with open(os.path.join(d, sid + ".jsonl"), "w") as f:
        for line in lines:
            f.write(json.dumps(line) + "\n")


def codex(machine, cwd, start, turns):
    sid = str(uuid.uuid4())
    d = os.path.join(home(machine), ".codex", "sessions", start.strftime("%Y/%m/%d"))
    os.makedirs(d, exist_ok=True)
    lines, t = [], start

    def add(typ, payload):
        nonlocal t
        t += dt.timedelta(seconds=10 + 3 * len(lines))
        lines.append({"timestamp": ts(t), "type": typ, "payload": payload})

    add("session_meta", {"id": sid, "timestamp": ts(start), "cwd": cwd, "originator": "codex_cli_rs",
                         "cli_version": "0.154.0", "source": "cli", "model_provider": "openai"})
    add("turn_context", {"cwd": cwd, "model": "gpt-5.5"})
    for turn in turns:
        if turn[0] == "user":
            add("response_item", {"type": "message", "role": "user", "content": [{"type": "input_text", "text": turn[1]}]})
        elif turn[0] == "text":
            add("response_item", {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": turn[1]}]})
        else:
            _, name, inp, out = turn
            cid = "call_" + uuid.uuid4().hex[:16]
            add("response_item", {"type": "function_call", "name": name, "arguments": json.dumps(inp), "call_id": cid})
            add("response_item", {"type": "function_call_output", "call_id": cid, "output": out})
    name = f"rollout-{start.strftime('%Y-%m-%dT%H-%M-%S')}-{sid}.jsonl"
    with open(os.path.join(d, name), "w") as f:
        for line in lines:
            f.write(json.dumps(line) + "\n")


def hermes(machine, cwd, start, title, turns):
    d = os.path.join(home(machine), ".hermes")
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, "state.db")
    fresh = not os.path.exists(path)
    db = sqlite3.connect(path)
    if fresh:
        with open(HERMES_SCHEMA) as f:
            db.executescript(f.read())
    sid = start.strftime("%Y%m%d_%H%M%S_") + uuid.uuid4().hex[:6]
    t = start.timestamp()
    db.execute("INSERT INTO sessions (id, source, model, cwd, git_branch, git_repo_root, started_at, title) VALUES (?, 'cli', 'gpt-5.5', ?, 'main', ?, ?, ?)",
               (sid, cwd, cwd, t, title))
    order = 0

    def add(role, content=None, tool_calls=None, tool_call_id=None, tool_name=None):
        nonlocal order, t
        order += 1
        t += 10 + 3 * order
        db.execute("INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, tool_name, timestamp, display_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                   (sid, role, content, tool_calls, tool_call_id, tool_name, t, order))

    for turn in turns:
        if turn[0] == "user":
            add("user", turn[1])
        elif turn[0] == "text":
            add("assistant", turn[1])
        else:
            _, name, inp, out = turn
            cid = "call_" + uuid.uuid4().hex[:8]
            add("assistant", tool_calls=json.dumps([{"id": cid, "call_id": cid, "type": "function",
                                                       "function": {"name": name, "arguments": json.dumps(inp)}}]))
            add("tool", out, tool_call_id=cid, tool_name=name)
    db.commit()
    db.close()


ago = lambda **kw: NOW - dt.timedelta(**kw)

claude_code("laptop", "/Users/alex/src/acme-api", ago(hours=3), [
    ("user", "The /orders endpoint hangs under load. I think it's the sqlite lock again — can you take a look?"),
    ("tool", "Grep", {"pattern": "BeginTx", "path": "internal/store"}, "internal/store/orders.go:88:\ttx, err := db.BeginTx(ctx, &sql.TxOptions{})"),
    ("tool", "Read", {"file_path": "/Users/alex/src/acme-api/internal/store/orders.go"}, "     88\t\ttx, err := db.BeginTx(ctx, &sql.TxOptions{})\n     89\t\tif err != nil {\n     90\t\t\treturn err\n     91\t\t}\n     92\t\trows, err := tx.QueryContext(ctx, listOrders)"),
    ("text", "Found it. `ListOrders` opens a **write** transaction just to read, so every reader queues behind the writer lock.\n\nThe fix:\n\n1. Read through the read-only pool (`db.ReadOnly()`), which runs in WAL mode.\n2. Set `busy_timeout = 5000` so the one real writer waits instead of failing.\n\n```go\nrows, err := s.ro.QueryContext(ctx, listOrders, userID)\n```"),
    ("tool", "Edit", {"file_path": "/Users/alex/src/acme-api/internal/store/orders.go", "old_string": "tx, err := db.BeginTx(ctx, &sql.TxOptions{})", "new_string": "rows, err := s.ro.QueryContext(ctx, listOrders, userID)"}, "The file has been updated."),
    ("tool", "Bash", {"command": "go test ./internal/store/... && hey -n 2000 -c 50 http://localhost:8000/orders"}, "ok  \tacme/internal/store\t0.412s\n\nSummary:\n  Total:\t1.8213 secs\n  Requests/sec:\t1098.1\n  p99:\t0.0612 secs"),
    ("text", "Tests pass and `/orders` now handles **~1,100 req/s** with a p99 of 61 ms (it was timing out at 30 s before)."),
])
codex("desktop", "/home/alex/src/acme-web", ago(hours=6), [
    ("user", "The checkout button double-submits on slow connections. Fix it."),
    ("tool", "shell", {"command": ["rg", "-n", "onSubmit", "src/checkout"]}, "src/checkout/CheckoutForm.tsx:41:  const onSubmit = async (e) => {"),
    ("text", "`onSubmit` never disables the button while the request is in flight. I'll track a `submitting` state and ignore clicks until the request settles."),
    ("tool", "apply_patch", {"input": "*** Update File: src/checkout/CheckoutForm.tsx\n-  <Button type=\"submit\">\n+  <Button type=\"submit\" disabled={submitting}>"}, "Success. Updated src/checkout/CheckoutForm.tsx"),
    ("text", "Fixed. The button is disabled from the first click until the order request returns."),
])
claude_code("laptop", "/Users/alex/src/acme-api", ago(days=1, hours=2), [
    ("user", "Add rate limiting to the public API: 100 requests per minute per API key."),
    ("text", "I'll add a token-bucket middleware keyed by API key, using `golang.org/x/time/rate`, with an LRU so idle keys don't leak memory."),
    ("tool", "Write", {"file_path": "/Users/alex/src/acme-api/internal/http/ratelimit.go", "content": "package http\n\n// RateLimit allows 100 req/min per API key.\n"}, "File created successfully."),
    ("text", "Done. Requests over the limit get `429 Too Many Requests` with a `Retry-After` header."),
], branch="feat/rate-limit")
hermes("laptop", "/Users/alex/src/infra", ago(days=1, hours=5), "Rotate the staging TLS certificates", [
    ("user", "Rotate the staging TLS certificates before they expire on Friday."),
    ("tool", "terminal", {"command": "kubectl -n staging get certificate"}, "NAME          READY   SECRET        AGE\nstaging-tls   True    staging-tls   89d"),
    ("text", "cert-manager owns `staging-tls`, so I renewed it with `cmctl renew staging-tls`. The new certificate is valid until January 8."),
])
claude_code("desktop", "/home/alex/src/blog", ago(days=1, hours=20), [
    ("user", "Write a draft post about migrating our CI from Jenkins to GitHub Actions."),
    ("text", "Here's an outline:\n\n## Why we left Jenkins\n- Plugin upgrades broke builds monthly\n- One shared agent was a bottleneck\n\n## The migration\n- Matrix builds replaced 14 copy-pasted jobs\n- Caching cut build time from 11 min to 4 min"),
    ("tool", "Write", {"file_path": "/home/alex/src/blog/content/posts/jenkins-to-actions.md", "content": "---\ntitle: Leaving Jenkins\n---\n"}, "File created successfully."),
    ("text", "Draft written to `content/posts/jenkins-to-actions.md`, 940 words."),
])
codex("laptop", "/Users/alex/src/acme-api", ago(days=2, hours=1), [
    ("user", "Write a migration that adds a nullable shipped_at column to orders."),
    ("tool", "shell", {"command": ["ls", "migrations"]}, "0011_add_api_keys.sql\n0012_orders_index.sql"),
    ("text", "Added `migrations/0013_orders_shipped_at.sql` with the column and an index on `(user_id, shipped_at)`."),
])
claude_code("laptop", "/Users/alex/src/dotfiles", ago(days=2, hours=4), [
    ("user", "Why does my zsh prompt take 800ms to show up?"),
    ("tool", "Bash", {"command": "zsh -i -c 'zprof' 2>&1 | head"}, "num  calls  time  self  name\n 1)    1  612.40  612.40  nvm_auto\n 2)    1   98.10   98.10  compinit"),
    ("text", "`nvm` is loading eagerly and costs **612 ms**. Lazy-load it, or switch to `mise`, which activates in about 5 ms."),
])
hermes("desktop", "/home/alex/src/homelab", ago(days=3, hours=5), "Grafana dashboards empty after Prometheus 3 upgrade", [
    ("user", "My Grafana dashboards are empty after upgrading Prometheus to 3.x."),
    ("tool", "terminal", {"command": "docker logs prometheus 2>&1 | tail -1"}, "level=ERROR msg=\"Error loading config\" err=\"unknown field \\\"scrape_classic_histograms\\\"\""),
    ("text", "Prometheus 3 renamed `scrape_classic_histograms` to `always_scrape_classic_histograms`. Rename it in `prometheus.yml` and restart."),
])
