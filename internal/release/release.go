// Package release holds the checks the tag workflow runs before it publishes
// (README.md §5.4).
package release

import (
	"fmt"
	"strings"

	"github.com/tedkulp/agent-history/protocol"
)

// CheckFloor reports whether a release tagged tag (like v0.4.0) may ship
// with the minimum Collector version floor.
func CheckFloor(floor, tag string) error {
	v, ok := strings.CutPrefix(tag, "v")
	if !ok || !protocol.ValidVersion(v) {
		return fmt.Errorf("tag %q is not a version tag like v0.4.0", tag)
	}
	if protocol.VersionLess(v, floor) {
		return fmt.Errorf("MinCollectorVersion %s is above the release version %s", floor, v)
	}
	return nil
}
