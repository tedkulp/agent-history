// Package buildinfo holds the version stamped in by the release ldflags.
package buildinfo

import "github.com/tedkulp/agent-history/protocol"

// Version is set with -ldflags "-X github.com/tedkulp/agent-history/internal/buildinfo.Version=0.3.1".
var Version = protocol.DevVersion
