package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")

	if err := healthcheck([]string{"--listen", listen}); err != nil {
		t.Errorf("healthy hub: %v", err)
	}
	t.Setenv("AGENT_HISTORY_LISTEN", listen)
	if err := healthcheck(nil); err != nil {
		t.Errorf("port from env: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := healthcheck(nil); err == nil {
		t.Error("503: no error")
	}
}

func TestHealthcheckHubDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if err := healthcheck([]string{"--listen", addr}); err == nil {
		t.Error("no hub listening: no error")
	}
}
