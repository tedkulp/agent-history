// Package api serves the Hub's ingestion endpoints under /api/v1
// (protocol.md §2.3).
package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

// maxCompressedBytes caps the request body read off the wire. zstd never
// expands incompressible input by more than a small frame overhead.
const maxCompressedBytes = protocol.MaxBodyBytes + 1<<20

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type server struct {
	store *store.Store
	log   *slog.Logger
	floor Floor
}

// Floor is the minimum Collector version check (protocol.md §4.6). A Floor
// without both fields skips the check, as a dev Hub does.
type Floor struct {
	// HubVersion is the Hub's own version. A dev Hub (protocol.DevVersion)
	// accepts every Collector.
	HubVersion string
	// Min is the effective minimum (protocol.EffectiveMinCollectorVersion).
	Min string
}

// allows reports whether a Collector sending userAgent may use the API.
func (f Floor) allows(userAgent string) bool {
	if f.HubVersion == "" || f.HubVersion == protocol.DevVersion || f.Min == "" {
		return true
	}
	v := protocol.CollectorVersion(userAgent)
	return v != protocol.DevVersion && protocol.ValidVersion(v) && !protocol.VersionLess(v, f.Min)
}

// New returns the /api/v1 handler. A nil logger means slog.Default().
func New(s *store.Store, log *slog.Logger, floor Floor) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	srv := &server{store: s, log: log, floor: floor}
	mux := http.NewServeMux()
	p := protocol.APIPrefix + "/machines/{id}"
	mux.HandleFunc("PUT "+p, srv.machineOnly(srv.putMachine))
	mux.HandleFunc("GET "+p+"/manifest", srv.machineOnly(srv.manifest))
	mux.HandleFunc("POST "+p+"/records", srv.machineOnly(srv.records))
	mux.HandleFunc("GET "+p+"/health", srv.machineOnly(srv.health))
	return srv.checkFloor(mux)
}

// checkFloor answers 426 to a Collector below the floor, on every request.
func (s *server) checkFloor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.UserAgent(); !s.floor.allows(ua) {
			s.log.Info("collector too old", "method", r.Method, "path", r.URL.Path, "user_agent", ua, "min_collector_version", s.floor.Min)
			writeJSON(w, http.StatusUpgradeRequired, protocol.TooOld{Error: protocol.ErrCollectorTooOld, MinCollectorVersion: s.floor.Min})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// machineOnly rejects requests whose X-Machine-Id differs from the path id.
func (s *server) machineOnly(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" || r.Header.Get(protocol.HeaderMachineID) != id {
			s.fail(w, r, http.StatusBadRequest, protocol.Error{Error: protocol.ErrBadRequest, Message: protocol.HeaderMachineID + " must equal the path id"})
			return
		}
		h(w, r, id)
	}
}

func (s *server) putMachine(w http.ResponseWriter, r *http.Request, id string) {
	var info protocol.MachineInfo
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&info); err != nil {
		s.fail(w, r, http.StatusBadRequest, protocol.Error{Error: protocol.ErrBadRequest, Message: "invalid JSON body"})
		return
	}
	if info.HomeDir == "" {
		s.fail(w, r, http.StatusBadRequest, protocol.Error{Error: protocol.ErrBadRequest, Message: "home_dir is required"})
		return
	}
	if err := s.store.UpsertMachine(r.Context(), id, info); err != nil {
		s.internal(w, r, err)
		return
	}
	s.log.Info("machine registered", "machine", id, "display_name", info.DisplayName, "collector_version", info.CollectorVersion)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) manifest(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.TouchMachine(r.Context(), id); err != nil {
		s.internal(w, r, err)
		return
	}
	records, err := s.store.Manifest(r.Context(), id, r.URL.Query().Get("source"))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Add("Vary", "Accept-Encoding")
	var out io.Writer = w
	switch accept := r.Header.Get("Accept-Encoding"); {
	case acceptsEncoding(accept, "zstd"):
		enc, err := zstd.NewWriter(w)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		w.Header().Set("Content-Encoding", "zstd")
		defer enc.Close()
		out = enc
	case acceptsEncoding(accept, "gzip"):
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		out = gz
	}
	if err := json.NewEncoder(out).Encode(protocol.Manifest{Records: records}); err != nil {
		s.log.Warn("writing manifest", "machine", id, "err", err)
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.TouchMachine(r.Context(), id); err != nil {
		s.internal(w, r, err)
		return
	}
	h, err := s.store.Health(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func acceptsEncoding(header, enc string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), enc) && strings.ReplaceAll(params, " ", "") != "q=0" {
			return true
		}
	}
	return false
}

