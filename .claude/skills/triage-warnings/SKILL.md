---
name: triage-warnings
description: Pull every Parse warning and parse failure from a running Hub and file one GitHub issue per cause. Takes the Hub URL as its argument.
disable-model-invocation: true
---

# Triage a Hub's warnings

Turn the Hub's "has warnings" list into tickets: one ticket per **cause**, not per Session. Tickets follow `docs/agents/issue-tracker.md` and the labels in `docs/agents/triage-labels.md`.

## The Hub URL

The Hub URL is the argument (e.g. `/triage-warnings http://host:port`). With no argument, ask for it. It is **session-only**: use it on the command line and nowhere else. Tickets, commits, files and memory name Sessions by id only ("Session 340"), never by URL or hostname.

## 1. Collect

```sh
python3 .claude/skills/triage-warnings/fetch_warnings.py <hub-url>
```

It prints JSON: `groups` (one per `kind` + `source_type` + Source version, with the Sessions and the first excerpt) and `parse_failures`. It reads the web UI, since the `/api/v1` routes need a Collector's token.

Done when the JSON is in hand. Zero groups and zero failures: tell the user the Hub is clean and stop.

## 2. Sort into causes

A cause is one `kind` + `source_type` on one Source; merge groups that differ only in Source version. Parse failures group by the first line of their `parse_error`. For each cause, decide which of these it is:

- **Already fixed**: the parser on `main` already handles it (grep the `source_type` in `internal/hub/parser/<source>/` and `docs/spec/adapters/<source>.md`). The Hub is running an older release or hasn't re-parsed. No ticket.
- **Already filed**: `gh issue list --state all --search "<source_type>"` finds a ticket for it. Open → no ticket; closed but not fixed on `main` → reopen-worthy, tell the user.
- **Documented**: the adapter spec says this warning is expected (e.g. a field absent in old versions). File it `needs-triage`, since removing it is a spec decision.
- **New**: file it.

Done when every group and failure is assigned to exactly one cause and one of these four outcomes.

## 3. Draft

This repo is **public**. Excerpts come from real Transcripts, so reduce each one to its **shape** before it goes in a ticket: keep the keys, the `type`/`subtype` values, and version strings; replace prompts, titles, paths, commands and other content with `"…"`. Session ids stay; Session titles stay out.

Per cause, one ticket:

- **Title**: what to change, e.g. "Keep Claude Code's continued-in lines Raw only".
- **Body**, in the shape of #63: `## What to build` (the warning, the Source and version, the Session ids, the shape-only example line, the proposal: spec first in `docs/spec/adapters/<source>.md`, then the parser, parser version bump, CHANGELOG line) and `## Acceptance criteria` as a checklist, ending with the Sessions losing their warning after re-parse.
- **Labels**: `enhancement` or `bug`, plus `ready-for-agent` when the fix is mechanical (a Raw-only type, a known field), `needs-triage` when it needs a decision (option list and open questions, in the shape of #64, with the AI-triage note at the top).

Done when every **New** and **Documented** cause has a draft.

## 4. Confirm and file

Show the user a table: cause, Sessions, outcome, and for drafts the title and labels. File only after a yes, then `gh issue create` each. Report the issue links, plus the causes skipped as already fixed or filed.
