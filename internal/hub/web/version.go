package web

import (
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/protocol"
)

// versionLabel is the header's build label: "dev" for a dev build, else
// "v<Version> · <date>" with the full BuildTime as its tooltip, or just
// "v<Version>" when BuildTime is missing or not RFC 3339.
func versionLabel() (label, title string) {
	v := buildinfo.Version
	if v == protocol.DevVersion {
		return "dev", ""
	}
	t, err := time.Parse(time.RFC3339, buildinfo.BuildTime)
	if err != nil {
		return "v" + v, ""
	}
	return "v" + v + " · " + t.Format(time.DateOnly), buildinfo.BuildTime
}
