// Package hubclient is the Collector's HTTP client for the Hub's /api/v1
// ingestion endpoints (protocol.md §2).
package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
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
	if resp.StatusCode != want {
		var e protocol.Error
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if resp.StatusCode == http.StatusConflict {
			return &ConflictError{Length: e.Length, Sha256: e.Sha256}
		}
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

// Append sends data as an append at offset. prefixSha256 is the hex sha256
// of the record's bytes before offset. A 409 comes back as *ConflictError.
func (c *Client) Append(ctx context.Context, source, key string, offset int64, prefixSha256 string, data []byte) (protocol.RecordState, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/records", bytes.NewReader(c.enc.EncodeAll(data, nil)))
	if err != nil {
		return protocol.RecordState{}, err
	}
	req.Header.Set(protocol.HeaderSource, source)
	req.Header.Set(protocol.HeaderRecordKey, url.PathEscape(key))
	req.Header.Set(protocol.HeaderMode, protocol.ModeAppend)
	req.Header.Set(protocol.HeaderOffset, strconv.FormatInt(offset, 10))
	req.Header.Set(protocol.HeaderPrefixSha256, prefixSha256)
	req.Header.Set("Content-Encoding", protocol.ContentEncodingZstd)
	var st protocol.RecordState
	err = c.do(req, http.StatusOK, &st)
	return st, err
}