func (s *server) records(w http.ResponseWriter, r *http.Request, id string) {
	bad := func(msg string) {
		s.fail(w, r, http.StatusBadRequest, protocol.Error{Error: protocol.ErrBadRequest, Message: msg})
	}
	source := r.Header.Get(protocol.HeaderSource)
	if source == "" {
		bad(protocol.HeaderSource + " is required")
		return
	}
	key, err := url.PathUnescape(r.Header.Get(protocol.HeaderRecordKey))
	if err != nil {
		bad(protocol.HeaderRecordKey + " is not valid percent-encoding")
		return
	}
	if key == "" || len(key) > protocol.MaxRecordKeyBytes || !utf8.ValidString(key) {
		bad(protocol.HeaderRecordKey + " must be non-empty UTF-8 of at most 1024 bytes")
		return
	}
	mode := r.Header.Get(protocol.HeaderMode)
	if mode != protocol.ModeAppend && mode != protocol.ModeReplace {
		bad(protocol.HeaderMode + " must be append or replace")
		return
	}
	offset, err := strconv.ParseInt(r.Header.Get(protocol.HeaderOffset), 10, 64)
	if err != nil || offset < 0 {
		bad(protocol.HeaderOffset + " must be a non-negative integer")
		return
	}
	if mode == protocol.ModeReplace && offset != 0 {
		bad(protocol.HeaderOffset + " must be 0 for replace")
		return
	}
	prefix := r.Header.Get(protocol.HeaderPrefixSha256)
	if prefix == "" && offset == 0 {
		prefix = protocol.EmptySha256
	}
	if mode == protocol.ModeAppend && !sha256Hex.MatchString(prefix) {
		bad(protocol.HeaderPrefixSha256 + " must be lowercase hex sha256")
		return
	}
	if !strings.EqualFold(r.Header.Get("Content-Encoding"), protocol.ContentEncodingZstd) {
		s.fail(w, r, http.StatusUnsupportedMediaType, protocol.Error{Error: protocol.ErrUnsupportedEncoding, Message: "Content-Encoding must be zstd"})
		return
	}

	compressed, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCompressedBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.fail(w, r, http.StatusRequestEntityTooLarge, protocol.Error{Error: protocol.ErrBodyTooLarge})
		return
	}
	if err != nil {
		bad("reading body: " + err.Error())
		return
	}
	data, err := decompress(compressed)
	if errors.Is(err, errTooLarge) {
		s.fail(w, r, http.StatusRequestEntityTooLarge, protocol.Error{Error: protocol.ErrBodyTooLarge})
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusUnsupportedMediaType, protocol.Error{Error: protocol.ErrUnsupportedEncoding, Message: "body is not valid zstd"})
		return
	}

	var st protocol.RecordState
	if mode == protocol.ModeReplace {
		st, err = s.store.Replace(r.Context(), store.ReplaceRequest{
			MachineID:  id,
			Source:     source,
			RecordKey:  key,
			Data:       data,
			Compressed: compressed,
		})
	} else {
		st, err = s.store.Append(r.Context(), store.AppendRequest{
			MachineID:    id,
			Source:       source,
			RecordKey:    key,
			Offset:       offset,
			PrefixSha256: prefix,
			Data:         data,
			Compressed:   compressed,
		})
	}
	var conflict *store.ConflictError
	if errors.As(err, &conflict) {
		s.log.Warn("append conflict", "machine", id, "source", source, "key", key, "offset", offset, "hub_length", conflict.Length)
		writeJSON(w, http.StatusConflict, protocol.Conflict{Error: protocol.ErrOffsetMismatch, Length: conflict.Length, Sha256: conflict.Sha256})
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.log.Debug("ingested chunk", "machine", id, "source", source, "key", key, "mode", mode, "offset", offset, "bytes", len(data), "version", st.Version)
	writeJSON(w, http.StatusOK, st)
}

var errTooLarge = errors.New("decompressed body over the limit")

func decompress(compressed []byte) ([]byte, error) {
	dec, err := zstd.NewReader(bytes.NewReader(compressed), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	data, err := io.ReadAll(io.LimitReader(dec, protocol.MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > protocol.MaxBodyBytes {
		return nil, errTooLarge
	}
	return data, nil
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, status int, e protocol.Error) {
	s.log.Warn("rejected request", "method", r.Method, "path", r.URL.Path, "status", status, "error", e.Error, "message", e.Message)
	writeJSON(w, status, e)
}

func (s *server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, protocol.Error{Error: protocol.ErrInternal})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
