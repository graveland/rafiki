package analyze

// analyzeMaxOutputTokens is the output budget for the forced-tool stages.
// Reasoning-heavy models (kimi, o-series style) can burn the llm default of
// 4096 tokens on preamble before completing the tool call, failing with
// stop_reason=max_tokens; the forced-tool JSON itself is small.
const analyzeMaxOutputTokens = 16384

const DetectorVersion = 1 // bump when the detector prompt or Finding schema changes

type TurnCite struct {
	Ordinal int    `json:"ordinal"`
	Quote   string `json:"quote"`
}

type Recommendation struct {
	Kind      string `json:"kind"` // new-skill | skill-edit | memory | none
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

	// Prompt knobs: *Prompt fully replaces the built-in stage prompt;
	// *PromptExtra appends to whichever base is active.
	DetectorPrompt      string `yaml:"detector_prompt" json:"detector_prompt"`
	DetectorPromptExtra string `yaml:"detector_prompt_extra" json:"detector_prompt_extra"`
	DraftPrompt         string `yaml:"draft_prompt" json:"draft_prompt"`
	DraftPromptExtra    string `yaml:"draft_prompt_extra" json:"draft_prompt_extra"`
}
