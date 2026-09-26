package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/protocol"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeAll(b, nil)
}

func appendChunk(t *testing.T, s *Store, machine, key string, offset int64, prefix, data []byte) (protocol.RecordState, error) {
	t.Helper()
	return s.Append(context.Background(), AppendRequest{
		MachineID:    machine,
		Source:       protocol.SourceClaudeCode,
		RecordKey:    key,
		Offset:       offset,
		PrefixSha256: hexSum(prefix),
		Data:         data,
		Compressed:   zstdBytes(t, data),
	})
}

func TestAppendBuildsCurrentVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	first := []byte("{\"a\":1}\n")
	second := []byte("{\"b\":2}\n")

	st, err := appendChunk(t, s, "m1", "p/s.jsonl", 0, nil, first)
	if err != nil {
		t.Fatal(err)
	}
	if st.Length != int64(len(first)) || st.Sha256 != hexSum(first) || st.Version != 1 {
		t.Fatalf("after first append: %+v", st)
	}

	st, err = appendChunk(t, s, "m1", "p/s.jsonl", int64(len(first)), first, second)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]byte{}, first...), second...)
	if st.Length != int64(len(all)) || st.Sha256 != hexSum(all) {
		t.Fatalf("after second append: %+v", st)
	}

	got, err := s.CurrentContent(ctx, "m1", protocol.SourceClaudeCode, "p/s.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(all) {
		t.Fatalf("content = %q, want %q", got, all)
	}
}

func TestAppendConflicts(t *testing.T) {
	s := openTest(t)
	data := []byte("line\n")
	if _, err := appendChunk(t, s, "m1", "k.jsonl", 0, nil, data); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		offset int64
		prefix []byte
	}{
		{"wrong offset", 2, data},
		{"wrong prefix", int64(len(data)), []byte("LINE\n")},
		{"offset zero on existing", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := appendChunk(t, s, "m1", "k.jsonl", tc.offset, tc.prefix, []byte("more\n"))
			var conflict *ConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("err = %v, want ConflictError", err)
			}
			if conflict.Length != int64(len(data)) || conflict.Sha256 != hexSum(data) {
				t.Fatalf("conflict state = %+v", conflict)
			}
		})
	}

	// A conflicting append must not change what is stored.
	got, err := s.CurrentContent(context.Background(), "m1", protocol.SourceClaudeCode, "k.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("content changed to %q", got)
	}
}

func TestAppendToUnknownRecordAtNonZeroOffsetConflicts(t *testing.T) {
	s := openTest(t)
	_, err := appendChunk(t, s, "m1", "k.jsonl", 5, []byte("12345"), []byte("x\n"))
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if conflict.Length != 0 || conflict.Sha256 != protocol.EmptySha256 {
		t.Fatalf("conflict state = %+v", conflict)
	}
}

func TestEmptyAppendRegistersEmptyRecord(t *testing.T) {
	s := openTest(t)
	st, err := appendChunk(t, s, "m1", "empty.jsonl", 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Length != 0 || st.Sha256 != protocol.EmptySha256 {
		t.Fatalf("state = %+v", st)
	}
	// Appending real data afterwards still works from offset 0.
	if _, err := appendChunk(t, s, "m1", "empty.jsonl", 0, nil, []byte("a\n")); err != nil {
		t.Fatal(err)
	}
}

func TestManifestIsPerMachine(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := []byte("a\n")
	b := []byte("bb\n")
	if _, err := appendChunk(t, s, "m1", "one.jsonl", 0, nil, a); err != nil {
		t.Fatal(err)
	}
	if _, err := appendChunk(t, s, "m2", "two.jsonl", 0, nil, b); err != nil {
		t.Fatal(err)
	}

	m, err := s.Manifest(ctx, "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.ManifestRecord{{Source: protocol.SourceClaudeCode, RecordKey: "one.jsonl", Length: 2, Sha256: hexSum(a)}}
	if len(m) != 1 || m[0] != want[0] {
		t.Fatalf("manifest = %+v, want %+v", m, want)
	}

	m, err = s.Manifest(ctx, "m1", protocol.SourceCodex)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("source-filtered manifest = %+v", m)
	}
}

func TestUpsertMachine(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	info := protocol.MachineInfo{DisplayName: "laptop", Hostname: "h", OS: "darwin", Arch: "arm64", HomeDir: "/Users/x", CollectorVersion: "0.0.0-dev"}
	if err := s.UpsertMachine(ctx, "m1", info); err != nil {
		t.Fatal(err)
	}
	info.DisplayName = "renamed"
	if err := s.UpsertMachine(ctx, "m1", info); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := s.read.QueryRowContext(ctx, `SELECT display_name FROM machines WHERE id = ?`, "m1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "renamed" {
		t.Fatalf("display_name = %q", name)
	}
}

func TestReopenKeepsDataAndSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendChunk(t, s, "m1", "k.jsonl", 0, nil, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.Manifest(ctx, "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("manifest after reopen = %+v", m)
	}
	var v int
	if err := s.read.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(mustMigrations(t)) {
		t.Fatalf("user_version = %d", v)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.ExecContext(ctx, `PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, dir, nil); err == nil {
		t.Fatal("Open succeeded on a newer schema")
	}
}

func mustMigrations(t *testing.T) []migration {
	t.Helper()
	m, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	return m
}
