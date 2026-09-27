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

// Notes returns the CHANGELOG.md section for tag (like v0.4.0), without its
// heading: the GitHub Release notes. It is an error when the section is
// missing or empty, so a release can't go out without its changes listed.
func Notes(changelog []byte, tag string) (string, error) {
	v, ok := strings.CutPrefix(tag, "v")
	if !ok || !protocol.ValidVersion(v) {
		return "", fmt.Errorf("tag %q is not a version tag like v0.4.0", tag)
	}
	var (
		body  []string
		found bool
	)
	for _, l := range strings.Split(string(changelog), "\n") {
		if strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "[") && strings.Contains(l, "]: ") {
			if found {
				break
			}
			found = l == "## ["+v+"]" || strings.HasPrefix(l, "## ["+v+"] ")
			continue
		}
		if found {
			body = append(body, l)
		}
	}
	notes := strings.TrimSpace(strings.Join(body, "\n"))
	switch {
	case !found:
		return "", fmt.Errorf("CHANGELOG.md has no section for %s; move the Unreleased entries under ## [%s] - <date>", v, v)
	case notes == "":
		return "", fmt.Errorf("CHANGELOG.md section %s is empty", v)
	}
	return notes + "\n", nil
}
