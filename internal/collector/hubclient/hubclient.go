// Package hubclient is the Collector's HTTP client for the Hub's /api/v1
// ingestion endpoints (protocol.md §2).
package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/protocol"
)

// Client talks to one Hub on behalf of one Machine.
type Client struct {
	baseURL   string
	machineID string
	userAgent string
	http      *http.Client
	enc       *zstd.Encoder
}

// New returns a Client. version is the Collector's version without a "v".
func New(hubURL, machineID, version string, hc *http.Client) (*Client, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL:   strings.TrimRight(hubURL, "/") + protocol.APIPrefix + "/machines/" + url.PathEscape(machineID),
		machineID: machineID,
		userAgent: protocol.UserAgentProduct + "/" + version,
		http:      hc,
		enc:       enc,
	}, nil
}

// StatusError is an unexpected response from the Hub.
type StatusError struct {
	StatusCode int
	Body       protocol.Error
}

func (e *StatusError) Error() string {
	if e.Body.Message != "" {
		return fmt.Sprintf("hub returned %d %s: %s", e.StatusCode, e.Body.Error, e.Body.Message)
	}
	return fmt.Sprintf("hub returned %d %s", e.StatusCode, e.Body.Error)
}

// TooOldError is a 426: the Hub requires a newer Collector (protocol.md §4.6).
type TooOldError struct {
	// MinVersion is the Hub's minimum Collector version, or "" when the
	// response didn't say.
	MinVersion string
}

func (e *TooOldError) Error() string {
	if e.MinVersion == "" {
		return "Hub requires a newer Collector"
	}
	return "Hub requires Collector ≥ " + e.MinVersion
}

// ConflictError is a 409 from an append. It carries the Hub's state.
type ConflictError struct {
	Length int64
	Sha256 string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("hub rejected append: it has length %d", e.Length)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(protocol.HeaderMachineID, c.machineID)
	req.Header.Set("User-Agent", c.userAgent)
	return req, nil
}

func (c *Client) do(req *http.Request, want int, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict && want != http.StatusConflict {
		var c protocol.Conflict
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil || c.Sha256 == "" {
			return &StatusError{StatusCode: resp.StatusCode, Body: protocol.Error{Error: c.Error, Message: "409 without the Hub's record state"}}
		}
		return &ConflictError{Length: c.Length, Sha256: c.Sha256}
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		var t protocol.TooOld
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t)
		return &TooOldError{MinVersion: t.MinCollectorVersion}
	}
	if resp.StatusCode != want {
		var e protocol.Error
		// The body is best-effort detail; the status code is the error.
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		return &StatusError{StatusCode: resp.StatusCode, Body: e}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// PutMachine registers the Machine or updates its metadata.
func (c *Client) PutMachine(ctx context.Context, info protocol.MachineInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, http.StatusNoContent, nil)
}

// Manifest returns the current version of every Raw record the Hub holds
// for this Machine.
func (c *Client) Manifest(ctx context.Context) ([]protocol.ManifestRecord, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/manifest", nil)
	if err != nil {
		return nil, err
	}
	var m protocol.Manifest
	if err := c.do(req, http.StatusOK, &m); err != nil {
		return nil, err
	}
	return m.Records, nil
}

// Ping checks that the Hub answers for this Machine, for `status` when no
// service is running. It asks for the manifest's headers only.
func (c *Client) Ping(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodHead, "/manifest", nil)
	if err != nil {
		return err
	}
	err = c.do(req, http.StatusOK, nil)
	var tooOld *TooOldError
	if errors.As(err, &tooOld) {
		return c.tooOldWithMinimum(ctx, tooOld)
	}
	return err
}

// tooOldWithMinimum fills in the minimum a 426 to HEAD couldn't carry. The
// Hub answers GET with the same 426 before building any manifest, so the
// GET costs no more than the HEAD.
func (c *Client) tooOldWithMinimum(ctx context.Context, headErr *TooOldError) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/manifest", nil)
	if err != nil {
		return headErr
	}
	var tooOld *TooOldError
	if err := c.do(req, http.StatusOK, nil); errors.As(err, &tooOld) && tooOld.MinVersion != "" {
		return tooOld
	}
	return headErr
}

// Append sends data as an append at offset. prefixSha256 is the hex sha256
// of the record's bytes before offset. A 409 comes back as *ConflictError.
func (c *Client) Append(ctx context.Context, source, key string, offset int64, prefixSha256 string, data []byte) (protocol.RecordState, error) {
	return c.postRecord(ctx, source, key, protocol.ModeAppend, offset, prefixSha256, data)
}

// Replace sends data as the first chunk of a new current version of the
// record. The Hub keeps the old version as superseded.
func (c *Client) Replace(ctx context.Context, source, key string, data []byte) (protocol.RecordState, error) {
	return c.postRecord(ctx, source, key, protocol.ModeReplace, 0, "", data)
}

func (c *Client) postRecord(ctx context.Context, source, key, mode string, offset int64, prefixSha256 string, data []byte) (protocol.RecordState, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/records", bytes.NewReader(c.enc.EncodeAll(data, nil)))
	if err != nil {
		return protocol.RecordState{}, err
	}
	req.Header.Set(protocol.HeaderSource, source)
	req.Header.Set(protocol.HeaderRecordKey, url.PathEscape(key))
	req.Header.Set(protocol.HeaderMode, mode)
	req.Header.Set(protocol.HeaderOffset, strconv.FormatInt(offset, 10))
	if mode == protocol.ModeAppend {
		req.Header.Set(protocol.HeaderPrefixSha256, prefixSha256)
	}
	req.Header.Set("Content-Encoding", protocol.ContentEncodingZstd)
	var st protocol.RecordState
	err = c.do(req, http.StatusOK, &st)
	return st, err
}

// Transient reports whether err means the Hub is unreachable or failing: a
// connection error or a 5xx (protocol.md §4.5). The Collector backs off and
// reconciles after one. A cancelled request is not transient.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.StatusCode >= 500
	}
	var ue *url.Error
	return errors.As(err, &ue)
}
