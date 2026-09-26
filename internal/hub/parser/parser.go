// Package parser defines the Hub-side Source parser interface (hub.md §2.5)
// and the normalized Transcript model every parser produces (hub.md §3.5).
// The Hub core never looks inside a Source format itself.
package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"unicode/utf8"
)

// Roles a Raw record can have within its Session (hub.md §4.3).
const (
	RoleMain       = "main"
	RoleAttachment = "attachment"
)

// Message roles.
const (
	MessageUser      = "user"
	MessageAssistant = "assistant"
)

// Part kinds (hub.md §3.5).
const (
	KindText = "text"
)

// Parse warning kinds (hub.md §3.6).
const (
	WarnBadLine      = "bad_line"
	WarnMissingField = "missing_field"
	WarnOrphan       = "orphan"
)

// Mapping is what MapKey returns for a Record key the parser owns.
type Mapping struct {
	NativeID   string
	Role       string // RoleMain or RoleAttachment
	Layout     string
	LayoutRank int
}

// Input is one Session's Raw records, as handed to Parse.
type Input struct {
	NativeID    string
	MainKey     string
	Main        []byte
	Attachments map[string][]byte // keyed by Record key
	HomeDir     string
}

// Result is one parsed Session.
type Result struct {
	Session  Session
	Messages []Message
	Warnings []Warning
}

// Session holds the Session fields a parse produces. Times are Unix
// milliseconds, 0 when unknown.
type Session struct {
	Title              string
	StartedAt          int64
	LastActivityAt     int64
	Cwd                string
	GitBranch          string
	SourceVersion      string
	ParentNativeID     string
	SpawningCallID     string
	ForkedFromNativeID string
}

// Message is one turn in a Transcript. Model, Provider and Usage are set on
// assistant Messages only.
type Message struct {
	ID        string
	Role      string
	Timestamp int64
	Model     string
	Provider  string
	Usage     *Usage
	Parts     []Part
}

// Part is one typed piece of a Message. Payload is marshalled to JSON as the
// Part's payload_json.
type Part struct {
	ID      string
	Kind    string
	Payload any
}

// TextPayload is the payload of a text Part.
type TextPayload struct {
	Text string `json:"text"`
}

// Usage is an assistant Message's token usage.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

// Add adds u2 to u.
func (u *Usage) Add(u2 Usage) {
	u.Input += u2.Input
	u.Output += u2.Output
	u.CacheRead += u2.CacheRead
	u.CacheWrite += u2.CacheWrite
	u.Reasoning += u2.Reasoning
}

// Warning is one aggregated Parse warning.
type Warning struct {
	Kind         string
	SourceType   string
	Count        int
	FirstExcerpt string
}

// Parser turns one Source's Raw records into Transcripts.
type Parser interface {
	Source() string
	Version() int
	// MapKey reports which Session a Record key belongs to, or ok=false when
	// the key is not this parser's. It must be a pure function.
	MapKey(recordKey string) (m Mapping, ok bool)
	// Parse must be deterministic: the same input gives the same output.
	Parse(in Input) (Result, error)
}

// Registry holds one Parser per Source identifier.
type Registry map[string]Parser

// NewRegistry indexes parsers by Source.
func NewRegistry(ps ...Parser) Registry {
	r := Registry{}
	for _, p := range ps {
		r[p.Source()] = p
	}
	return r
}

// MapKey calls MapKey on the Source's parser; ok is false for an unknown Source.
func (r Registry) MapKey(source, recordKey string) (Mapping, bool) {
	p, ok := r[source]
	if !ok {
		return Mapping{}, false
	}
	return p.MapKey(recordKey)
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// SafeID returns id if it matches the allowed id charset (hub.md §3.5),
// else a hex hash of it.
func SafeID(id string) string {
	if idPattern.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

// Warnings aggregates Parse warnings by (kind, source type), keeping the
// first excerpt and the order of first appearance.
type Warnings struct {
	list  []Warning
	index map[[2]string]int
}

// Add records one occurrence. The excerpt is truncated to 500 bytes.
func (w *Warnings) Add(kind, sourceType, excerpt string) {
	if w.index == nil {
		w.index = map[[2]string]int{}
	}
	k := [2]string{kind, sourceType}
	if i, ok := w.index[k]; ok {
		w.list[i].Count++
		return
	}
	if len(excerpt) > 500 {
		// Cut at a character boundary.
		n := 500
		for n > 0 && !utf8.RuneStart(excerpt[n]) {
			n--
		}
		excerpt = excerpt[:n]
	}
	w.index[k] = len(w.list)
	w.list = append(w.list, Warning{Kind: kind, SourceType: sourceType, Count: 1, FirstExcerpt: excerpt})
}

// List returns the aggregated warnings.
func (w *Warnings) List() []Warning { return w.list }
