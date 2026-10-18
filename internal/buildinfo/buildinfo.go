// Package buildinfo holds the version stamped in by the release ldflags.
package buildinfo

import "github.com/tedkulp/agent-history/protocol"

// Version is set with -ldflags "-X github.com/tedkulp/agent-history/internal/buildinfo.Version=0.3.1".
var Version = protocol.DevVersion

// BuildTime is the RFC 3339 commit time of a release build, set with
// -ldflags "-X github.com/tedkulp/agent-history/internal/buildinfo.BuildTime=2026-10-16T14:02:00Z".
// It is empty in dev builds.
var BuildTime string
