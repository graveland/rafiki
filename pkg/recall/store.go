package recall

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Message is one captured conversation message, as stored for recall.
type Message struct {
	ConversationID string
	Ordinal        int
	Role           string          // "user" | "assistant"
	Kind           string          // "" or "compaction_summary"
	Content        json.RawMessage // Anthropic content blocks array (or a JSON string)
	CreatedAt      time.Time
}

// ConversationMeta describes a captured conversation for search hits, the
// embed header and the indexer.
type ConversationMeta struct {
	ID          string
	OwnerUserID string
	Name        string
	Repo        string // basename(repo_root), "" if none
	Kind        string // driven_by-derived label: "fundi" when origin_entrypoint="agent", "claude" otherwise
	CreatedAt   time.Time
	LastAt      time.Time
	MaxOrdinal  int
	Stopped     bool
}

// Window is one indexed slice of a conversation: at most WindowTargetChars of
// extracted message text, sealed once later windows exist.
type Window struct {
	ID               string
	ConversationID   string
	OwnerUserID      string
	Seq              int
	OrdinalFrom      int
	OrdinalTo        int
	Text             string
	Sealed           bool
	ExtractorVersion int
}

// Summary is one LLM-written summary of a conversation segment or whole.
type Summary struct {
	ID             string
	ConversationID string
	OwnerUserID    string
	Level          string // "segment" | "conversation"
	Seq            int
	OrdinalFrom    int
	OrdinalTo      int
	Title          string
	Summary        string
	PromptVersion  int
	Model          string
	InputTokens    int64
	OutputTokens   int64
	CostUSD        float64 // this call's cost; UpsertSummary adds it to total_cost_usd
	TotalCostUSD   float64 // read-only, as stored
	CreatedAt      time.Time
}

// Memory is one curated memory document.
type Memory struct {
	ID          string
	OwnerUserID string
	Path        string
	Name        string
	Body        string
	Meta        json.RawMessage // "{}" when absent
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// EmbedItem is one row awaiting an embedding vector.
type EmbedItem struct {
	Source Source
	ID     string
	Text   string // header + text, ready to embed
}

// ExtractCursor is one indexer candidate: the conversation plus its current
// windowing tail (nil before the first window exists).
type ExtractCursor struct {
	Conversation ConversationMeta
	Tail         *Window // nil when the conversation has no windows yet
}

// EligibleOpts selects summary candidates.
type EligibleOpts struct {
	PromptVersion int
	Model         string
	Limit         int
}

// Status reports recall index state for tooling.
type Status struct {
	Conversations     int64
	Windows           int64
	WindowsUnembedded int64
	Summaries         int64
	SummariesPending  int64
	Memories          int64
	SummaryCostUSD    float64
	BackfillSince     *time.Time
	BackfillBudgetUSD float64
	BackfillSpentUSD  float64
	EmbeddingModel    string
}

// Store is the recall persistence boundary: memories, per-source search lists,
// context expansion, the background indexer's working set, summaries, and
// shared state. Implementations enforce Scope/owner checks before any SQL.
type Store interface {
	// memories (owner-scoped; tombstones never returned)
	PutMemory(ctx context.Context, ownerUserID string, m Memory) (Memory, error)
	GetMemory(ctx context.Context, ownerUserID, path, name string) (Memory, error)
	MemoryTree(ctx context.Context, ownerUserID, path string, depth int) ([]Memory, error)
	DeleteMemory(ctx context.Context, ownerUserID, path, name string) error

	// search: one ranked list per source; fusion happens in recall.Search
	SearchBM25(ctx context.Context, q SearchQuery, src Source) ([]Hit, error)
	SearchVector(ctx context.Context, q SearchQuery, src Source, model string, vec []float32) ([]Hit, error)

	// context expansion
	Conversation(ctx context.Context, scope Scope, conversationID string) (ConversationMeta, error)
	Messages(ctx context.Context, scope Scope, conversationID string, fromOrdinal, toOrdinal int) ([]Message, error)
	Window(ctx context.Context, scope Scope, id string) (Window, error)
	Summary(ctx context.Context, scope Scope, id string) (Summary, error)
	MemoryByID(ctx context.Context, ownerUserID, id string) (Memory, error)

	// indexer (daemon trust level; no Scope)
	ExtractCursors(ctx context.Context, excluded []string, limit int) ([]ExtractCursor, error)
	MessagesFrom(ctx context.Context, conversationID string, fromOrdinal int) ([]Message, error)
	WriteWindows(ctx context.Context, conversationID string, ws []Window) error
	PendingEmbeds(ctx context.Context, model string, limit int) ([]EmbedItem, error)
	SetEmbeddings(ctx context.Context, model string, items []EmbedItem, vecs [][]float32) error
	EnsureVectorIndexes(ctx context.Context, model string, dims int) error

	// summaries
	EligibleForSummary(ctx context.Context, excluded []string, o EligibleOpts) ([]ConversationMeta, error)
	Summaries(ctx context.Context, conversationID string) ([]Summary, error)
	UpsertSummary(ctx context.Context, s Summary) error
	DeleteSegmentsFrom(ctx context.Context, conversationID string, fromSeq int) error
	RecordSummaryFailure(ctx context.Context, conversationID string, promptVersion int, model, errText string) error

	// state + coordination
	GetState(ctx context.Context, key string) (string, bool, error)
	SetState(ctx context.Context, key, value string) error
	AddState(ctx context.Context, key string, delta float64) error
	TryLock(ctx context.Context) (release func(), ok bool, err error)
	Status(ctx context.Context) (Status, error)
}

// Embedder produces embedding vectors for recall's vector search.
type Embedder interface {
	Model() string
	Dimensions() int
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Completion is one summarizer LLM call's result and cost.
type Completion struct {
	Text         string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// Completer issues summarizer completions on behalf of an owner.
type Completer interface {
	Model() string
	// ContextWindow returns the model's context length in tokens; ok=false if unknown.
	ContextWindow() (tokens int, ok bool)
	Complete(ctx context.Context, ownerUserID, system, user string, maxTokens int) (Completion, error)
}

var (
	ErrInvalidScope = errors.New("recall: invalid scope")
	ErrNoOwner      = errors.New("recall: no owner")
	ErrNotFound     = errors.New("recall: not found")
	ErrInvalidPath  = errors.New("recall: invalid memory path")
)
