// Package protocol holds the Collector → Hub wire contract shared by both
// binaries: header names, Source identifiers, JSON bodies and the minimum
// Collector version. See docs/spec/protocol.md.
package protocol

// APIPrefix is the path prefix of every ingestion endpoint.
const APIPrefix = "/api/v1"

// MinCollectorVersion is the compiled-in floor for Collector versions
// (protocol.md §4.6). Bump it by hand, only with an incompatible /api/v1 change.
const MinCollectorVersion = "0.1.0"

// UserAgentProduct prefixes the Collector's User-Agent: "agent-history-collector/<version>".
const UserAgentProduct = "agent-history-collector"

// Request headers (protocol.md §2.2, §2.3).
const (
	HeaderMachineID    = "X-Machine-Id"
	HeaderSource       = "X-Source"
	HeaderRecordKey    = "X-Record-Key"
	HeaderMode         = "X-Mode"
	HeaderOffset       = "X-Offset"
	HeaderPrefixSha256 = "X-Prefix-Sha256"
)

// Values of X-Mode.
const (
	ModeAppend  = "append"
	ModeReplace = "replace"
)

// ContentEncodingZstd is the only accepted Content-Encoding for record bodies.
const ContentEncodingZstd = "zstd"

// Source identifiers (protocol.md §3.1).
const (
	SourceClaudeCode = "claude-code"
	SourceCodex      = "codex"
	SourceOhMyPi     = "oh-my-pi"
	SourceOpencode   = "opencode"
)

// Size limits on a record body, in decompressed bytes (protocol.md §2.3).
const (
	ChunkSize    = 8 << 20
	MaxBodyBytes = 64 << 20
)

// MaxRecordKeyBytes is the longest Record key allowed (protocol.md §3.3).
const MaxRecordKeyBytes = 1024

// EmptySha256 is the lowercase hex sha256 of the empty string.
const EmptySha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// MachineInfo is the body of PUT /machines/{id}.
type MachineInfo struct {
	DisplayName      string       `json:"display_name"`
	Hostname         string       `json:"hostname"`
	OS               string       `json:"os"`
	Arch             string       `json:"arch"`
	HomeDir          string       `json:"home_dir"`
	CollectorVersion string       `json:"collector_version"`
	Sources          []SourceInfo `json:"sources"`
}

// SourceInfo describes one Source in MachineInfo.
type SourceInfo struct {
	Source   string   `json:"source"`
	Detected bool     `json:"detected"`
	Version  *string  `json:"version"`
	Root     string   `json:"root"`
	Layouts  []string `json:"layouts"`
}

// Manifest is the body of GET /machines/{id}/manifest.
type Manifest struct {
	Records []ManifestRecord `json:"records"`
}

// ManifestRecord is the current version of one Raw record.
type ManifestRecord struct {
	Source    string `json:"source"`
	RecordKey string `json:"record_key"`
	Length    int64  `json:"length"`
	Sha256    string `json:"sha256"`
}

// Health is the body of GET /machines/{id}/health: the Machine's Sessions
// whose latest parse recorded any Parse warning, and those whose latest
// parse failed. Fields may be added later.
type Health struct {
	SessionsWithWarnings int `json:"sessions_with_warnings"`
	SessionsFailed       int `json:"sessions_failed"`
}

// RecordState is the body of a 200 from POST /machines/{id}/records.
type RecordState struct {
	Length  int64  `json:"length"`
	Sha256  string `json:"sha256"`
	Version int64  `json:"version"`
}

// Error is the body of every error response.
type Error struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// Conflict is the body of a 409: the Hub's current state for the record.
type Conflict struct {
	Error  string `json:"error"`
	Length int64  `json:"length"`
	Sha256 string `json:"sha256"`
}

// TooOld is the body of a 426: the Collector is below the minimum version.
type TooOld struct {
	Error               string `json:"error"`
	MinCollectorVersion string `json:"min_collector_version"`
}

// Error codes.
const (
	ErrBadRequest          = "bad_request"
	ErrOffsetMismatch      = "offset_mismatch"
	ErrBodyTooLarge        = "body_too_large"
	ErrUnsupportedEncoding = "unsupported_encoding"
	ErrInternal            = "internal"
	ErrCollectorTooOld     = "collector_too_old"
)
