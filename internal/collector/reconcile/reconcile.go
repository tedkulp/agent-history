// Package reconcile brings the Hub in line with the Raw records on disk
// (protocol.md §4.1).
package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/protocol"
)

// Hub is the part of the Hub API a reconcile uses.
type Hub interface {
	PutMachine(ctx context.Context, info protocol.MachineInfo) error
	Manifest(ctx context.Context) ([]protocol.ManifestRecord, error)
	Append(ctx context.Context, source, key string, offset int64, prefixSha256 string, data []byte) (protocol.RecordState, error)
	Replace(ctx context.Context, source, key string, data []byte) (protocol.RecordState, error)
}

// Source is one Source's discovered Raw records.
type Source struct {
	ID      string
	Records []source.Record
}

// Result counts what a reconcile did, per record.
type Result struct {
	Uploaded  int   // records that had bytes appended
	Replaced  int   // records sent as a new version
	Unchanged int   // records the Hub already had
	Failed    int   // records that hit an error; they are skipped until the next reconcile
	Bytes     int64 // decompressed bytes sent
}

// Reconcile registers the Machine, fetches the manifest and ships every
// record the Hub is missing, holds only a prefix of, or holds a different
// version of. Per-record errors are
// logged and counted in Result; the returned error is for failures that stop
// the whole reconcile.
func Reconcile(ctx context.Context, hub Hub, info protocol.MachineInfo, sources []Source, log *slog.Logger) (Result, error) {
	if log == nil {
		log = slog.Default()
	}
	var res Result
	if err := hub.PutMachine(ctx, info); err != nil {
		return res, fmt.Errorf("registering machine: %w", err)
	}
	manifest, err := hub.Manifest(ctx)
	if err != nil {
		return res, fmt.Errorf("fetching manifest: %w", err)
	}
	onHub := make(map[string]protocol.ManifestRecord, len(manifest))
	for _, m := range manifest {
		onHub[m.Source+"\x00"+m.RecordKey] = m
	}

	for _, src := range sources {
		for _, rec := range src.Records {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			var h *protocol.ManifestRecord
			if m, ok := onHub[src.ID+"\x00"+rec.Key]; ok {
				h = &m
			}
			rlog := log.With("source", src.ID, "key", rec.Key)
			sent, outcome, err := ship(ctx, hub, src.ID, rec, h, rlog)
			res.Bytes += sent
			switch {
			case err != nil:
				res.Failed++
				rlog.Error("shipping record", "err", err)
			case outcome == doNothing:
				res.Unchanged++
			case outcome == doReplace:
				res.Replaced++
				rlog.Debug("replaced record", "bytes", sent)
			default:
				res.Uploaded++
				rlog.Debug("shipped record", "bytes", sent)
			}
		}
	}
	return res, nil
}

type actionKind int

const (
	doNothing actionKind = iota
	doAppend
	doReplace
)

type action struct {
	kind   actionKind
	offset int64
}

// decide applies the protocol.md §4.1 table to the Hub's entry h (nil when
// the Hub doesn't list the record) and the local content L.
func decide(h *protocol.ManifestRecord, L []byte) action {
	switch {
	case h == nil:
		return action{kind: doAppend}
	case h.Length == int64(len(L)) && h.Sha256 == hexSum(L):
		return action{kind: doNothing}
	case h.Length < int64(len(L)) && hexSum(L[:h.Length]) == h.Sha256:
		return action{kind: doAppend, offset: h.Length}
	default:
		return action{kind: doReplace}
	}
}

// maxConflicts bounds how often one record is re-decided after a 409.
const maxConflicts = 3

// MismatchError is a 200 whose state differs from the local hash of the same
// prefix (protocol.md §4.4). The Hub's manifest then lists that state, so the
// next reconcile decides on a replace.
type MismatchError struct {
	HubLength, LocalLength int64
	HubSha256, LocalSha256 string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("hub acked length %d sha256 %s, local content is length %d sha256 %s; the record will be replaced on the next reconcile",
		e.HubLength, e.HubSha256, e.LocalLength, e.LocalSha256)
}

