package protocol

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DevVersion is the version of a binary built without the release ldflags
// (protocol.md §4.6).
const DevVersion = "0.0.0-dev"

// semverRe is a semantic version without a leading "v" (semver.org §2, §9, §10).
var semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// ValidVersion reports whether v is a semantic version without a "v".
func ValidVersion(v string) bool { return semverRe.MatchString(v) }

// core is v's MAJOR.MINOR.PATCH; v must be valid.
func core(v string) [3]int {
	m := semverRe.FindStringSubmatch(v)
	var c [3]int
	for i := range c {
		c[i], _ = strconv.Atoi(m[i+1])
	}
	return c
}

// VersionLess reports whether a is below b once pre-release and build
// suffixes are dropped from both: 0.4.0-rc.1 counts as 0.4.0. Both must be
// valid (ValidVersion).
func VersionLess(a, b string) bool {
	ca, cb := core(a), core(b)
	for i := range ca {
		if ca[i] != cb[i] {
			return ca[i] < cb[i]
		}
	}
	return false
}

// EffectiveMinCollectorVersion is the higher of MinCollectorVersion and env,
// the AGENT_HISTORY_MIN_COLLECTOR_VERSION value, which can only raise it. An
// empty env means unset.
func EffectiveMinCollectorVersion(env string) (string, error) {
	if env == "" {
		return MinCollectorVersion, nil
	}
	if !ValidVersion(env) {
		return "", fmt.Errorf("minimum Collector version %q is not a semantic version like 0.4.0", env)
	}
	if VersionLess(MinCollectorVersion, env) {
		return env, nil
	}
	return MinCollectorVersion, nil
}

// CollectorVersion is the version in a Collector User-Agent
// "agent-history-collector/<version>", or "" when ua isn't one.
func CollectorVersion(ua string) string {
	product, _, _ := strings.Cut(ua, " ")
	name, v, ok := strings.Cut(product, "/")
	if !ok || name != UserAgentProduct {
		return ""
	}
	return v
}
