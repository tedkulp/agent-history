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
		feed, err := s.Feed(ctx, store.FeedFilter{}, 10)
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

func TestFirstParsePanicListsSessionAsFailed(t *testing.T) {
	ctx := context.Background()
	reg := parser.NewRegistry(panicky{claudecode.New()})
	s := setup(t, reg)
	w := New(s, reg, nil)
	worked, _, err := w.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	if worked, _, _ := w.RunOnce(ctx); worked {
		t.Error("failed job stayed in the queue")
	}
	feed, _ := s.Feed(ctx, store.FeedFilter{Warnings: true}, 10)
	if len(feed) != 1 || !feed[0].Failed || feed[0].Title != "" {
		t.Fatalf("has-warnings feed = %+v, want the failed Session", feed)
	}
	if h, _ := s.Health(ctx, "m1"); h.SessionsFailed != 1 {
		t.Errorf("sessions_failed = %d", h.SessionsFailed)
	}
	h, msgs, ok, err := s.Transcript(ctx, feed[0].ID)
	if err != nil || !ok || h.Parsed || h.ParseError != "parser panic: boom" || len(msgs) != 0 {
		t.Errorf("Transcript = %+v, %d msgs, ok=%v, err=%v", h, len(msgs), ok, err)
	}
}

func TestParserPanicKeepsPreviousTranscriptAndIngest(t *testing.T) {
	ctx := context.Background()
	reg := parser.NewRegistry(claudecode.New())
	s := setup(t, reg)
	w := New(s, reg, nil)
	if worked, _, err := w.RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}

	// The parser starts panicking. Ingest is still acked.
	reg[protocol.SourceClaudeCode] = panicky{claudecode.New()}
	cur, err := s.CurrentContent(ctx, "m1", protocol.SourceClaudeCode, mainKey)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"type":"user","uuid":"u2","parentUuid":"u1","timestamp":"2026-09-01T10:01:00.000Z","message":{"role":"user","content":"again"}}` + "\n")
	enc, _ := zstd.NewWriter(nil)
	sum := sha256.Sum256(cur)
	if _, err := s.Append(ctx, store.AppendRequest{
		MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: mainKey, Offset: int64(len(cur)),
		PrefixSha256: hex.EncodeToString(sum[:]), Data: data, Compressed: enc.EncodeAll(data, nil),
	}); err != nil {
		t.Fatalf("ingest after a failing parser: %v", err)
	}
	s.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	if worked, _, err := w.RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}

	feed, _ := s.Feed(ctx, store.FeedFilter{}, 10)
	if len(feed) != 1 || !feed[0].Failed || feed[0].Title != "hello" {
		t.Fatalf("feed = %+v", feed)
	}
	h, msgs, ok, _ := s.Transcript(ctx, feed[0].ID)
	if !ok || !h.Parsed || h.ParseError == "" || len(msgs) != 1 {
		t.Errorf("Transcript: parsed=%v error=%q msgs=%d ok=%v; want the last good one", h.Parsed, h.ParseError, len(msgs), ok)
	}
}
