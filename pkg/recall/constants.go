package recall

import "time"

const (
	ExtractorVersion            = 1
	SummaryPromptVersion        = 1
	WindowTargetChars           = 3200 // ~800 tokens at 4 chars/token
	ToolArgMaxChars             = 1500
	SnippetChars                = 200
	CharsPerToken               = 4
	QuietPeriod                 = time.Hour
	SummaryMaxOutputTokens      = 2048
	SummaryPromptOverheadTokens = 2000
	UnknownContextTokens        = 100000
	SummaryMaxFailures          = 3
	RRFK                        = 60
	RecallDefaultLimit          = 10
	RecallMaxLimit              = 50
	RecallMaxOutputChars        = 6000
	ContextDefaultMaxChars      = 8000
	TreeMaxChars                = 20000
	// IndexerSweep is how often the indexer runs a full pass with no event to
	// prompt it. New messages prompt a pass themselves (see Indexer.Mark), so
	// the sweep only catches what no event announces: time-gated summaries and
	// embeddings, rows written outside the daemon's commit path, backfill and
	// extractor upgrades.
	IndexerSweep = time.Minute
	// IndexerDebounce is how long message commits must pause before the
	// indexer extracts them, and IndexerMaxDebounce bounds the wait under a
	// steady stream of commits.
	IndexerDebounce    = 3 * time.Second
	IndexerMaxDebounce = 30 * time.Second
	EmbedBatchSize     = 64
	SummaryEntrypoint  = "recall-summary"
)

// ExcludedEntrypoints are conversation.origin_entrypoint values the indexer
// never windows, embeds or summarizes: machine conversations produced by the
// daemon's own LLM jobs (the summarizer's calls are themselves captured).
var ExcludedEntrypoints = []string{"recall-summary", "analyze"}
