package analyze

const DetectorVersion = 2 // bump when the detector prompt or Finding schema changes

type TurnCite struct {
	Ordinal int    `json:"ordinal"`
	Quote   string `json:"quote"`
}

type Recommendation struct {
	Kind      string `json:"kind"` // new-skill | skill-edit | memory | mcp-tool | none
	SkillName string `json:"skill_name,omitempty"`
	Summary   string `json:"summary"`
}

type Finding struct {
	Axis           string         `json:"axis"` // skill-gap | knowledge-to-persist | grind
	Title          string         `json:"title"`
	TopicKey       string         `json:"topic_key"` // stable slug for cross-conversation grouping
	Evidence       []TurnCite     `json:"evidence"`
	Recommendation Recommendation `json:"recommendation"`
	Confidence     float64        `json:"confidence"`
	GrindTokens    int64          `json:"grind_tokens,omitempty"` // detector-estimated wasted tokens
}

type Analysis struct {
	ConversationID  string            `json:"conversation_id"`
	DetectorVersion int               `json:"detector_version"`
	Model           string            `json:"model"`
	PromptHash      string            `json:"prompt_hash"` // "" = builtin prompts (see Profile.PromptHash)
	Outcome         string            `json:"outcome"`     // one-line what-happened
	Verdicts        map[string]string `json:"verdicts"`    // axis -> ok|finding|n/a
	Findings        []Finding         `json:"findings"`
	InputTokens     int64             `json:"input_tokens"`
	OutputTokens    int64             `json:"output_tokens"`
	CostUSD         float64           `json:"cost_usd"`
}

type RankedFinding struct {
	Finding
	Conversations []string `json:"conversations"` // contributing conv ids
	Occurrences   int      `json:"occurrences"`
	Score         int64    `json:"score"` // expected token savings
}

type SkillFile struct {
	Path    string
	Content string
}

type SkillEdit struct {
	FindingTitle string      `json:"finding_title"`
	Files        []SkillFile `json:"files"` // full proposed contents
	Rationale    string      `json:"rationale"`
}

type CompactPolicy struct {
	MaxToolResultBytes int `yaml:"max_tool_result_bytes" json:"max_tool_result_bytes"` // per tool_result block; default 2048 (head+tail split)
	MaxTranscriptBytes int `yaml:"max_transcript_bytes" json:"max_transcript_bytes"`   // whole-transcript byte proxy for the token budget; default 300<<10
	KeepFirstTurns     int `yaml:"keep_first_turns" json:"keep_first_turns"`           // default 4
	KeepLastTurns      int `yaml:"keep_last_turns" json:"keep_last_turns"`             // default 20
}

type Profile struct {
	Name          string            `yaml:"-" json:"name"`
	DetectorModel string            `yaml:"detector_model" json:"detector_model"`
	RankModel     string            `yaml:"rank_model" json:"rank_model"` // "" = code-only rank
	DraftModel    string            `yaml:"draft_model" json:"draft_model"`
	Limit         int               `yaml:"limit" json:"limit"` // batch cap; 0 = default 50
	Compact       CompactPolicy     `yaml:"compact" json:"compact"`
	Filters       map[string]string `yaml:"filters" json:"filters"` // search-filter fields (since, persona, ...)

	// MaxOutputTokens is the output budget for the forced-tool Detect/Draft
	// calls; 0 = default 16384 (applied by Defaults). Reasoning-heavy models
	// (kimi, o-series style) can burn the llm default of 4096 tokens on
	// preamble before completing the tool call, failing with
	// stop_reason=max_tokens, and need headroom above 16384 (kimi observed
	// 8,871 alone). Models with a low output cap or slow throughput need it
	// lowered instead (opus-4 line must stay <=8192 per the anthropic-sdk-go
	// non-streaming 10-minute guard).
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`

	// Prompt knobs: *Prompt fully replaces the built-in stage prompt;
	// *PromptExtra appends to whichever base is active.
	DetectorPrompt      string `yaml:"detector_prompt" json:"detector_prompt"`
	DetectorPromptExtra string `yaml:"detector_prompt_extra" json:"detector_prompt_extra"`
	DraftPrompt         string `yaml:"draft_prompt" json:"draft_prompt"`
	DraftPromptExtra    string `yaml:"draft_prompt_extra" json:"draft_prompt_extra"`

	// *PromptFile / *PromptExtraFile: yaml-only sugar, resolved by
	// LoadAnalyzerDir into the corresponding inline field above (paths are
	// relative to the analyzer directory; the _file field is zeroed once
	// resolved, so it never appears on the wire). LoadProfiles (the
	// server's single-file dev loader) rejects these as unsupported.
	DetectorPromptFile      string `yaml:"detector_prompt_file,omitempty" json:"-"`
	DetectorPromptExtraFile string `yaml:"detector_prompt_extra_file,omitempty" json:"-"`
	DraftPromptFile         string `yaml:"draft_prompt_file,omitempty" json:"-"`
	DraftPromptExtraFile    string `yaml:"draft_prompt_extra_file,omitempty" json:"-"`

	// *PromptBase: the analyzer directory's detector.md/draft.md BASE
	// prompts, attached by a resolution layer (not by LoadAnalyzerDir
	// itself — see its doc comment for why). Never read from profiles.yaml
	// (yaml:"-"), but travels on the wire (json) so every consumer — CLI
	// inline, corpus, server — resolves the same effective prompt.
	DetectorPromptBase string `yaml:"-" json:"detector_prompt_base,omitempty"`
	DraftPromptBase    string `yaml:"-" json:"draft_prompt_base,omitempty"`
}

// BuiltinDetectorPrompt and BuiltinDraftPrompt expose the compiled-in stage
// prompts — the last-resort base when no analyzer directory supplies
// detector.md/draft.md (e.g. corpus mode on a machine that has never
// synced). Embedders use them to render effective-prompt reports.
func BuiltinDetectorPrompt() string { return builtinDetectorPrompt }
func BuiltinDraftPrompt() string    { return builtinDraftPrompt }