// ship brings one record up to date and returns the bytes sent and the first
// decision. A 409 is re-decided against the state it carries (protocol.md
// §4.4); any other error skips the record until the next reconcile.
func ship(ctx context.Context, hub Hub, sourceID string, rec source.Record, h *protocol.ManifestRecord, log *slog.Logger) (int64, actionKind, error) {
	L, err := content(rec)
	if err != nil {
		return 0, doNothing, err
	}
	jsonl := isJSONL(rec.Key)
	if jsonl {
		var cut bool
		if L, cut = cutOversizedLine(L, protocol.MaxBodyBytes); cut {
			log.Warn("a line is over the 64 MiB body limit; shipping the record only up to it", "offset", len(L))
		}
	}
	first := decide(h, L).kind
	var sent int64
	for conflicts := 0; ; conflicts++ {
		a := decide(h, L)
		if a.kind == doNothing {
			return sent, first, nil
		}
		n, err := upload(ctx, hub, sourceID, rec.Key, L, jsonl, a)
		sent += n
		var conflict *hubclient.ConflictError
		if errors.As(err, &conflict) && conflicts < maxConflicts {
			h = &protocol.ManifestRecord{Length: conflict.Length, Sha256: conflict.Sha256}
			continue
		}
		return sent, first, err
	}
}

// upload sends L from action a in chunks (protocol.md §4.3): a replace
// carries the first chunk and appends carry the rest. After every 200 it
// checks the Hub's state against the local hash of the same prefix.
func upload(ctx context.Context, hub Hub, sourceID, key string, L []byte, jsonl bool, a action) (int64, error) {
	off := a.offset
	h := sha256.New()
	h.Write(L[:off])
	replace := a.kind == doReplace
	var sent int64
	for {
		chunk := L[off : off+int64(nextChunk(L[off:], jsonl, protocol.ChunkSize))]
		prefix := hex.EncodeToString(h.Sum(nil))
		var st protocol.RecordState
		err := retryUnsupported(func() (err error) {
			if replace {
				st, err = hub.Replace(ctx, sourceID, key, chunk)
			} else {
				st, err = hub.Append(ctx, sourceID, key, off, prefix, chunk)
			}
			return err
		})
		if err != nil {
			return sent, err
		}
		h.Write(chunk)
		off += int64(len(chunk))
		sent += int64(len(chunk))
		if sum := hex.EncodeToString(h.Sum(nil)); st.Length != off || st.Sha256 != sum {
			return sent, &MismatchError{HubLength: st.Length, HubSha256: st.Sha256, LocalLength: off, LocalSha256: sum}
		}
		if off == int64(len(L)) {
			return sent, nil
		}
		replace = false
	}
}

// retryUnsupported sends once more after a 415 (protocol.md §4.7).
func retryUnsupported(send func() error) error {
	err := send()
	var se *hubclient.StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusUnsupportedMediaType {
		err = send()
	}
	return err
}

// nextChunk returns the length of the next chunk of rest: at most size bytes,
// ending on a line boundary for JSONL content. A single JSONL line longer
// than size goes in a chunk of its own (protocol.md §4.3).
func nextChunk(rest []byte, jsonl bool, size int) int {
	if len(rest) <= size {
		return len(rest)
	}
	if !jsonl {
		return size
	}
	if i := bytes.LastIndexByte(rest[:size], '\n'); i >= 0 {
		return i + 1
	}
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		return i + 1
	}
	return len(rest)
}

// cutOversizedLine cuts JSONL content before its first line longer than max,
// which no request could carry (protocol.md §4.3).
func cutOversizedLine(L []byte, max int) ([]byte, bool) {
	for start := 0; start < len(L); {
		n := bytes.IndexByte(L[start:], '\n') + 1
		if n == 0 {
			n = len(L) - start
		}
		if n > max {
			return L[:start], true
		}
		start += n
	}
	return L, false
}

// content reads a record's local content L. JSONL content is cut at the last
// complete line, so a half-written line is never sent (protocol.md §3.2).
func content(rec source.Record) ([]byte, error) {
	b, err := os.ReadFile(rec.Path)
	if err != nil {
		return nil, err
	}
	if isJSONL(rec.Key) {
		b = b[:bytes.LastIndexByte(b, '\n')+1]
	}
	return b, nil
}

// isJSONL reports whether a record is JSONL content (protocol.md §3.2).
func isJSONL(key string) bool {
	return strings.HasSuffix(key, ".jsonl")
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
