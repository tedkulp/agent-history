// Package control is the Collector's control socket (collector.md §2.4):
// HTTP/1.1 over a Unix socket in the state directory, through which
// `status`, `sync` and `set-name` reach the running service. It is internal
// to one binary version, not a public API.
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/status"
)

// SocketName is the control socket's file name in the state directory.
const SocketName = "control.sock"

// ErrNotRunning means no Collector is listening on the socket.
var ErrNotRunning = errors.New("service not running")

// Backend is what the socket exposes: the running Collector.
type Backend interface {
	Status(ctx context.Context) (status.Report, error)
	// Sync runs a full reconcile, reporting progress lines as it goes, and
	// returns a one-line summary.
	Sync(ctx context.Context, progress func(string)) (string, error)
	// SetName sends the new display name to the Hub.
	SetName(ctx context.Context, name string) error
}

// Listen opens the socket at path with mode 0600, replacing a stale one. The
// caller must hold collector.lock, so no other Collector is listening there.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve answers requests on ln until ctx is cancelled, then closes it.
func Serve(ctx context.Context, ln net.Listener, b Backend) error {
	srv := &http.Server{Handler: Handler(b), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// A sync in progress is cut off: the Collector is shutting down.
		srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Lines of a POST /sync response start with one of these.
const (
	linePrefixProgress = "progress "
	linePrefixDone     = "done "
	linePrefixError    = "error "
)

type nameBody struct {
	DisplayName string `json:"display_name"`
}

// Handler serves the socket's endpoints.
func Handler(b Backend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		rep, err := b.Status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rep)
	})
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rc := http.NewResponseController(w)
		line := func(prefix, s string) {
			// One line per message, whatever the message holds.
			io.WriteString(w, prefix+strings.ReplaceAll(s, "\n", " ")+"\n")
			rc.Flush()
		}
		summary, err := b.Sync(r.Context(), func(s string) { line(linePrefixProgress, s) })
		if err != nil {
			line(linePrefixError, err.Error())
			return
		}
		line(linePrefixDone, summary)
	})
	mux.HandleFunc("PUT /name", func(w http.ResponseWriter, r *http.Request) {
		var body nameBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.DisplayName == "" {
			http.Error(w, "display_name is required", http.StatusBadRequest)
			return
		}
		if err := b.SetName(r.Context(), body.DisplayName); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Client talks to the Collector listening on one socket.
type Client struct {
	http *http.Client
}

// NewClient returns a Client for the socket at path. Nothing is dialled
// until a request is made.
func NewClient(path string) *Client {
	var d net.Dialer
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := d.DialContext(ctx, "unix", path)
			if err != nil {
				// No socket, or a stale one nobody listens on.
				return nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
			}
			return c, nil
		},
	}}}
}

// The host is ignored: every request goes to the socket.
const baseURL = "http://collector"

// Status fetches the running Collector's report.
func (c *Client) Status(ctx context.Context) (status.Report, error) {
	var rep status.Report
	resp, err := c.do(ctx, http.MethodGet, "/status", nil)
	if err != nil {
		return rep, err
	}
	defer resp.Body.Close()
	return rep, json.NewDecoder(resp.Body).Decode(&rep)
}

// Sync asks the running Collector for a full reconcile, calling progress
// for each progress line, and returns its summary.
func (c *Client) Sync(ctx context.Context, progress func(string)) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/sync", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, linePrefixProgress):
			progress(strings.TrimPrefix(l, linePrefixProgress))
		case strings.HasPrefix(l, linePrefixDone):
			return strings.TrimPrefix(l, linePrefixDone), nil
		case strings.HasPrefix(l, linePrefixError):
			return "", errors.New(strings.TrimPrefix(l, linePrefixError))
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("the service stopped before the sync finished")
}

// SetName asks the running Collector to send a new display name to the Hub.
func (c *Client) SetName(ctx context.Context, name string) error {
	b, err := json.Marshal(nameBody{name})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, "/name", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// do sends a request and turns a non-2xx response into an error holding its body.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrNotRunning) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, errors.New(strings.TrimSpace(string(msg)))
	}
	return resp, nil
}
