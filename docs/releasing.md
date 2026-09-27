# Releasing

One `vX.Y.Z` tag releases the Collector archives and the Hub image together
(`docs/spec/README.md` §5). `.github/workflows/release.yml` does the work;
`.goreleaser.yaml` says what gets built.

## Cut a release

1. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [0.2.0] - <today>`,
   add a fresh empty `## [Unreleased]` above it, and update the compare
   links at the bottom. For a new minor version, also move the pinned
   `vX.Y` image tag in `compose.yaml` and the README's "Run the Hub"
   section. Commit that on `main`.
2. On an up-to-date `main` with green CI, tag and push:

   ```sh
   git tag v0.2.0
   git push origin v0.2.0
   ```

3. The workflow reruns the tests, fails if `protocol.MinCollectorVersion` is
   above the tag's version or `CHANGELOG.md` has no section for it, runs
   `goreleaser release` with that section as the release notes, then attests
   the archives and the image.
4. If this release moves `MinCollectorVersion`, start its changelog section
   with **"⚠ requires Collector ≥ X"**.

An rc tag needs its own section too (`## [0.2.0-rc.1] - <today>`); fold the
rc sections into the final release's section when you cut it.

A `vX.Y.Z-rc.N` tag publishes a prerelease and only the `vX.Y.Z-rc.N` image
tag; `vX.Y` and `latest` stay where they are.

## Check before tagging

- `just snapshot` builds the four archives and both image architectures into
  `dist/` and publishes nothing (needs `goreleaser` on your PATH).
- `go run ./internal/release/floorcheck v0.2.0` runs the floor check.
- `go run ./internal/release/notes v0.2.0` prints the release notes, or
  fails if the changelog section is missing.

## First release only

- The first push to `ghcr.io/tedkulp/agent-history-hub` creates the package
  as private. In the package settings, make it public and give the
  `agent-history` repository write access under "Manage Actions access".
- Tag attestations need the repository's Actions to have
  `id-token: write` and `attestations: write`; the workflow asks for both.
