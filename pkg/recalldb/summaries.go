package recalldb

import (
	"context"
	"strconv"

	"go.graveland.dev/rafiki/pkg/recall"
)

// EligibleForSummary lists conversations the summarizer may summarize, under
// the exact eligibility rule from the plan's Global Constraints. The state
// values are read inside the query: nullif(value,”)::timestamptz makes an
// empty string mean unset (the summarizer clears backfill by writing ""). An
// unset summaries_enabled_at yields no candidates — the summarizer sets it on
// its first pass.
func (s *Store) EligibleForSummary(ctx context.Context, excluded []string, o recall.EligibleOpts) ([]recall.ConversationMeta, error) {
	if _, ok, err := s.GetState(ctx, "summaries_enabled_at"); err != nil {
		return nil, err
	} else if !ok {
		return nil, nil
	}
	limit := o.Limit
	if limit <= 0 {
		limit = recall.RecallDefaultLimit
	}

	var b sqlb
	enabledAt := `(SELECT nullif(value, '')::timestamptz FROM conversations.recall_state WHERE key = 'summaries_enabled_at')`
	backfillSince := `(SELECT nullif(value, '')::timestamptz FROM conversations.recall_state WHERE key = 'backfill_since')`
	maxOrdinal := `(SELECT max(m.ordinal) FROM conversations.conversation_message m WHERE m.conversation_id = c.id)`
	b.WriteString(`SELECT c.id::text, coalesce(c.owner_user_id::text, ''), coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			c.created_at, c.updated_at, ` + maxOrdinal + `, ` + lastActivitySQL + `,
			coalesce(` + stoppedSQL + `, false)
		FROM conversations.conversation c
		WHERE c.origin_entrypoint <> ALL(` + b.arg(excludedArg(excluded)) + `)
		  AND (c.closed_at IS NOT NULL
		       OR (NOT EXISTS (SELECT 1 FROM conversations.child ch
		                       WHERE (` + linkageSQL + `) AND ch.closed_at IS NULL)
		           AND ` + lastActivitySQL + ` < now() - ` + quietIntervalSQL + `))
		  AND (` + lastActivitySQL + ` >= ` + enabledAt + `
		       OR (` + backfillSince + ` IS NOT NULL AND ` + lastActivitySQL + ` >= ` + backfillSince + `))
		  AND NOT EXISTS (SELECT 1 FROM conversations.conversation_summary s
		                  WHERE s.conversation_id = c.id AND s.level = 'conversation'
		                    AND s.ordinal_to >= ` + maxOrdinal + `
		                    AND s.prompt_version = ` + b.arg(o.PromptVersion) + `)
		  AND NOT EXISTS (SELECT 1 FROM conversations.recall_summary_failure f
		                  WHERE f.conversation_id = c.id
		                    AND f.prompt_version = ` + b.arg(o.PromptVersion) + `
		                    AND f.model = ` + b.arg(o.Model) + `
		                    AND f.attempts >= ` + b.arg(recall.SummaryMaxFailures) + `)
		ORDER BY ` + lastActivitySQL + ` DESC
		LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.ConversationMeta
	for rows.Next() {
		c, err := scanConversationMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Summaries lists a conversation's summaries ordered by level, seq.
func (s *Store) Summaries(ctx context.Context, conversationID string) ([]recall.Summary, error) {
	uid, ok := parseID(conversationID)
	if !ok {
		return nil, recall.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT s.id::text, s.conversation_id::text, coalesce(s.owner_user_id::text, ''), s.level, s.seq,
			s.ordinal_from, s.ordinal_to, s.title, s.summary, s.prompt_version, s.model,
			s.input_tokens, s.output_tokens, s.total_cost_usd, s.created_at
		FROM conversations.conversation_summary s
		WHERE s.conversation_id = $1::uuid
		ORDER BY s.level, s.seq`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.Summary
	for rows.Next() {
		var sm recall.Summary
		if err := rows.Scan(&sm.ID, &sm.ConversationID, &sm.OwnerUserID, &sm.Level, &sm.Seq,
			&sm.OrdinalFrom, &sm.OrdinalTo, &sm.Title, &sm.Summary, &sm.PromptVersion, &sm.Model,
			&sm.InputTokens, &sm.OutputTokens, &sm.TotalCostUSD, &sm.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// UpsertSummary writes one summary; the conflict target is
// (conversation_id, level, seq) and an update accumulates cost and clears the
// embedding. A conversation-level write also clears the failure ledger row:
// a successful conversation summary is the strongest evidence the failure
// resolved.
func (s *Store) UpsertSummary(ctx context.Context, sm recall.Summary) error {
	uid, ok := parseID(sm.ConversationID)
	if !ok {
		return recall.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO conversations.conversation_summary
			(conversation_id, owner_user_id, level, seq, ordinal_from, ordinal_to,
			 title, summary, prompt_version, model, input_tokens, output_tokens, total_cost_usd)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (conversation_id, level, seq) DO UPDATE SET
			ordinal_from = EXCLUDED.ordinal_from,
			ordinal_to = EXCLUDED.ordinal_to,
			title = EXCLUDED.title,
			summary = EXCLUDED.summary,
			prompt_version = EXCLUDED.prompt_version,
			model = EXCLUDED.model,
			input_tokens = EXCLUDED.input_tokens,
			output_tokens = EXCLUDED.output_tokens,
			total_cost_usd = conversations.conversation_summary.total_cost_usd + EXCLUDED.total_cost_usd,
			embedding = NULL,
			embedding_model = NULL,
			created_at = now()`,
		uid, nullUUID(sm.OwnerUserID), sm.Level, sm.Seq, sm.OrdinalFrom, sm.OrdinalTo,
		sm.Title, sm.Summary, sm.PromptVersion, sm.Model, sm.InputTokens, sm.OutputTokens, sm.CostUSD); err != nil {
		return err
	}
	if sm.Level == "conversation" {
		if _, err := tx.Exec(ctx,
			`DELETE FROM conversations.recall_summary_failure WHERE conversation_id = $1::uuid`, uid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeleteSegmentsFrom drops segment summaries at or after fromSeq. Derived
// index data, so a hard delete is correct here (constraints: never
// hard-delete source rows).
func (s *Store) DeleteSegmentsFrom(ctx context.Context, conversationID string, fromSeq int) error {
	uid, ok := parseID(conversationID)
	if !ok {
		return recall.ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM conversations.conversation_summary
		WHERE conversation_id = $1::uuid AND level = 'segment' AND seq >= $2`, uid, fromSeq)
	return err
}

// RecordSummaryFailure counts one more failure per (conversation,
// prompt_version, model): a changed prompt_version or model resets attempts
// to 1.
func (s *Store) RecordSummaryFailure(ctx context.Context, conversationID string, promptVersion int, model, errText string) error {
	uid, ok := parseID(conversationID)
	if !ok {
		return recall.ErrNotFound
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO conversations.recall_summary_failure
			(conversation_id, prompt_version, model, attempts, last_error)
		VALUES ($1::uuid, $2, $3, 1, $4)
		ON CONFLICT (conversation_id) DO UPDATE SET
			attempts = CASE WHEN conversations.recall_summary_failure.prompt_version = EXCLUDED.prompt_version
			                 AND conversations.recall_summary_failure.model = EXCLUDED.model
			                THEN conversations.recall_summary_failure.attempts + 1 ELSE 1 END,
			prompt_version = EXCLUDED.prompt_version,
			model = EXCLUDED.model,
			last_error = EXCLUDED.last_error,
			updated_at = now()`,
		uid, promptVersion, model, errText)
	return err
}
