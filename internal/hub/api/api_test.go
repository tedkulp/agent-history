package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

const machine = "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33"

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(New(s, nil))
	t.Cleanup(srv.Close)
	return srv
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func compress(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeAll(b, nil)
}

type recordReq struct {
	machineHeader string
	key           string
	mode          string
	offset        int64
	prefix        []byte
	body          []byte
	rawBody       []byte // sent as-is when set
	encoding      string
}

func postRecord(t *testing.T, srv *httptest.Server, r recordReq) *http.Response {
	t.Helper()
	body := r.rawBody
	if body == nil {
		body = compress(t, r.body)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/machines/"+machine+"/records", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	mh := r.machineHeader
	if mh == "" {
		mh = machine
	}
	mode := r.mode
	if mode == "" {
		mode = protocol.ModeAppend
	}
	enc := r.encoding
	if enc == "" {
		enc = protocol.ContentEncodingZstd
	}
	req.Header.Set(protocol.HeaderMachineID, mh)
	req.Header.Set(protocol.HeaderSource, protocol.SourceClaudeCode)
	req.Header.Set(protocol.HeaderRecordKey, url.PathEscape(r.key))
	req.Header.Set(protocol.HeaderMode, mode)
	req.Header.Set(protocol.HeaderOffset, strconv.FormatInt(r.offset, 10))
	if r.offset > 0 || r.prefix != nil {
		req.Header.Set(protocol.HeaderPrefixSha256, hexSum(r.prefix))
	}
	req.Header.Set("Content-Encoding", enc)
	req.Header.Set("User-Agent", protocol.UserAgentProduct+"/0.0.0-dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func getManifest(t *testing.T, srv *httptest.Server, id string) protocol.Manifest {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/machines/"+id+"/manifest", nil)
	req.Header.Set(protocol.HeaderMachineID, id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status %d", resp.StatusCode)
	}
	return decode[protocol.Manifest](t, resp)
}

func TestAppendThenManifest(t *testing.T) {
	srv := newServer(t)
	key := "-Users-ted-src-app/5f1c0000-0000-4000-8000-0000000000e2.jsonl"
	a := []byte("{\"x\":1}\n")
	b := []byte("{\"y\":2}\n")

	resp := postRecord(t, srv, recordReq{key: key, body: a})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	st := decode[protocol.RecordState](t, resp)
	if st.Length != int64(len(a)) || st.Sha256 != hexSum(a) || st.Version != 1 {
		t.Fatalf("state %+v", st)
	}

	resp = postRecord(t, srv, recordReq{key: key, offset: int64(len(a)), prefix: a, body: b})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second append status %d", resp.StatusCode)
	}

	all := append(append([]byte{}, a...), b...)
	m := getManifest(t, srv, machine)
	want := protocol.ManifestRecord{Source: protocol.SourceClaudeCode, RecordKey: key, Length: int64(len(all)), Sha256: hexSum(all)}
	if len(m.Records) != 1 || m.Records[0] != want {
		t.Fatalf("manifest %+v, want %+v", m.Records, want)
	}

	if other := getManifest(t, srv, "11111111-1111-4111-8111-111111111111"); len(other.Records) != 0 {
		t.Fatalf("other Machine's manifest %+v", other.Records)
	}
}

func TestAppendConflictReturnsHubState(t *testing.T) {
	srv := newServer(t)
	data := []byte("line\n")
	if resp := postRecord(t, srv, recordReq{key: "k.jsonl", body: data}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, r := range []recordReq{
		{key: "k.jsonl", offset: 3, prefix: data[:3], body: []byte("x\n")},
		{key: "k.jsonl", offset: int64(len(data)), prefix: []byte("LINE\n"), body: []byte("x\n")},
	} {
		resp := postRecord(t, srv, r)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status %d, want 409", resp.StatusCode)
		}
		e := decode[protocol.Conflict](t, resp)
		if e.Error != protocol.ErrOffsetMismatch || e.Length != int64(len(data)) || e.Sha256 != hexSum(data) {
			t.Fatalf("409 body %+v", e)
		}
	}
}

func TestRecordsRejectsBadRequests(t *testing.T) {
	srv := newServer(t)
	cases := []struct {
		name string
		req  recordReq
		want int
	}{
		{"machine header mismatch", recordReq{key: "k", machineHeader: "other", body: []byte("x")}, http.StatusBadRequest},
		{"empty key", recordReq{key: "", body: []byte("x")}, http.StatusBadRequest},
		{"unknown mode", recordReq{key: "k", mode: "merge", body: []byte("x")}, http.StatusBadRequest},
		{"not zstd encoding", recordReq{key: "k", encoding: "gzip", body: []byte("x")}, http.StatusUnsupportedMediaType},
		{"body not zstd", recordReq{key: "k", rawBody: []byte("plain text")}, http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postRecord(t, srv, tc.req)
			if resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tc.want, b)
			}
		})
	}
}

func TestPutMachine(t *testing.T) {
	srv := newServer(t)
	put := func(id, header string, info protocol.MachineInfo) int {
		b, _ := json.Marshal(info)
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/machines/"+id, bytes.NewReader(b))
		req.Header.Set(protocol.HeaderMachineID, header)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	info := protocol.MachineInfo{DisplayName: "laptop", HomeDir: "/Users/ted", OS: "darwin", Arch: "arm64"}
	if got := put(machine, machine, info); got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if got := put(machine, "other", info); got != http.StatusBadRequest {
		t.Fatalf("mismatched header: status %d", got)
	}
	info.HomeDir = ""
	if got := put(machine, machine, info); got != http.StatusBadRequest {
		t.Fatalf("missing home_dir: status %d", got)
	}
}

func TestConflictOnUnknownRecordCarriesZeroLength(t *testing.T) {
	srv := newServer(t)
	resp := postRecord(t, srv, recordReq{key: "new.jsonl", offset: 3, prefix: []byte("abc"), body: []byte("x\n")})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if raw["length"] != float64(0) || raw["sha256"] != protocol.EmptySha256 {
		t.Fatalf("409 body %v", raw)
	}
}
