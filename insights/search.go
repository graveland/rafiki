package insights

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SearchFilter narrows a conversation search. Zero-value fields are ignored, so
// an empty filter returns the most recent conversations up to Limit.
//
// Since/Until, Model, and Source are TURN-level filters: a conversation
// matches when a single turn satisfies all of them (turn activity in the
// window, not conversation creation time) — the same population
// GlobalStats selects with the same filter.
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

	// Conditions split three ways: conversation-column conds (with the
	// turn-level EXISTS folded in), and conds that reference the LATERAL
	// aggregates and so can only apply outside the conversation scan.
	var a argList
	convConds := []string{"1=1"}
	if db := f.Path.drivenBy(); db != "" {
		convConds = append(convConds, "c.driven_by = "+a.next(db))
	}
	if f.Owner != "" {
		convConds = append(convConds, "c.owner = "+a.next(f.Owner))
	}
	if f.Persona != "" {
		convConds = append(convConds, "c.persona = "+a.next(f.Persona))
	}
	if f.Status != "" {
		convConds = append(convConds, "c.status = "+a.next(f.Status))
	}
	// Model, Source, and Since/Until filter on PER-TURN values via ONE shared
	// EXISTS — a single turn must satisfy all of them, matching the population
	// GlobalStats selects (which ANDs t.model/t.source/t.created_at on the
	// same turn row). Filtering the conversation-level c.model or
	// c.created_at here would diverge from stats: the served model is
	// recorded per turn, and the time window bounds turn activity, not
	// conversation creation.
	var turnConds []string
	if f.Model != "" {
		turnConds = append(turnConds, "tm.model = "+a.next(f.Model))
	}
	if f.Source != "" {
		turnConds = append(turnConds, "tm.source = "+a.next(f.Source))
	}
	if f.Since != nil {
		turnConds = append(turnConds, "tm.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		turnConds = append(turnConds, "tm.created_at < "+a.next(*f.Until))
	}
	if len(turnConds) > 0 {
		convConds = append(convConds, "EXISTS (SELECT 1 FROM conversations.conversation_turn tm "+
			"WHERE tm.conversation_id = c.id AND "+strings.Join(turnConds, " AND ")+")")
	}
	var lateralConds []string
	if f.MinTokens > 0 {
		lateralConds = append(lateralConds, "coalesce(t.in_tok,0) + coalesce(t.out_tok,0) >= "+a.next(f.MinTokens))
	}
	if f.Text != "" {
		lateralConds = append(lateralConds, "fm.first_text ILIKE '%' || "+a.next(f.Text)+" || '%'")
	}

	// With no LATERAL-dependent filters the result page is decided by the
	// conversation scan alone, so order+limit it BEFORE the lateral joins and
	// the per-conversation aggregates run only for the returned page. With
	// such filters the laterals must be probed until the page fills, so the
	// limit stays outside.
	limitArg := a.next(limit)
	convFrom := `conversations.conversation c WHERE ` + strings.Join(convConds, "\n  AND ")
	from := `(SELECT * FROM ` + convFrom + `
  ORDER BY c.created_at DESC LIMIT ` + limitArg + `) c`
	where := ""
	if len(lateralConds) > 0 {
		from = `(SELECT * FROM ` + convFrom + `) c`
		where = "WHERE " + strings.Join(lateralConds, "\n  AND ")
	}

	query := `
SELECT c.id::text, coalesce(c.owner,''), coalesce(c.persona,''),
       coalesce(t.source,''), coalesce(c.model,''), c.status, c.driven_by, c.created_at,
       coalesce(t.turns,0), coalesce(t.in_tok,0), coalesce(t.out_tok,0), coalesce(t.cache_read,0),
       coalesce(left(fm.first_text, 200), '')
FROM ` + from + `
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
` + where + `
ORDER BY c.created_at DESC
LIMIT ` + limitArg

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
