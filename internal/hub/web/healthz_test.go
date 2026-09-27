package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

func getHealthz(t *testing.T, srv *httptest.Server) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestHealthz(t *testing.T) {
	s, err := store.Open(context.Background(), t.TempDir(), parser.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(s, nil))
	t.Cleanup(srv.Close)

	if code, body := getHealthz(t, srv); code != http.StatusOK || body != "ok\n" {
		t.Errorf("healthy: %d %q, want 200 \"ok\\n\"", code, body)
	}
	s.Close()
	if code, _ := getHealthz(t, srv); code != http.StatusServiceUnavailable {
		t.Errorf("closed database: %d, want 503", code)
	}
}
