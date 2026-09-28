package source

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Compressed records are read decompressed (collector.md §2.6).
func TestReadFileDecompresses(t *testing.T) {
	const body = "{\"type\":\"session\"}\n"
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(body))
	w.Close()

	dir := t.TempDir()
	for name, content := range map[string][]byte{
		"a.jsonl":     []byte(body),
		"a.jsonl.zst": enc.EncodeAll([]byte(body), nil),
		"a.jsonl.gz":  gz.Bytes(),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ReadFile(p)
		if err != nil || string(got) != body {
			t.Errorf("ReadFile(%s) = %q, %v", name, got, err)
		}
	}
}
