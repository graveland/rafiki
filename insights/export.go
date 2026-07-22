package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// TranscriptTurn is one message in an exported conversation, annotated with the
// skills it invoked and — for assistant messages — the producing turn's metrics
// (matched via conversation_turn.response_ordinal = message ordinal).
type TranscriptTurn struct {
	Ordinal int             `json:"ordinal"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // verbatim content-block array

	Skills []string `json:"skills"` // skills invoked in this message (Skill tool_use / user /slash markers)

	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	LatencyMS       int    `json:"latency_ms"`
	Model           string `json:"model"`
	PrefixHash      string `json:"prefix_hash"`
}

// Transcript is a decomposed conversation: header identity, the ordered message
// list, and the recovered skill catalog available to the agent.
type Transcript struct {
	ConversationID string `json:"conversation_id"`
	Owner          string `json:"owner"`
	Persona        string `json:"persona"`
	Source         string `json:"source"`
	DrivenBy       string `json:"driven_by"`

	Turns           []TranscriptTurn `json:"turns"`
	AvailableSkills []string         `json:"available_skills"` // catalog recovered from prefix_content, else union of invoked skills
}

// turnMetrics holds the per-turn figures attached to an assistant message.
type turnMetrics struct {
	inTok, outTok, cacheRead int64
	latencyMS                int
	model, prefixHash        string
}

// Export reconstructs a conversation as an ordered, decomposed transcript.
// Works for both paths: the proxy path decomposes requests into
// conversation_message rows just as the in-process path does.
func (i *Insights) Export(ctx context.Context, conversationID string) (*Transcript, error) {
	tr := &Transcript{ConversationID: conversationID}

	err := i.pool.QueryRow(ctx, `
		SELECT coalesce(c.owner,''), coalesce(c.persona,''), c.driven_by,
		       coalesce((SELECT min(source) FROM conversations.conversation_turn
		                  WHERE conversation_id = c.id), '')
		  FROM conversations.conversation c WHERE c.id = $1::uuid`,
		conversationID).Scan(&tr.Owner, &tr.Persona, &tr.DrivenBy, &tr.Source)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("export: conversation %s: %w", conversationID, ErrNotFound)
		}
		return nil, fmt.Errorf("export: load conversation: %w", err)
	}

	metrics, err := i.turnMetricsByOrdinal(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	rows, err := i.pool.Query(ctx, `
		SELECT ordinal, role, content
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid ORDER BY ordinal`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("export: load messages: %w", err)
	}
	defer rows.Close()

	invoked := map[string]struct{}{}
	for rows.Next() {
		var (
			ordinal int
			role    string
			content []byte
		)
		if err := rows.Scan(&ordinal, &role, &content); err != nil {
			return nil, fmt.Errorf("export: scan message: %w", err)
		}
		turn := TranscriptTurn{Ordinal: ordinal, Role: role, Content: json.RawMessage(content)}
		turn.Skills = skillsInMessage(role, content)
		for _, sk := range turn.Skills {
			invoked[sk] = struct{}{}
		}
		if m, ok := metrics[ordinal]; ok {
			turn.InputTokens, turn.OutputTokens, turn.CacheReadTokens = m.inTok, m.outTok, m.cacheRead
			turn.LatencyMS, turn.Model, turn.PrefixHash = m.latencyMS, m.model, m.prefixHash
		}
		tr.Turns = append(tr.Turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export: messages: %w", err)
	}

	tr.AvailableSkills, err = i.availableSkills(ctx, conversationID, invoked)
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// turnMetricsByOrdinal maps the ordinal of each turn's produced assistant
// message to that turn's metrics. The key is coalesce(response_ordinal,
// ordinal): the proxy path stamps response_ordinal (and writes turn ordinal 0),
// while the direct (in-process) path leaves response_ordinal NULL and its turn
// ordinal already equals the assistant message ordinal it produced
// (llm.Conversation derives both from the same nextOrdinal). ORDER BY created_at
// makes the assignment deterministic: on a duplicate key (e.g. a resumed turn
// re-run at the same ordinal) the newest turn's metrics win, stably across runs.
func (i *Insights) turnMetricsByOrdinal(ctx context.Context, conversationID string) (map[int]turnMetrics, error) {
	rows, err := i.pool.Query(ctx, `
		SELECT coalesce(response_ordinal, ordinal), coalesce(input_tokens,0), coalesce(output_tokens,0),
		       coalesce(cache_read_tokens,0), coalesce(latency_ms,0),
		       coalesce(model,''), coalesce(prefix_hash,'')
		  FROM conversations.conversation_turn
		 WHERE conversation_id = $1::uuid
		 ORDER BY created_at`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("export: load turn metrics: %w", err)
	}
	defer rows.Close()
	out := map[int]turnMetrics{}
	for rows.Next() {
		var (
			ord int
			m   turnMetrics
		)
		if err := rows.Scan(&ord, &m.inTok, &m.outTok, &m.cacheRead, &m.latencyMS, &m.model, &m.prefixHash); err != nil {
			return nil, fmt.Errorf("export: scan turn metrics: %w", err)
		}
		out[ord] = m // ORDER BY created_at → newest wins on duplicate keys
	}
	return out, rows.Err()
}

// availableSkills recovers the skill catalog from the latest non-null
// prefix_content (the static system prompt lists skills as "- name: ...").
// When the prefix carries no such listing, it falls back to the union of
// skills actually invoked in the transcript.
func (i *Insights) availableSkills(ctx context.Context, conversationID string, invoked map[string]struct{}) ([]string, error) {
	var prefix []byte
	err := i.pool.QueryRow(ctx, `
		SELECT prefix_content FROM conversations.conversation_turn
		 WHERE conversation_id = $1::uuid AND prefix_content IS NOT NULL
		 ORDER BY created_at DESC, id DESC LIMIT 1`, conversationID).Scan(&prefix)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("export: load prefix_content: %w", err)
	}

	catalog := map[string]struct{}{}
	if len(prefix) > 0 {
		for _, name := range skillListingNames(prefix) {
			catalog[name] = struct{}{}
		}
	}
	if len(catalog) == 0 {
		catalog = invoked
	}
	return sortedKeys(catalog), nil
}

// skillListingRE matches a markdown skill-listing entry, e.g. "- brainstorming:"
// or "- superpowers:writing-plans:". Names are kebab/colon-cased identifiers.
var skillListingRE = regexp.MustCompile(`(?m)^\s*-\s+([a-z0-9][a-z0-9:_.-]*)\s*:\s`)

// skillListingNames extracts skill names from any string scalars in a
// prefix_content JSON value (the system prompt / skills listing).
func skillListingNames(prefix []byte) []string {
	var v any
	if err := json.Unmarshal(prefix, &v); err != nil {
		return nil
	}
	var text strings.Builder
	collectStrings(v, &text)
	var out []string
	for _, m := range skillListingRE.FindAllStringSubmatch(text.String(), -1) {
		out = append(out, m[1])
	}
	return out
}

func collectStrings(v any, b *strings.Builder) {
	switch t := v.(type) {
	case string:
		b.WriteString(t)
		b.WriteByte('\n')
	case []any:
		for _, e := range t {
			collectStrings(e, b)
		}
	case map[string]any:
		for _, e := range t {
			collectStrings(e, b)
		}
	}
}

// skillsInMessage returns the skills a message invokes: Skill tool_use blocks
// (assistant) and leading /slash command markers (user text blocks).
func skillsInMessage(role string, content []byte) []string {
	skills := skillsInContent(content)
	if role == "user" {
		skills = append(skills, slashCommandsInContent(content)...)
	}
	return dedupe(skills)
}

// skillsInContent scans content blocks for {"type":"tool_use","name":"Skill","input":{"skill":"..."}}.
func skillsInContent(content []byte) []string {
	var blocks []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name == "Skill" {
			var in struct {
				Skill string `json:"skill"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Skill != "" {
				out = append(out, in.Skill)
			}
		}
	}
	return out
}

// slashCommandsInContent extracts a leading "/name" marker from user text blocks.
func slashCommandsInContent(content []byte) []string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		text := strings.TrimSpace(b.Text)
		if !strings.HasPrefix(text, "/") {
			continue
		}
		name := strings.TrimPrefix(text, "/")
		if i := strings.IndexAny(name, " \t\n"); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
