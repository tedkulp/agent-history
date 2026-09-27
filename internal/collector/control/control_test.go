package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tedkulp/agent-history/internal/collector/status"
)

type fakeBackend struct {
	report  status.Report
	lines   []string
	summary string
	syncErr error
	nameErr error
	names   []string
}

func (f *fakeBackend) Status(context.Context) (status.Report, error) { return f.report, nil }

func (f *fakeBackend) Sync(_ context.Context, progress func(string)) (string, error) {
	for _, l := range f.lines {
		progress(l)
	}
	return f.summary, f.syncErr
}

func (f *fakeBackend) SetName(_ context.Context, name string) error {
	f.names = append(f.names, name)
	return f.nameErr
}

// socketPath is short enough for a Unix socket: t.TempDir can exceed the
// 104-byte limit on macOS.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ahctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, SocketName)
}

func serve(t *testing.T, s Service) (*Client, string) {
	t.Helper()
	path := socketPath(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, s) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return NewClient(path), path
}

func TestSocketIsPrivate(t *testing.T) {
	_, path := serve(t, &fakeBackend{})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want a 0600 socket", fi.Mode())
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

func TestStatus(t *testing.T) {
	n := 3
	want := status.Report{Version: "0.3.1", MachineID: "m", ServiceRunning: true, Pending: &n,
		Sources: []status.Source{{ID: "claude-code", Enabled: true, Layouts: []string{"jsonl"}, Records: 7}}}
	c, _ := serve(t, &fakeBackend{report: want})
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSyncStreamsProgress(t *testing.T) {
	c, _ := serve(t, &fakeBackend{lines: []string{"reconciling 2 records", "multi\nline"}, summary: "uploaded 2"})
	var got []string
	summary, err := c.Sync(context.Background(), func(s string) { got = append(got, s) })
	if err != nil {
		t.Fatal(err)
	}
	if summary != "uploaded 2" {
		t.Errorf("summary = %q", summary)
	}
	if want := []string{"reconciling 2 records", "multi line"}; !reflect.DeepEqual(got, want) {
		t.Errorf("progress = %q, want %q", got, want)
	}
}

func TestSyncError(t *testing.T) {
	c, _ := serve(t, &fakeBackend{syncErr: errors.New("hub unreachable")})
	_, err := c.Sync(context.Background(), func(string) {})
	if err == nil || err.Error() != "hub unreachable" {
		t.Errorf("err = %v", err)
	}
}

func TestSetName(t *testing.T) {
	b := &fakeBackend{}
	c, _ := serve(t, b)
	if err := c.SetName(context.Background(), "new-name"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.names, []string{"new-name"}) {
		t.Errorf("names = %q", b.names)
	}
	b.nameErr = errors.New("hub returned 500")
	if err := c.SetName(context.Background(), "x"); err == nil || err.Error() != "hub returned 500" {
		t.Errorf("err = %v", err)
	}
}

func TestNotRunning(t *testing.T) {
	path := socketPath(t)
	c := NewClient(path)
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("no socket: err = %v, want ErrNotRunning", err)
	}
	// A socket file left behind by a killed Collector.
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(interface{ SetUnlinkOnClose(bool) }).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := c.Sync(context.Background(), func(string) {}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stale socket: err = %v, want ErrNotRunning", err)
	}
}
