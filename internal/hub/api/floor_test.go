package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

func newFloorServer(t *testing.T, floor Floor, log *slog.Logger) *httptest.Server {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(New(s, log, floor))
	t.Cleanup(srv.Close)
	return srv
}

// request sends method to path under the Machine with a Collector
// User-Agent at version, or none when version is empty.
func request(t *testing.T, srv *httptest.Server, method, path, version string) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"home_dir":"/home/ted"}`)
	req, _ := http.NewRequest(method, srv.URL+"/api/v1/machines/"+machine+path, body)
	req.Header.Set(protocol.HeaderMachineID, machine)
	if version != "" {
		req.Header.Set("User-Agent", protocol.UserAgentProduct+"/"+version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestFloorRejectsOldCollectorOnEveryEndpoint(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv := newFloorServer(t, Floor{HubVersion: "0.4.0", Min: "0.4.0"}, log)
	for _, ep := range []struct{ method, path string }{
		{http.MethodPut, ""},
		{http.MethodGet, "/manifest"},
		{http.MethodPost, "/records"},
		{http.MethodGet, "/no-such-endpoint"},
	} {
		resp := request(t, srv, ep.method, ep.path, "0.3.9")
		if resp.StatusCode != http.StatusUpgradeRequired {
			t.Fatalf("%s %s: status %d, want 426", ep.method, ep.path, resp.StatusCode)
		}
		got := decode[protocol.TooOld](t, resp)
		if got.Error != protocol.ErrCollectorTooOld || got.MinCollectorVersion != "0.4.0" {
			t.Fatalf("%s %s: body %+v", ep.method, ep.path, got)
		}
	}
	if n := strings.Count(logs.String(), "level=INFO msg=\"collector too old\""); n != 4 {
		t.Fatalf("logged %d 426s at info, want 4:\n%s", n, logs.String())
	}
}

func TestFloor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		floor     Floor
		collector string // "" sends no Collector User-Agent
		want      int
	}{
		{"at the floor", Floor{"0.4.0", "0.4.0"}, "0.4.0", http.StatusNoContent},
		{"above the floor", Floor{"0.5.0", "0.4.0"}, "0.4.2", http.StatusNoContent},
		{"below the floor", Floor{"0.4.0", "0.4.0"}, "0.3.9", http.StatusUpgradeRequired},
		{"rc Collector against its release's floor", Floor{"0.4.0-rc.2", "0.4.0"}, "0.4.0-rc.1", http.StatusNoContent},
		{"build suffix", Floor{"0.4.0", "0.4.0+hub"}, "0.4.0+abc", http.StatusNoContent},
		{"unparsable version", Floor{"0.4.0", "0.1.0"}, "latest", http.StatusUpgradeRequired},
		{"no Collector User-Agent", Floor{"0.4.0", "0.1.0"}, "", http.StatusUpgradeRequired},
		{"dev Collector, release Hub", Floor{"0.4.0", "0.1.0"}, protocol.DevVersion, http.StatusUpgradeRequired},
		{"dev Collector, dev Hub", Floor{protocol.DevVersion, "0.1.0"}, protocol.DevVersion, http.StatusNoContent},
		{"old Collector, dev Hub", Floor{protocol.DevVersion, "0.4.0"}, "0.1.0", http.StatusNoContent},
		{"zero Floor skips the check", Floor{}, "", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFloorServer(t, tc.floor, slog.New(slog.DiscardHandler))
			if got := request(t, srv, http.MethodPut, "", tc.collector).StatusCode; got != tc.want {
				t.Fatalf("status %d, want %d", got, tc.want)
			}
		})
	}
}
