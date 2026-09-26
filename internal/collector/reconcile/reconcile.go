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
}

// Source is one Source's discovered Raw records.
type Source struct {
	ID      string
	Records []source.Record
}

// Result counts what a reconcile did, per record.
type Result struct {
	Uploaded  int   // records that had bytes appended
	Unchanged int   // records the Hub already had
	Deferred  int   // records needing a replace, which this Collector can't send yet
	Failed    int   // records that hit an error
	Bytes     int64 // decompressed bytes sent
}

// Reconcile registers the Machine, fetches the manifest and ships every
// record the Hub is missing or holds only a prefix of. Per-record errors are
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
			sent, outcome, err := ship(ctx, hub, src.ID, rec, h)
			res.Bytes += sent
			rlog := log.With("source", src.ID, "key", rec.Key)
			switch {
			case err != nil:
				res.Failed++
				rlog.Error("shipping record", "err", err)
			case outcome == doNothing:
				res.Unchanged++
			case outcome == doReplace:
				res.Deferred++
				rlog.Warn("record was rewritten on disk; replace is not supported yet, skipping")
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

// ship brings one record up to date and returns the bytes sent and what was
// decided. A 409 is re-decided once against the state it carries.
func ship(ctx context.Context, hub Hub, sourceID string, rec source.Record, h *protocol.ManifestRecord) (int64, actionKind, error) {
	L, err := content(rec)
	if err != nil {
		return 0, 0, err
	}
	var sent int64
	for attempt := 0; ; attempt++ {
		a := decide(h, L)
		if a.kind != doAppend {
			return sent, a.kind, nil
		}
		st, err := hub.Append(ctx, sourceID, rec.Key, a.offset, hexSum(L[:a.offset]), L[a.offset:])
		var conflict *hubclient.ConflictError
		if errors.As(err, &conflict) && attempt == 0 {
			h = &protocol.ManifestRecord{Length: conflict.Length, Sha256: conflict.Sha256}
			continue
		}
		if err != nil {
			return sent, doAppend, err
		}
		sent += int64(len(L)) - a.offset
		if st.Length != int64(len(L)) || st.Sha256 != hexSum(L) {
			return sent, doAppend, fmt.Errorf("hub acked length %d sha256 %s, expected length %d", st.Length, st.Sha256, len(L))
		}
		return sent, doAppend, nil
	}
}

// content reads a record's local content L. JSONL content is cut at the last
// complete line, so a half-written line is never sent (protocol.md §3.2).
func content(rec source.Record) ([]byte, error) {
	b, err := os.ReadFile(rec.Path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(rec.Key, ".jsonl") {
		b = b[:bytes.LastIndexByte(b, '\n')+1]
	}
	return b, nil
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
