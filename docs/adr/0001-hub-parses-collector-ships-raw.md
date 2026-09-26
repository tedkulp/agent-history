# The Hub parses Source formats; the Collector only ships raw data

The four Sources (Claude Code, Codex, oh-my-pi, opencode) store history in different on-disk formats that change without notice. We decided the Collector only discovers each Source's files and ships the Raw records verbatim, and the Hub does all parsing into Transcripts. When a format drifts, one Hub redeploy fixes it, instead of upgrading the Collector on every Machine. Because the Hub keeps every Raw record, it can re-parse old Sessions with a corrected parser.

## Considered Options

- **Collector parses, ships normalized Transcripts + raw**: spreads the upload work across Machines and keeps the Hub simpler, but every parser fix needs a Collector release rolled out to every Machine, and stale Collectors would keep producing bad Transcripts.

## Consequences

- The Collector still needs per-Source *discovery* logic (paths, what counts as a Session, change detection). Only parsing moves to the Hub.
- The Hub must store Raw records durably and support re-parsing them. See the "Re-parse flow" fog on the map.
- Upload volume is raw size, not normalized size. That's acceptable at the expected scale (under 10 Machines, a few GB).
