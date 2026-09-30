// Package parser defines the Hub-side Source parser interface (hub.md §2.5)
// and the normalized Transcript model every parser produces (hub.md §3.5).
// The Hub core never looks inside a Source format itself.
package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
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
	KindText       = "text"
	KindThinking   = "thinking"
	KindToolCall   = "tool_call"
	KindImage      = "image"
	KindAttachment = "attachment"
	KindMarker     = "marker"
	KindUnknown    = "unknown"
)

// Marker kinds (hub.md §3.5).
const (
	MarkerCompaction    = "compaction"
	MarkerModelChange   = "model_change"
	MarkerThinkingLevel = "thinking_level"
	MarkerSlashCommand  = "slash_command"
	MarkerShellCommand  = "shell_command"
	// MarkerTaskNotification is a background task or sub-agent reporting
	// back: its text is the task's summary.
	MarkerTaskNotification = "task_notification"
)

// CompactionText is a compaction marker's text when the Source gives no
// summary.
const CompactionText = "Conversation compacted"

// Tool call statuses.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusPending = "pending"
)

// Parse warning kinds (hub.md §3.6).
const (
	WarnBadLine      = "bad_line"
	WarnMissingField = "missing_field"
	WarnOrphan       = "orphan"
	WarnUnknownType  = "unknown_type"
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
	Images   []Image // the bytes of every image Part
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

// ThinkingPayload is the payload of a thinking Part: readable thinking or its
// summary only.
type ThinkingPayload struct {
	Text string `json:"text"`
}

// AttachmentPayload is the payload of an attachment Part: a label for a file
// reference, no bytes.
type AttachmentPayload struct {
	Label string `json:"label"`
}

// MarkerPayload is the payload of a marker Part. Output is a shell_command's
// output or a task_notification's event, "" when it has none. Task is set on
// a task_notification only.
type MarkerPayload struct {
	Marker string       `json:"marker"`
	Text   string       `json:"text"`
	Output string       `json:"output,omitempty"`
	Task   *TaskPayload `json:"task,omitempty"`
}

// Task notification statuses.
const (
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskKilled    = "killed"
)

// TaskPayload is what a task_notification marker knows about its task. Each
// field is empty or 0 when the Source doesn't say. CallPart is the id of the
// tool_call Part that started the task, when it is in the Session.
type TaskPayload struct {
	Status     string `json:"status,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	CallPart   string `json:"call_part,omitempty"`
	Result     string `json:"result,omitempty"`
	Tokens     int64  `json:"tokens,omitempty"`
	ToolUses   int64  `json:"tool_uses,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// UnknownPayload is the payload of an unknown Part: the Source type name and
// the first 500 bytes of its raw JSON.
type UnknownPayload struct {
	SourceType string `json:"source_type"`
	Excerpt    string `json:"excerpt"`
}

// NewUnknown returns the payload of an unknown Part for raw JSON of the given
// Source type.
func NewUnknown(sourceType string, raw []byte) UnknownPayload {
	return UnknownPayload{SourceType: sourceType, Excerpt: Excerpt(string(raw))}
}

// ToolCallPayload is the payload of a tool_call Part. Parsers set Output to
// the full output (nil while pending), and Description where the Source
// records what the call is for apart from its input; the Hub sets OutputSize
// and OutputPreview, and moves large output to tool_outputs (hub.md §3.5).
type ToolCallPayload struct {
	CallID        string          `json:"call_id"`
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Input         json.RawMessage `json:"input"`
	Status        string          `json:"status"`
	Output        *string         `json:"output"`
	OutputSize    int             `json:"output_size"`
	OutputPreview string          `json:"output_preview,omitempty"`
	ChildSessions []string        `json:"child_sessions"`
	Diff          *Diff           `json:"diff"`
	// Notifications are the Part ids of the task_notification markers
	// about this call.
	Notifications []string `json:"notifications,omitempty"`
}

// Diff is a file edit a tool call made.
type Diff struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// ImagePayload is the payload of an image Part. Its bytes travel in
// Result.Images.
type ImagePayload struct {
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime"`
	Alt    string `json:"alt"`
}

// Image is the content of an image Part, stored content-addressed.
type Image struct {
	SHA256 string
	MIME   string
	Bytes  []byte
}

// NewImage hashes an image's bytes, returning its Part payload and content.
func NewImage(mime string, b []byte) (ImagePayload, Image) {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	return ImagePayload{SHA256: h, MIME: mime}, Image{SHA256: h, MIME: mime, Bytes: b}
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
	w.index[k] = len(w.list)
	w.list = append(w.list, Warning{Kind: kind, SourceType: sourceType, Count: 1, FirstExcerpt: Excerpt(excerpt)})
}

// excerptMax is the most bytes an excerpt keeps (hub.md §3.5, §3.6).
const excerptMax = 500

// Excerpt cuts s to at most 500 bytes, at a character boundary.
func Excerpt(s string) string { return CutBytes(s, excerptMax) }

// CutBytes cuts s to at most n bytes, at a character boundary.
func CutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// List returns the aggregated warnings.
func (w *Warnings) List() []Warning { return w.list }

// housekeepingCommands is every Housekeeping command, shared by all Sources: they
// never become a Session's title.
var housekeepingCommands = map[string]bool{
	"/clear": true, "/reset": true, "/new": true, "/resume": true, "/compact": true,
	"/model": true, "/login": true, "/logout": true, "/config": true, "/status": true,
	"/cost": true, "/help": true, "/exit": true, "/effort": true, "/fast": true,
	"/theme": true, "/permissions": true, "/context": true, "/usage": true, "/init": true,
}

// FirstUserText returns the text of the first user Message that has any,
// its text Parts joined by newlines, or "" when there is none.
func FirstUserText(msgs []Message) string {
	for _, m := range msgs {
		if t := userText(m); t != "" {
			return t
		}
	}
	return ""
}

// TitleCandidate returns the text a Session is titled by when its Source
// gives no title: the first user Message's text or the first slash command
// that isn't a Housekeeping command, whichever comes first. It is "" when
// there is neither.
func TitleCandidate(msgs []Message) string {
	for _, m := range msgs {
		if m.Role != MessageUser {
			continue
		}
		for _, p := range m.Parts {
			switch pl := p.Payload.(type) {
			case TextPayload:
				if t := userText(m); t != "" {
					return t
				}
			case MarkerPayload:
				if isTitleMarker(pl) {
					return pl.Text
				}
			}
		}
	}
	return ""
}

// userText is a user Message's text Parts joined by newlines, or "" for any
// other Message.
func userText(m Message) string {
	if m.Role != MessageUser {
		return ""
	}
	var texts []string
	for _, p := range m.Parts {
		if tp, ok := p.Payload.(TextPayload); ok {
			texts = append(texts, tp.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// isTitleMarker reports whether a marker is a slash command that isn't a
// Housekeeping command. oh-my-pi extension markers have no leading slash.
func isTitleMarker(mp MarkerPayload) bool {
	if mp.Marker != MarkerSlashCommand {
		return false
	}
	f := strings.Fields(mp.Text)
	return len(f) > 0 && strings.HasPrefix(f[0], "/") && !housekeepingCommands[f[0]]
}
