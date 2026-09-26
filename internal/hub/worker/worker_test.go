package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

const mainKey = "-Users-ted-src-app/3d34bfcc-90e7-4fd0-900f-86c047b22433.jsonl"

type panicky struct{ *claudecode.Parser }

func (panicky) Parse(parser.Input) (parser.Result, error) { panic("boom") }

func setup(t *testing.T, reg parser.Registry) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	data := []byte(`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/x","message":{"role":"user","content":"hello"}}` + "\n")
	enc, _ := zstd.NewWriter(nil)
	sum := sha256.Sum256(nil)
	if _, err := s.Append(context.Background(), store.AppendRequest{
		MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: mainKey,
		PrefixSha256: hex.EncodeToString(sum[:]), Data: data, Compressed: enc.EncodeAll(data, nil),
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunParsesIngestedSession(t *testing.T) {
	reg := parser.NewRegistry(claudecode.New())
	s := setup(t, reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { New(s, reg, nil).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		feed, err := s.Feed(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(feed) == 1 && feed[0].Title == "hello" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session never parsed")
}

func TestParserPanicMarksSessionFailed(t *testing.T) {
	reg := parser.NewRegistry(panicky{claudecode.New()})
	s := setup(t, reg)
	w := New(s, reg, nil)
	worked, _, err := w.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	if worked, _, _ := w.RunOnce(context.Background()); worked {
		t.Error("failed job stayed in the queue")
	}
	if feed, _ := s.Feed(context.Background(), 10); len(feed) != 0 {
		t.Errorf("never-parsed failed Session in the feed: %+v", feed)
	}
}
