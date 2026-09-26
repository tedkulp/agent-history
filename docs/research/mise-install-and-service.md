# Distributing the Collector via mise, and registering it as a per-user service

## TL;DR

- **Distribution**: Use mise's [`github:` backend](https://mise.jdx.dev/dev-tools/backends/github.html) (mise's own native re-implementation of the ideas behind the [`ubi` backend](https://mise.jdx.dev/dev-tools/backends/ubi.html), which is now **deprecated** in favor of `github:`/`gitlab:`/`http:`). It downloads a matching asset straight from GitHub Releases with no registry entry required, auto-detects OS/arch/libc from the asset filename, and lets you pin an `asset_pattern`/`checksum` when auto-detection isn't good enough. The [`aqua:` backend](https://mise.jdx.dev/dev-tools/backends/aqua.html) is registry-based and is more work for a project not already in the (large, third-party) aqua registry — skip it unless the project ends up needing aqua-specific features (attestation, richer registry metadata already curated by someone else).
- **Stable path for services**: mise keeps a version-independent **shim** at `~/.local/share/mise/shims/<bin>` for every installed tool. On Unix this shim is a **symlink to the mise binary itself** (mise dispatches based on `argv[0]`), and it is re-pointed transparently on every `mise upgrade`/`mise install` — the shim path itself never changes, only what it resolves to underneath ([mise shims docs](https://mise.jdx.dev/dev-tools/shims.html)). This is exactly the stable path a launchd/systemd unit should reference.
- **Service registration**: Hand-roll the `launchd` plist and `systemd --user` unit yourself, pointing `Program`/`ExecStart` at the mise shim path as a literal string. **Do not** use [`kardianos/service`](https://github.com/kardianos/service)'s default install path resolution for this: it calls `os.Executable()` to determine the binary path to bake into the service definition, and `os.Executable()` resolves through symlinks to the real underlying binary (e.g., the `mise` binary itself, not the collector) if the process happened to be launched via the mise shim at `service install` time. `kardianos/service` does let you override this via `Config.Executable`, so it *can* be made to work, but the "just call `service.New()`" default path is actively wrong for this deployment shape. A small, explicit plist/unit template avoids the footgun entirely and is well within reach for two file formats this simple.

---

## Part A: mise distribution mechanics

### A1. The `github:` backend (and its relationship to `ubi`)

mise's `github:` backend "downloads release assets from GitHub repositories and is ideal for tools that distribute pre-built binaries through GitHub releases" ([mise github backend docs](https://mise.jdx.dev/dev-tools/backends/github.html)). Declare it in `mise.toml`:

```toml
[tools]
"github:tedkulp/agent-history-collector" = "latest"
```

or via the CLI:

```sh
mise use github:tedkulp/agent-history-collector
```

More advanced installs can carry inline options, e.g. narrowing to a specific asset and renaming the extracted executable:

```sh
mise use "github:oxc-project/oxc[matching=oxlint,rename_exe=oxlint]@apps_v1.69.0"
```
([mise github backend docs](https://mise.jdx.dev/dev-tools/backends/github.html))

Separately, mise has (had) a `ubi:` backend that wrapped the same asset-matching idea as the standalone [`ubi` (Universal Binary Installer) project](https://github.com/houseabsolute/ubi). As of the current docs, **the `ubi:` backend is marked deprecated**, with guidance to migrate to `github:`, `gitlab:`, or `http:` instead ([mise ubi backend docs](https://mise.jdx.dev/dev-tools/backends/ubi.html)). The `github:` backend is effectively the maintained successor for this use case — for a new project like the Collector, `github:` is the correct one to target, not `ubi:`.

`ubi:`-syntax is still documented for reference:

```toml
[tools]
"ubi:owner/repo" = "version"
```

with options such as `exe` (executable name inside the archive when it differs from the repo name), `matching` (substring filter among candidates), `matching_regex`, `rename_exe`, `extract_all`, `bin_path`, `tag_regex` (filter releases by tag when a repo publishes multiple CLIs), `provider` (github/gitlab), and `api_url` for self-hosted instances ([mise ubi backend docs](https://mise.jdx.dev/dev-tools/backends/ubi.html)).

### A2. Asset naming/matching conventions

Without any explicit configuration, mise's `github:` backend auto-selects the right release asset by scoring candidates on:

- OS (linux/macos/windows)
- Architecture (x64/arm64/x86/arm)
- libc variant (gnu/musl on Linux, msvc on Windows)
- archive format preference
- avoidance of unwanted build variants

([mise github backend docs](https://mise.jdx.dev/dev-tools/backends/github.html))

This means a conventional naming scheme — e.g. `agent-history-collector_darwin_amd64.tar.gz`, `agent-history-collector_darwin_arm64.tar.gz`, `agent-history-collector_linux_amd64.tar.gz`, `agent-history-collector_linux_arm64.tar.gz` — should auto-match with **zero extra configuration**, which is the common pattern for Go binaries built with `goreleaser`.

For less-standard naming, mise supports two escape hatches:

1. **`matching`** — narrows the asset selection to names containing a given substring "while keeping platform autodetection" (a soft filter layered on top of auto-detection).
2. **`asset_pattern`** — a full manual override:

   ```toml
   [tools]
   "github:cli/cli" = { version = "latest", asset_pattern = "gh_*_linux_amd64.tar.gz" }
   ```

([mise github backend docs](https://mise.jdx.dev/dev-tools/backends/github.html))

The underlying `ubi` project's own matching algorithm (which the `ubi:`/older `github:` matching logic is modeled on) is documented in more granular, step-by-step form in its README ([houseabsolute/ubi](https://github.com/houseabsolute/ubi)):

1. Filter out assets with unrecognized extensions, keeping archives (`.tar.gz`, `.zip`, etc.) and bare executables.
2. Filter by OS via regex, then by CPU architecture.
3. On musl-based Linux, prefer musl-tagged assets over glibc ones when both are present (matches on `-gnu`/`-musl` in the filename).
4. **Disambiguating multiple remaining candidates**: drop 32-bit assets on a 64-bit platform; apply the user-supplied `--matching` string as a filter if given; on macOS arm64 drop non-arm64 variants; **and if more than one asset is still left after all that, ubi sorts by filename and picks the first one alphabetically** — i.e., ambiguity is resolved deterministically but silently, not by erroring out, unless you've supplied a `--matching-regex` that fails to narrow to exactly one asset (in which case that specific flow does error).
5. Inside an archive, it looks for a file matching the project name, then partial name matches (must be executable on Unix; `.bat`/`.exe` on Windows).

**Recommendation for the Collector's release process**: adopt an unambiguous, single-asset-per-OS/arch naming scheme (goreleaser's default `{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}.tar.gz` pattern works well) so mise's auto-detection never needs to fall back to alphabetical tie-breaking, and so no `asset_pattern` override is needed in the `.mise.toml` that consumers write.

The mise `github:` docs describe checksum pinning explicitly:

```toml
[tools."github:owner/repo"]
version = "1.0.0"
checksum = "sha256:REPLACE_WITH_THE_64_HEX_DIGIT_DIGEST"
```

and note that mise additionally verifies GitHub Artifact Attestations / SLSA provenance when the release publishes them ([mise github backend docs](https://mise.jdx.dev/dev-tools/backends/github.html)) — worth wiring up on the release pipeline (e.g. via `actions/attest-build-provenance` in the GitHub Actions release workflow) since it's effectively free hardening once mise is the install path.

### A3. The `aqua:` backend

mise ships a **native re-implementation** of the aqua backend: "mise does not use the aqua CLI at all; it uses the aqua registry, which is compiled into the mise binary on release" ([mise aqua backend docs](https://mise.jdx.dev/dev-tools/backends/aqua.html)). The aqua registry is a large, community-maintained catalog of package definitions (platform names, download URL templates, checksum/verification metadata) that mise bundles a snapshot of at build time, with an option (`registry_floating`) to check the live upstream registry first.

Practically: **a tool that isn't already an entry in the aqua registry gets no benefit from this backend** — there's no mechanism shown in the docs for a project to self-register or supply its own aqua-format recipe inline the way `asset_pattern`/`checksum` let you do with `github:`. The docs describe fixing bad *existing* entries ("report it to the aqua registry or contribute a correction") but not adding new ones from a `mise.toml`. For a brand-new, project-specific binary like the Collector, this means either (a) upstreaming a registry entry to the aqua project and waiting on that review process, or (b) skipping aqua entirely. Given `github:` already covers the need with zero external dependencies, **`aqua:` is not worth pursuing for this project** unless there's a future desire to be listed in that ecosystem for unrelated discoverability reasons.

### A4. Install paths and `mise upgrade` behavior

mise's directories are documented at <https://mise.jdx.dev/directories.html>:

- **Data dir**: `~/.local/share/mise` (override: `MISE_DATA_DIR`) — "tool installations, plugins, shims, and command wrappers."
- **Installs dir**: `~/.local/share/mise/installs`, with per-tool/per-version subdirectories, e.g. `installs/node/24.0.0` (override: `MISE_INSTALLS_DIR`).
- **Shims dir**: `~/.local/share/mise/shims` (override: `MISE_SHIMS_DIR` / `shims_dir` setting).
- **Downloads dir**: `~/.local/share/mise/downloads` — scratch space for tarballs during install, cleaned up afterward by default.

So a `github:`-installed Collector binary at version `1.4.0` would live at a path shaped like `~/.local/share/mise/installs/github-tedkulp-agent-history-collector/1.4.0/...` (exact naming of the tool-id segment depends on how mise namespaces `github:` backend tools — verify against a real install, see Open Questions) — **this path changes every time the pinned/resolved version changes**, which is precisely why nothing should hardcode it.

`mise upgrade` re-resolves the newest version satisfying the configured range (e.g. `node = "20"` upgrades within the 20.x line), **installs the new version alongside the old one** rather than replacing it in place, and by default **does not delete the old version** — "the old version is left in place and is not scheduled for removal" unless `--prune` is passed or `upgrade.auto_prune` is configured ([mise upgrade docs](https://mise.jdx.dev/cli/upgrade.html)). The `mise.toml` version pin is only rewritten if `--bump` is passed. The docs don't explicitly narrate shim repointing as a separate step, but this falls out naturally from how shims work (next section): the shim doesn't statically point at a version-specific path at all, so there's nothing to repoint.

### A5. Shims: the stable path mechanism

This is the load-bearing fact for Part B. Per mise's shims documentation ([mise shims docs](https://mise.jdx.dev/dev-tools/shims.html)):

- Shims live at `~/.local/share/mise/shims/<bin-name>` by default.
- **On Unix, shims are symlinks to the mise binary itself**, e.g. `~/.local/share/mise/shims/node -> ~/.local/bin/mise`. mise inspects `argv[0]` (the name it was invoked as) to figure out which tool/version to actually run, then executes the real, currently-active versioned binary.
- Because of this indirection, **the shim's path on disk never changes across upgrades** — only the mapping mise looks up internally when the shim fires changes. This is corroborated by mise's own GitHub discussion of shim internals, which independently describes command-wrapper shims as "symlinks to the mise binary… invoking one runs mise with argv[0] = cargo" ([jdx/mise discussion #7605](https://github.com/jdx/mise/discussions/7605)) — used here only to corroborate the official docs' description of the mechanism, not as the primary citation for the behavior itself.
- Shims "resolve the environment when a command is invoked" dynamically — i.e., each invocation re-resolves which version is "active" (honoring cwd-local `.mise.toml`/`.tool-versions` if present, otherwise falling back to the global config) rather than baking in a version at shim-creation time.
- Performance: resolution happens on every invocation, so a tight loop repeatedly re-invoking a shim pays repeated resolution overhead; mise recommends `mise exec` to resolve once and then invoke the real binary directly for that use case. This is a completely different usage pattern from a long-running background service (which invokes the shim exactly once, at process start, and then runs indefinitely) — so this overhead is a non-issue for the Collector's service use case.
- Trade-off vs. `mise activate`/PATH activation: PATH activation is preferred for interactive shells; shims are the mechanism meant to work "outside activated shells" — i.e., exactly the situation a launchd/systemd service is in (it has no shell, no `.zshrc`/`.bashrc`, no `mise activate` hook).

**This confirms the plan**: `~/.local/share/mise/shims/agent-history-collector` is a path that (a) exists without needing shell activation, (b) is stable across `mise upgrade`, and (c) is a real filesystem entry (symlink) that both `launchd`'s `execve`-based `Program` key and `systemd`'s `ExecStart=` can point at directly, since both exec paths transparently follow symlinks at the kernel level.

---

## Part B: per-user service registration

### B1. launchd (macOS) — per-user LaunchAgents

Primary source: Apple's `launchd.plist(5)` man page, shipped with macOS itself (`man launchd.plist`), and Apple's developer documentation on creating launchd jobs.

- **Location**: "Property list files describing agents are installed in `/Library/LaunchAgents` or in the `LaunchAgents` subdirectory of an individual user's `Library` directory" — i.e. `~/Library/LaunchAgents/*.plist` for per-user agents ([Apple: Creating Launchd Jobs](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)).
- **`Program` key**: "maps to the first argument of `execv(3)` and indicates **the absolute path to the executable** for the job. If this key is missing, then the first element of the array of strings provided to the `ProgramArguments` will be used instead." The man page is explicit and emphatic about the absolute-path requirement: **"NOTE: The `Program` key must be an absolute path. Previous versions of launchd did not enforce this requirement but failed to run the job."** (`man launchd.plist`, XML PROPERTY LIST KEYS section).
- **`ProgramArguments`**: "maps to the second argument of `execvp(3)`" and is required in the absence of `Program`.
- **`RunAtLoad` (boolean)**: "used to control whether your job is launched once at the time the job is loaded. The default is false." The man page also cautions: "This key should be avoided, as speculative job launches have an adverse effect on system-boot and user-login scenarios" — worth noting but not disqualifying for a per-user background service that's supposed to start at login.
- **`KeepAlive` (boolean or dict)**: "used to control whether your job is to be kept continuously running... The value may be set to `true` to unconditionally keep the job alive... The use of this key implicitly implies `RunAtLoad`." Conditional forms exist (`SuccessfulExit`, `PathState`, `OtherJobEnabled`, `Crashed`) for more nuanced restart policies.
- Loading: `launchctl load ~/Library/LaunchAgents/<label>.plist` (or the newer `launchctl bootstrap gui/<uid> <path>` domain-targeted form described in `man launchctl`).

Since `Program` requires only that the path be absolute — not that it be a "real" (non-symlink) file — pointing it at `~/Library/LaunchAgents`'s sibling, the mise shim path (`$HOME/.local/share/mise/shims/agent-history-collector`), satisfies the requirement: `execv(3)` follows symlinks transparently at the kernel level, so launchd doesn't care that the target is itself a symlink to the `mise` binary underneath.

### B2. systemd --user (Linux)

Primary source: freedesktop.org's official systemd man pages (`systemd.service(5)`, `systemd.unit(5)`, `loginctl(1)`), current/latest versions.

- **User unit search path**: for `--user` mode, unit files load from a stack of directories including `$XDG_CONFIG_HOME/systemd/user` (or `~/.config/systemd/user` when `XDG_CONFIG_HOME` is unset), among others such as `$XDG_RUNTIME_DIR/systemd/user`, `/etc/systemd/user`, and `/usr/lib/systemd/user` (`systemd.unit(5)`, "User Unit Search Path" / "Load path when running in user mode (--user)" tables).
- **`ExecStart=` path rules**: "For each command, the first argument must be either an absolute path to an executable or a simple file name without any slashes. If the command is not a full (absolute) path, it will be resolved to a full path using a fixed search path determined at compilation time" (searching `/usr/local/bin/`, `/usr/bin/`, etc.) (`systemd.service(5)`, `ExecStart=` section). A path under `~/.local/share/mise/shims/` is absolute once `$HOME` is expanded, and systemd unit files support `%h`/environment-variable-based path construction for exactly this (see next point).
- **Enabling/starting**: `systemctl --user enable <unit>` / `systemctl --user start <unit>`.
- **Running without an active login session — lingering**: `loginctl enable-linger [USER...]` — "Enable/disable user lingering for one or more users. If enabled for a specific user, a user manager is spawned for the user at boot and kept around after logouts. This allows users who are not logged in to run long-running services." (`loginctl(1)`, "enable-linger" section). Without lingering enabled, a `systemd --user` service normally only runs while that user has an active session (e.g. is logged in via SSH or a GUI session) — for a background Collector that should run continuously on a desktop machine, `loginctl enable-linger $USER` is the mechanism that keeps the user's systemd instance (and hence the Collector unit) alive across logout.
- **Environment considerations**: systemd's `--user` manager does **not** source the user's shell startup files (`.bashrc`/`.zshrc`/`.profile`); the environment a unit sees is whatever systemd's user manager itself was started with (typically inherited from the login session's PAM environment, plus anything set via `Environment=`/`EnvironmentFile=` in the unit, or `systemctl --user import-environment`/`systemctl --user set-environment`). This matters if the Collector's `ExecStart` line relies on `PATH` implicitly — but since we recommend pointing `ExecStart` directly at the absolute mise shim path rather than a bare command name resolved via `PATH`, this is moot for locating the binary itself. It could still matter if the Collector binary's *own* runtime logic shells out to other tools expecting an interactive-shell-like `PATH` — worth checking against the Collector's actual runtime dependencies (out of scope for this doc).

### B3. Recommended service-file shape

Given A5 + B1/B2, the safe pattern is: **write the plist/unit by hand (or via a small Go text/template baked into the Collector's own `agent-history-collector service install` subcommand), hardcoding `Program`/`ExecStart` to the mise shim path, not to any versioned install path.**

launchd (`~/Library/LaunchAgents/com.tedkulp.agent-history-collector.plist`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.tedkulp.agent-history-collector</string>
  <key>Program</key>
  <string>/Users/USERNAME/.local/share/mise/shims/agent-history-collector</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/USERNAME/.local/share/mise/shims/agent-history-collector</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```

systemd `--user` (`~/.config/systemd/user/agent-history-collector.service`):

```ini
[Unit]
Description=Agent History Collector

[Service]
ExecStart=%h/.local/share/mise/shims/agent-history-collector run
Restart=on-failure

[Install]
WantedBy=default.target
```

(then `systemctl --user daemon-reload && systemctl --user enable --now agent-history-collector.service`, and `loginctl enable-linger $USER` so it survives logout).

`%h` is systemd's specifier for the invoking user's home directory, avoiding the need to hardcode `$HOME` — this is standard systemd specifier syntax per `systemd.unit(5)`.

### B4. `kardianos/service` — what it actually does, and why not to lean on its default path resolution

[`kardianos/service`](https://github.com/kardianos/service) is a Go library providing a unified `Install`/`Uninstall`/`Start`/`Stop` API across Windows (SCM), macOS (launchd), and Linux (systemd, Upstart, SysV, OpenRC). Reading its source directly (not just the README):

- Its README states supported systems as "Windows XP+, Linux/(systemd | Upstart | SysV), and OSX/Launchd."
- It exposes a `UserService` config option (`Config.Option["UserService"]`) that, when true, targets the per-user domain: on darwin this writes to `~/Library/LaunchAgents/<name>.plist` instead of `/Library/LaunchDaemons/`; on Linux/systemd, `s.isUserService()` gates using `systemctl --user` semantics and the systemd unit template ([`service_darwin.go`](https://github.com/kardianos/service/blob/master/service_darwin.go), [`service_systemd_linux.go`](https://github.com/kardianos/service/blob/master/service_systemd_linux.go), confirmed by direct read of source, not just README).
- **The critical finding**: the executable path baked into the generated plist/unit comes from `(*Config).execPath()`, defined in [`service_go1.8.go`](https://github.com/kardianos/service/blob/master/service_go1.8.go):

  ```go
  func (c *Config) execPath() (string, error) {
      if len(c.Executable) != 0 {
          return filepath.Abs(c.Executable)
      }
      return os.Executable()
  }
  ```

  i.e., **unless the caller explicitly sets `Config.Executable`, kardianos/service falls back to `os.Executable()`** — the path of the currently-running process's binary, as resolved by the OS (on Linux, via `/proc/self/exe`, which resolves through any symlink to the final real file backing the running process; on Darwin, via `_NSGetExecutablePath` plus symlink resolution). Both `service_darwin.go` and `service_systemd_linux.go` call this same `execPath()` and template it directly into `Program`/`ExecStart` (confirmed by source: `path, err := s.execPath()` in both files, feeding the `"Path"` template variable used in both the plist `<string>{{Path}}</string>` and the unit's `ExecStart={{Path | cmdEscape}}` lines).

  **The consequence for this project**: if `agent-history-collector service install` is implemented by calling `kardianos/service`'s default `New(...)`/`Install()` without setting `Config.Executable`, and that install command was itself launched via the mise shim (the normal, expected way a user would invoke it, e.g. `agent-history-collector service install` typed at a shell where the shim is on `PATH`), then `os.Executable()` resolves through the shim symlink to **the underlying `mise` binary**, not to the collector. The generated service file would then try to `exec` `mise` (with no arguments telling it what to do), which is entirely wrong and would fail or behave unpredictably at every subsequent launch. This is exactly the failure mode the ticket is worried about, and it's real, not hypothetical, per direct reading of the library's own source.

  **The fix, if `kardianos/service` is used at all**, is to explicitly set `Config.Executable` to the mise shim path (a string constant/config value the Collector already knows, since it can compute `~/.local/share/mise/shims/<its own binary name>` from `os.UserHomeDir()`), rather than relying on the library's `os.Executable()` default. Given how thin the actual win from `kardianos/service` is here — it saves writing ~15-20 lines of plist/unit template, at the cost of a library dependency and having to remember to override this one field correctly — **hand-rolling the two small text/templates (as sketched in B3) is the safer, more transparent choice**, and avoids the failure mode entirely rather than relying on a config field being set correctly at every call site.

  Documented limitations, per the README/source, worth flagging independently: "Dependencies field is not implemented for Linux systems and Launchd," and an open bug that "OS X when running as a UserService Interactive will not be accurate."

### B5. Shim-as-service-target: any compatibility gotchas?

- **Symlink exec is transparent to both service managers.** Neither `launchd`'s `execv(3)`-based `Program` nor systemd's `ExecStart=` require the target to be a "real" file rather than a symlink; both ultimately go through the kernel's `execve()`, which resolves symlinks as part of path lookup. No special configuration is needed on either side.
- **No shell, no rc files.** Neither launchd nor systemd (`--user` or system) source `.bashrc`/`.zshrc`/`.profile` when launching a job — this is standard behavior for both process-spawning models (launchd calls `execv`/`execvp` directly per the man page's `ProgramArguments` description; systemd's unit files are explicit about not being a shell environment, per `systemd.service(5)`'s `ExecStart=` semantics quoted above, which describes literal argv construction, not shell invocation, unless you deliberately prefix the command with `sh -c` via the `@`/other specifiers). Because the recommended pattern points directly at an absolute shim path rather than relying on `PATH` lookup or shell-level `mise activate` hooks, this is not a problem for locating the binary — mise's shims are specifically designed to "work outside of an activated shell" for this exact reason (per the shims doc, A5 above).
- **mise's own environment resolution still needs `$HOME` (and no other special shell state).** Since the shim is a symlink to the `mise` binary, and mise resolves the active version/config by reading `$HOME`-relative config files, `$HOME` must be present in the unit's/plist's environment. launchd per-user agents are launched by the per-user launchd instance with the invoking user's environment already populated (via `/usr/libexec/opendirectoryd`/session bootstrap), and systemd `--user` unit environments always have `$HOME` set as it's fundamental to XDG environment setup for user services. No explicit `Environment=HOME=...`/`EnvironmentVariables` overrides should be needed for this alone in the common case — but this is asserted from general systemd/launchd session-environment behavior rather than a single citable line in the docs fetched above, so it's listed as a good sanity-check item in Open Questions rather than a fully nailed-down citation.
- **No interpreter startup weirdness.** Since both the shim (native binary/symlink) and the collector binary itself (a plain compiled Go binary) are native executables, not interpreted scripts, there's no "shebang script needs its interpreter on PATH" class of problem that can arise with e.g. Python/Node-based CLI shims.

---

## Concrete recommended approach

1. **Release**: Publish `.tar.gz` (or `.zip` on nothing — macOS/Linux only per the ticket) archives per OS/arch on GitHub Releases with goreleaser-style unambiguous names (`agent-history-collector_darwin_arm64.tar.gz`, etc.), so mise's `github:` backend auto-detects with no `asset_pattern` override needed. Optionally wire up checksums/build attestations in the release workflow so mise's `checksum`/attestation verification has something to check.
2. **Install docs / `.mise.toml` snippet for users**:
   ```toml
   [tools]
   "github:tedkulp/agent-history-collector" = "latest"
   ```
3. **Service install**: Ship an `agent-history-collector service install` subcommand in the Go binary that:
   - Computes the mise shim path itself: `filepath.Join(os.UserHomeDir(), ".local/share/mise/shims/agent-history-collector")` (allow an override flag/env var in case a user has customized `MISE_SHIMS_DIR` or `MISE_DATA_DIR`).
   - Writes `~/Library/LaunchAgents/<label>.plist` on `darwin` (per B3), or `~/.config/systemd/user/<name>.service` on `linux` (per B3), using that shim path as `Program`/`ExecStart` — via a small hand-rolled template, not `kardianos/service`'s default `os.Executable()`-driven install path.
   - On Linux, also either invoke `loginctl enable-linger $(whoami)` itself (needs no special privilege for the user's own account) or clearly instruct the user to run it, so the Collector keeps running after logout.
   - Runs `launchctl load`/`launchctl bootstrap` (macOS) or `systemctl --user daemon-reload && systemctl --user enable --now` (Linux) to actually activate it.
4. **If `kardianos/service` is adopted anyway** (e.g. for its cross-platform `Start`/`Stop`/`Uninstall`/log-redirection conveniences), **always set `Config.Executable` explicitly** to the computed shim path rather than leaving it to default to `os.Executable()`.
5. **Upgrades are then a non-event for the service layer**: `mise upgrade` (or `mise upgrade github:tedkulp/agent-history-collector`) installs the new version under a new versioned directory and repoints the shim's internal resolution — the plist/unit file never needs to be rewritten, and the running service simply executes the newer binary the next time launchd/systemd (re)starts it. If `KeepAlive`/`Restart=on-failure` are set, the *already-running* process from before the upgrade will keep running the old binary in memory until it's restarted (e.g. via `launchctl kickstart` / `systemctl --user restart`) — the install flow should probably restart the service explicitly after an upgrade rather than assuming a fresh exec happens automatically, since upgrading files on disk doesn't affect an already-running process's loaded image.

---

## Open questions / caveats

- **Exact tool-id path segment mise uses for `github:` backend installs** (i.e., the literal path under `~/.local/share/mise/installs/...` for a `github:owner/repo`-declared tool) wasn't confirmed against mise's docs text directly — the docs describe the general `installs/<tool>/<version>` shape but I did not find a worked example specifically for a `github:`-namespaced tool id. Low-risk because the whole point of using the shim path is that this detail becomes irrelevant to the service definition, but worth a quick `mise install` dry run against a test repo to confirm before writing user-facing docs that mention the raw install path.
- **`$HOME`/environment guarantees for launchd/systemd-launched processes** are stated above based on general, well-established systemd/launchd session-bootstrap behavior, but I did not find one single authoritative doc page that explicitly enumerates "these env vars are guaranteed present for a per-user agent/unit." If the Collector's install code wants to be maximally defensive, it could set `Environment=HOME=%h` / an explicit `<key>EnvironmentVariables</key>` block rather than relying on inherited environment.
- **mise's own docs did not explicitly narrate "shims are repointed on upgrade" as an explicit sentence** — this is inferred correctly from how the shim mechanism works (dynamic resolution per invocation rather than a static link target), corroborated by the mechanism description in the docs, but there isn't a single quotable sentence that says "upgrades repoint shims automatically" in so many words.
- **Whether `mise` needs to be present/initialized in a minimal, non-interactive `--user` service environment** (e.g., does the mise binary itself need any lazy first-run setup, like creating `~/.config/mise/config.toml`, that normally happens on first interactive shell use?) wasn't directly verified against mise's docs for this specific non-interactive-invocation scenario. Worth a manual smoke test: install the collector via mise on a fresh account, then invoke the shim path directly via `systemd-run --user --pty <shim-path> run` (or an equivalent manual launchd load) before shipping this as the documented approach.
- The **`aqua:` backend's actual level of effort** to add a brand-new package was inferred from the docs' framing (fixing *existing* entries) rather than from an explicit "how to add a new package" walkthrough in the pages fetched; if this path is ever reconsidered, check aqua's own registry contribution docs (out of scope for this pass, since `github:` already avoids the need).

---

## Sources

- [mise docs: GitHub backend](https://mise.jdx.dev/dev-tools/backends/github.html)
- [mise docs: UBI backend (deprecated)](https://mise.jdx.dev/dev-tools/backends/ubi.html)
- [mise docs: Aqua backend](https://mise.jdx.dev/dev-tools/backends/aqua.html)
- [mise docs: Shims](https://mise.jdx.dev/dev-tools/shims.html)
- [mise docs: Directories](https://mise.jdx.dev/directories.html)
- [mise docs: `mise upgrade` CLI reference](https://mise.jdx.dev/cli/upgrade.html)
- [jdx/mise GitHub discussion #7605 — shims are symlinks to the mise binary](https://github.com/jdx/mise/discussions/7605) (corroborating, lower-confidence secondary discussion — used only to confirm the official docs' own description of the mechanism)
- [houseabsolute/ubi — README, asset-matching algorithm](https://github.com/houseabsolute/ubi)
- Apple `launchd.plist(5)` man page (`man launchd.plist` on macOS — Apple/Darwin primary source, no stable public URL; mirrored conceptually at [Apple Developer: Creating Launchd Jobs](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html))
- Apple `launchctl(1)` man page (`man launchctl` on macOS)
- [systemd.service(5)](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html) (fetched via Wayback Machine snapshot due to live-site bot blocking; content is systemd upstream's own man page)
- [systemd.unit(5)](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html) (same access note)
- [loginctl(1)](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html) (same access note)
- [kardianos/service](https://github.com/kardianos/service) — README plus direct source read of `service.go`, `service_go1.8.go`, `service_darwin.go`, `service_systemd_linux.go`, `service_unix.go`, `service_linux.go`
