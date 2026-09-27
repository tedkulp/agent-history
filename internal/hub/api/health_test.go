package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/worker"
	"github.com/tedkulp/agent-history/protocol"
)

func getHealth(t *testing.T, srv *httptest.Server, id string) protocol.Health {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/machines/"+id+"/health", nil)
	req.Header.Set(protocol.HeaderMachineID, id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	return decode[protocol.Health](t, resp)
}

func TestHealthCountsUnknownClaudeEntryType(t *testing.T) {
	ctx := context.Background()
	reg := parser.NewRegistry(claudecode.New())
	s, err := store.Open(ctx, t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(New(s, nil, Floor{}))
	t.Cleanup(srv.Close)

	if got := getHealth(t, srv, machine); got != (protocol.Health{}) {
		t.Errorf("empty health = %+v", got)
	}

	body := []byte(`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/x","message":{"role":"user","content":"hello"}}` + "\n" +
		`{"type":"brand-new-thing","uuid":"z1","parentUuid":"u1","timestamp":"2026-09-01T10:00:01.000Z"}` + "\n")
	resp := postRecord(t, srv, recordReq{key: "-x/3d34bfcc-90e7-4fd0-900f-86c047b22433.jsonl", body: body})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest status %d", resp.StatusCode)
	}
	if worked, _, err := worker.New(s, reg, nil).RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if got, want := getHealth(t, srv, machine), (protocol.Health{SessionsWithWarnings: 1}); got != want {
		t.Errorf("health = %+v, want %+v", got, want)
	}
}

func TestHealthRequiresMatchingMachineHeader(t *testing.T) {
	srv := newServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/machines/"+machine+"/health", nil)
	req.Header.Set(protocol.HeaderMachineID, "someone-else")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}
