package hubclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/api"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

func TestTransient(t *testing.T) {
	connRefused := &url.Error{Op: "Post", URL: "http://hub", Err: errors.New("connection refused")}
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{connRefused, true},
		{fmt.Errorf("wrapped: %w", connRefused), true},
		{&StatusError{StatusCode: 500}, true},
		{&StatusError{StatusCode: 503}, true},
		{&StatusError{StatusCode: 400}, false},
		{&StatusError{StatusCode: 426}, false},
		{&ConflictError{}, false},
		{&url.Error{Op: "Post", URL: "http://hub", Err: context.Canceled}, false},
		{errors.New("open: permission denied"), false},
	}
	for _, tc := range cases {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("Transient(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestPing(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(api.New(st, slog.New(slog.DiscardHandler)))
	c, err := New(srv.URL, "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33", "0.0.0-dev", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	srv.Close()
	if err := c.Ping(context.Background()); !Transient(err) {
		t.Fatalf("down: err = %v, want a transient error", err)
	}
}
