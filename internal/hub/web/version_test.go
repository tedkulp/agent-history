package web

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/protocol"
)

// headerOf fetches path and returns the page's top header.
func headerOf(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	start := strings.Index(page, `<header class="top">`)
	if start < 0 {
		t.Fatalf("%s: no top header in %q", url, page)
	}
	return page[start : start+strings.Index(page[start:], "</header>")]
}

func stampBuild(t *testing.T, version, buildTime string) {
	t.Helper()
	v, bt := buildinfo.Version, buildinfo.BuildTime
	buildinfo.Version, buildinfo.BuildTime = version, buildTime
	t.Cleanup(func() { buildinfo.Version, buildinfo.BuildTime = v, bt })
}

func TestHeaderShowsTheVersion(t *testing.T) {
	srv := usageSite(t)
	for _, tc := range []struct {
		name, version, buildTime, want string
	}{
		{"dev", protocol.DevVersion, "", `<span class="version">dev</span>`},
		{"dev ignores a build time", protocol.DevVersion, "2026-10-16T14:02:00Z", `<span class="version">dev</span>`},
		{"release", "0.12.1", "2026-10-16T14:02:00Z", `<span class="version" title="2026-10-16T14:02:00Z">v0.12.1 · 2026-10-16</span>`},
		{"release without a build time", "0.12.1", "", `<span class="version">v0.12.1</span>`},
		{"release with a bad build time", "0.12.1", "yesterday", `<span class="version">v0.12.1</span>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stampBuild(t, tc.version, tc.buildTime)
			for _, path := range []string{"/", "/usage"} {
				h := headerOf(t, srv.URL+path)
				if !strings.HasSuffix(h, tc.want) {
					t.Errorf("%s header %q, want it to end with %q", path, h, tc.want)
				}
			}
		})
	}
}
