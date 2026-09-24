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
	IndexerTick                 = 5 * time.Second
	EmbedBatchSize              = 64
	SummaryEntrypoint           = "recall-summary"
)

// ExcludedEntrypoints are conversation.origin_entrypoint values the indexer
// never windows, embeds or summarizes: machine conversations produced by the
// daemon's own LLM jobs (the summarizer's calls are themselves captured).
var ExcludedEntrypoints = []string{"recall-summary", "analyze"}
