---
name: release
description: Use when releasing a new version of agent-history (final or rc): cutting the CHANGELOG section, tagging, and pushing to trigger the release workflow.
---

# agent-history release

One `vX.Y.Z` tag releases the Collector archives and the Hub image together. Pushing the tag runs `.github/workflows/release.yml`, which fails before publishing if the tag's `CHANGELOG.md` section is missing or `protocol.MinCollectorVersion` is above the tag, then runs GoReleaser with that section as the GitHub Release notes. Your job is to get the repo into a state where that run succeeds, then tag. `docs/releasing.md` is the reference for everything around it (first-release setup, `just snapshot`).

## 1. Check the starting point

```bash
git switch main && git pull --ff-only
git status --short
gh run list --branch main --workflow ci.yml --limit 1
```

Done when: on `main`, level with `origin/main`, the tree is clean, and the latest CI run on `main` is green. If any fails, stop and tell the user.

## 2. Pick the version

```bash
git tag --sort=-version:refname | head -5
git log --oneline "$(git describe --tags --abbrev=0)"..HEAD
```

Read `## [Unreleased]` in `CHANGELOG.md` and choose by semver. While the version is `0.x`, breaking changes bump the minor:

- **Patch** (`0.2.0 → 0.2.1`): only `Fixed` / `Security` entries
- **Minor** (`0.2.x → 0.3.0`): anything `Added`, `Changed`, `Deprecated` or `Removed`, and any breaking change to the protocol, config or database
- **rc** (`v0.3.0-rc.1`, then `-rc.2`…): a prerelease of the next version. It moves only its own image tag; `vX.Y` and `latest` stay put.

Check whether the Collector floor moved since the last tag:

```bash
git diff "$(git describe --tags --abbrev=0)" -- protocol/protocol.go | grep MinCollectorVersion
```

Done when: the user has confirmed the version. Propose it with a one-line reason (and whether the floor moved), then wait for a yes before editing anything.

## 3. Cut the CHANGELOG section

In `CHANGELOG.md`:

1. Rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` (today's date; drop the `v`). Put a fresh, empty `## [Unreleased]` above it.
2. If the floor moved, make the section's first line `**⚠ requires Collector ≥ <MinCollectorVersion>**`.
3. Final release after rcs: fold every `## [X.Y.Z-rc.N]` section into the new `## [X.Y.Z]` section, merging duplicate groups, and delete the rc sections and their links. The final notes cover everything since the last final release.
4. Update the link references at the bottom: `[Unreleased]` compares `vX.Y.Z...HEAD`, and add `[X.Y.Z]` comparing the previous tag to `vX.Y.Z` (or `releases/tag/vX.Y.Z` for the first).
5. Tidy the entries for people reading release notes: one line each, in the groups Added, Changed, Deprecated, Removed, Fixed, Security, with no entries for fixes to things never released.

For a new minor version (final, not rc), also move the pinned `vX.Y` image tag in `compose.yaml` and the README's "Run the Hub" section. Done when `grep -n 'agent-history-hub:v' compose.yaml README.md` shows only the new `vX.Y`.

Done when this prints the new section and exits 0:

```bash
go run ./internal/release/notes vX.Y.Z
go run ./internal/release/floorcheck vX.Y.Z
```

## 4. Commit, push, tag

```bash
go test ./...
git add CHANGELOG.md compose.yaml README.md
git commit -m "Release vX.Y.Z: <one-line summary>"
git push origin main
git tag vX.Y.Z
git push origin vX.Y.Z
```

Commit first, then tag, so the tag holds the CHANGELOG section the workflow reads. `git push` never pushes tags; the tag needs its own push.

Done when both pushes succeed.

## 5. Watch the release

```bash
gh run list --workflow release.yml --limit 1
gh run watch <run id> --exit-status
gh release view vX.Y.Z
```

Done when the run is green and the GitHub Release shows the CHANGELOG section as its notes, the four Collector archives, and checksums. The Hub image is `ghcr.io/tedkulp/agent-history-hub:vX.Y.Z` (plus `vX.Y` and `latest` for a final release).

If the run fails, report the failing step and its log (`gh run view <run id> --log-failed`), and stop.
- Failed before the GoReleaser step: nothing was published. Fix on `main`, then, once the user approves, move the tag with `git tag -f vX.Y.Z && git push -f origin vX.Y.Z`.
- Failed in or after GoReleaser: a GitHub Release or image may already exist. Check `gh release view vX.Y.Z` and tell the user what is out there; the usual way on is a new patch version rather than moving the tag.
