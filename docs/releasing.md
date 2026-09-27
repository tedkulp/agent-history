# Releasing

One `vX.Y.Z` tag releases the Collector archives and the Hub image together
(`docs/spec/README.md` §5). `.github/workflows/release.yml` does the work;
`.goreleaser.yaml` says what gets built.

## Cut a release

1. On an up-to-date `main` with green CI, tag and push:

   ```sh
   git tag v0.2.0
   git push origin v0.2.0
   ```

2. The workflow reruns the tests, fails if `protocol.MinCollectorVersion` is
   above the tag's version, runs `goreleaser release`, then attests the
   archives and the image.
3. If this release moves `MinCollectorVersion`, add
   **"⚠ requires Collector ≥ X"** to the GitHub Release notes by hand.

A `vX.Y.Z-rc.N` tag publishes a prerelease and only the `vX.Y.Z-rc.N` image
tag; `vX.Y` and `latest` stay where they are.

## Check before tagging

- `just snapshot` builds the four archives and both image architectures into
  `dist/` and publishes nothing (needs `goreleaser` on your PATH).
- `go run ./internal/release/floorcheck v0.2.0` runs the floor check.

## First release only

- The first push to `ghcr.io/tedkulp/agent-history-hub` creates the package
  as private. In the package settings, make it public and give the
  `agent-history` repository write access under "Manage Actions access".
- Tag attestations need the repository's Actions to have
  `id-token: write` and `attestations: write`; the workflow asks for both.
