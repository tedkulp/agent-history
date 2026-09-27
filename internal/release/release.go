// Package release holds the checks the tag workflow runs before it publishes
// (README.md §5.4).
package release

import (
	"fmt"
	"strings"

	"github.com/tedkulp/agent-history/protocol"
)

// CheckFloor returns an error when floor, the minimum Collector version, is
// above the version of tag (like v0.4.0), or when tag isn't a version tag.
func CheckFloor(floor, tag string) error {
	v, ok := strings.CutPrefix(tag, "v")
	if !ok || !protocol.ValidVersion(v) {
		return fmt.Errorf("tag %q is not a version tag like v0.4.0", tag)
	}
	if protocol.VersionLess(v, floor) {
		return fmt.Errorf("minimum Collector version %s is above the release version %s", floor, v)
	}
	return nil
}
