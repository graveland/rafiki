package insights

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SearchFilter narrows a conversation search. Zero-value fields are ignored, so
// an empty filter returns the most recent conversations up to Limit.
type SearchFilter struct {
	Since, Until *time.Time
	Owner        string
	Persona      string
	Source       string
	Model        string
	Status       string
	Path         Path
	MinTokens    int64  // minimum total (input+output) tokens across the conversation's turns
	Text         string // ILIKE substring match against the first user message snippet
	Limit        int    // defaults to 50 when <= 0
}

// ConversationSummary is one row of Search: the conversation plus a cheap
// per-conversation turn aggregate and the first user message snippet.
type ConversationSummary struct {
	ID       string `json:"id"`
	Owner    string `json:"owner"`
	Persona  string `json:"persona"`
	Source   string `json:"source"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	DrivenBy string `json:"driven_by"`

	CreatedAt time.Time `json:"created_at"`
	Turns     int       `json:"turns"`

	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`

	FirstMessage string `json:"first_message"`
}

const defaultSearchLimit = 50

// Search returns conversations matching f, most recent first. Token counts and
// turn counts come from a per-conversation aggregate over conversation_turn;
// FirstMessage is the leading 200 chars of the earliest user message.
func (i *Insights) Search(ctx context.Context, f SearchFilter) ([]ConversationSummary, error) {
	if err := f.Path.validate(); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	var a argList
	conds := []string{"1=1"}
	if db := f.Path.drivenBy(); db != "" {
		conds = append(conds, "c.driven_by = "+a.next(db))
	}
	if f.Owner != "" {
		conds = append(conds, "c.owner = "+a.next(f.Owner))
	}
	if f.Persona != "" {
		conds = append(conds, "c.persona = "+a.next(f.Persona))
	}
	// Model and Source filter on the PER-TURN served value via EXISTS, matching
	// the population GlobalStats selects (which filters t.model / t.source).
	// Filtering the conversation-level c.model here would diverge from stats,
	// since the served model is recorded per turn.
	if f.Model != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM conversations.conversation_turn tm "+
			"WHERE tm.conversation_id = c.id AND tm.model = "+a.next(f.Model)+")")
	}
	if f.Status != "" {
		conds = append(conds, "c.status = "+a.next(f.Status))
	}
	if f.Since != nil {
		conds = append(conds, "c.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "c.created_at < "+a.next(*f.Until))
	}
	if f.Source != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM conversations.conversation_turn ts "+
			"WHERE ts.conversation_id = c.id AND ts.source = "+a.next(f.Source)+")")
	}
	if f.MinTokens > 0 {
		conds = append(conds, "coalesce(t.in_tok,0) + coalesce(t.out_tok,0) >= "+a.next(f.MinTokens))
	}
	if f.Text != "" {
		conds = append(conds, "fm.first_text ILIKE '%' || "+a.next(f.Text)+" || '%'")
	}

	query := `
SELECT c.id::text, coalesce(c.owner,''), coalesce(c.persona,''),
       coalesce(t.source,''), coalesce(c.model,''), c.status, c.driven_by, c.created_at,
       coalesce(t.turns,0), coalesce(t.in_tok,0), coalesce(t.out_tok,0), coalesce(t.cache_read,0),
       coalesce(left(fm.first_text, 200), '')
FROM conversations.conversation c
LEFT JOIN LATERAL (
    SELECT count(*) AS turns,
           sum(input_tokens) AS in_tok, sum(output_tokens) AS out_tok,
           sum(cache_read_tokens) AS cache_read,
           min(source) AS source
    FROM conversations.conversation_turn WHERE conversation_id = c.id
) t ON true
LEFT JOIN LATERAL (
    -- Extract the message's actual text, not raw JSONB: content is either an
    -- array of blocks (take the first type=text block's .text) or a plain
    -- JSON string. Feeds both the snippet and the Text ILIKE filter, so search
    -- matches message text rather than JSON structure (keys like "type").
    SELECT CASE jsonb_typeof(content)
             WHEN 'array'  THEN jsonb_path_query_first(content, '$[*] ? (@.type == "text").text') #>> '{}'
             WHEN 'string' THEN content #>> '{}'
           END AS first_text
    FROM conversations.conversation_message
    WHERE conversation_id = c.id AND role = 'user' ORDER BY ordinal LIMIT 1
) fm ON true
WHERE ` + strings.Join(conds, "\n  AND ") + `
ORDER BY c.created_at DESC
LIMIT ` + a.next(limit)

	rows, err := i.pool.Query(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("search conversations: %w", err)
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var s ConversationSummary
		if err := rows.Scan(&s.ID, &s.Owner, &s.Persona, &s.Source, &s.Model, &s.Status, &s.DrivenBy,
			&s.CreatedAt, &s.Turns, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.FirstMessage); err != nil {
			return nil, fmt.Errorf("scan conversation summary: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
