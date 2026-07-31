// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AnalysisRow is one conversations.conversation_analysis row: the result of
// running the analyze pipeline against a conversation under a given
// detector version, model and prompt hash.
type AnalysisRow struct {
	ID              string
	ConversationID  string
	DetectorVersion int
	Model           string
	Profile         string
	Status          string // ok | failed
	Error           string
	PromptHash      string
	Analysis        []byte // full analyze.Analysis JSON; nil when Status == "failed"
	InputTokens     int64
	OutputTokens    int64
	CostUSD         float64
	CreatedAt       time.Time
}

// FindingKey identifies a finding's topic within an analysis, independent of
// which analysis row (and so which finding id) it happens to live on across
// re-analyses: the (axis, topic_key) pair is what a human dismissal or
// action is really about.
type FindingKey struct {
	Axis     string
	TopicKey string
}

// UpsertAnalysis writes row, replacing any existing analysis at the same
// (conversation_id, detector_version, model, prompt_hash) key. The replace is
// delete-then-insert in one transaction rather than an ON CONFLICT UPDATE so
// the --force path always gets a fresh id (and, via the analysis_finding
// foreign key's ON DELETE CASCADE, its old findings): a caller can never end
// up holding a stale finding row pointed at a re-analyzed conversation.
//
// That cascade is also a trap: deleting the old analysis row silently drops
// any dismissed/actioned status a human had set on its findings, and the
// caller's subsequent ReplaceFindings re-inserts the re-detected findings as
// fresh 'open' rows — a --force re-analysis resurrects findings a human
// already triaged away. To let a caller avoid that, UpsertAnalysis captures
// each non-'open' finding's status at the same (axis, topic_key) under this
// conversation/detector_version/model/prompt_hash key *before* the delete,
// and returns it keyed by FindingKey so ReplaceFindings can carry the status
// forward onto the matching newly-detected finding.
func UpsertAnalysis(ctx context.Context, pool *pgxpool.Pool, row AnalysisRow) (id string, priorStatuses map[FindingKey]string, err error) {
	status := row.Status
	if status == "" {
		status = "ok"
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("upsert analysis: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	priorStatuses = make(map[FindingKey]string)
	rows, err := tx.Query(ctx, `
		SELECT af.axis, af.topic_key, af.status
		  FROM conversations.analysis_finding af
		  JOIN conversations.conversation_analysis ca ON ca.id = af.analysis_id
		 WHERE ca.conversation_id = $1::uuid AND ca.detector_version = $2 AND ca.model = $3 AND ca.prompt_hash = $4
		   AND af.status <> 'open'`,
		row.ConversationID, row.DetectorVersion, row.Model, row.PromptHash)
	if err != nil {
		return "", nil, fmt.Errorf("upsert analysis: prior statuses: %w", err)
	}
	for rows.Next() {
		var k FindingKey
		var s string
		if err := rows.Scan(&k.Axis, &k.TopicKey, &s); err != nil {
			rows.Close()
			return "", nil, fmt.Errorf("upsert analysis: prior statuses: scan: %w", err)
		}
		priorStatuses[k] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("upsert analysis: prior statuses: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM conversations.conversation_analysis
		 WHERE conversation_id = $1::uuid AND detector_version = $2 AND model = $3 AND prompt_hash = $4`,
		row.ConversationID, row.DetectorVersion, row.Model, row.PromptHash); err != nil {
		return "", nil, fmt.Errorf("upsert analysis: delete existing: %w", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO conversations.conversation_analysis
			(conversation_id, detector_version, model, profile, status, error, prompt_hash,
			 analysis, input_tokens, output_tokens, cost_usd)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id::text`,
		row.ConversationID, row.DetectorVersion, row.Model, nullify(row.Profile), status, nullify(row.Error),
		row.PromptHash, nullifyBytes(jsonbSafe(row.Analysis)), row.InputTokens, row.OutputTokens, row.CostUSD,
	).Scan(&id)
	if err != nil {
		return "", nil, fmt.Errorf("upsert analysis: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, fmt.Errorf("upsert analysis: commit: %w", err)
	}
	return id, priorStatuses, nil
}

// AnalyzedSet returns the subset of convIDs that already have an analysis row
// (status ok or failed — a failed attempt still counts as analyzed under that
// key, so a re-run doesn't retry it every time) at the given
// (detector_version, model, prompt_hash) key.
func AnalyzedSet(ctx context.Context, pool *pgxpool.Pool, convIDs []string, detectorVersion int, model, promptHash string) (map[string]bool, error) {
	out := make(map[string]bool, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT conversation_id::text FROM conversations.conversation_analysis
		 WHERE conversation_id = ANY($1::uuid[]) AND detector_version = $2 AND model = $3 AND prompt_hash = $4`,
		convIDs, detectorVersion, model, promptHash)
	if err != nil {
		return nil, fmt.Errorf("analyzed set: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("analyzed set: scan: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// FindingRow is one conversations.analysis_finding row. Tags use the same
// snake_case wire shape as agentcli.Summary/Progress, so `findings -j` and
// any other JSON-emitting path share one consistent finding representation.
type FindingRow struct {
	ID                    string `json:"id"`
	AnalysisID            string `json:"analysis_id"`
	Axis                  string `json:"axis"`
	TopicKey              string `json:"topic_key"`
	SkillName             string `json:"skill_name,omitempty"`
	Title                 string `json:"title"`
	ExpectedSavingsTokens int64  `json:"expected_savings_tokens"`
	Status                string `json:"status"`
}

// ReplaceFindings replaces analysisID's findings with rows, atomically: a
// delete of the existing set followed by a bulk insert, in one transaction.
// Called after every (re-)analysis, so a re-run's findings never accumulate
// alongside a prior run's stale ones.
//
// prior carries forward a human's triage decision across that replace: when
// a row's (Axis, TopicKey) is a key in prior, the row is inserted with
// prior's status instead of its own (zero value == 'open') — otherwise a
// --force re-analysis of a conversation would resurrect a
// dismissed/actioned finding as 'open' the moment it's re-detected, since
// UpsertAnalysis's delete-then-insert of the analysis row cascades away the
// old finding row (and the status that lived on it) before this call ever
// runs. Pass nil for no carry-over (e.g. a first-time analysis, where
// nothing prior can exist).
func ReplaceFindings(ctx context.Context, pool *pgxpool.Pool, analysisID string, rows []FindingRow, prior map[FindingKey]string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace findings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `DELETE FROM conversations.analysis_finding WHERE analysis_id = $1::uuid`, analysisID); err != nil {
		return fmt.Errorf("replace findings: delete existing: %w", err)
	}
	for _, r := range rows {
		status := r.Status
		if status == "" {
			status = "open"
		}
		if carried, ok := prior[FindingKey{Axis: r.Axis, TopicKey: r.TopicKey}]; ok {
			status = carried
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO conversations.analysis_finding
				(analysis_id, axis, topic_key, skill_name, title, expected_savings_tokens, status)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
			analysisID, r.Axis, r.TopicKey, nullify(r.SkillName), r.Title, r.ExpectedSavingsTokens, status); err != nil {
			return fmt.Errorf("replace findings: insert %q: %w", r.TopicKey, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace findings: commit: %w", err)
	}
	return nil
}

// FindingFilter narrows ListFindings. A zero-value FindingFilter lists open
// findings across every axis and skill.
type FindingFilter struct {
	Axis   string
	Skill  string
	Status string
}

// ListFindings returns findings matching f, most-impactful first
// (expected_savings_tokens DESC, then by the joined analysis's created_at
// DESC as a tiebreak — the finding's own created_at is nearly identical
// across a whole ReplaceFindings batch, so it doesn't discriminate; the
// analysis's recency does). Status defaults to "open" when f.Status is
// empty — dismissed/actioned findings stay out of the default view.
func ListFindings(ctx context.Context, pool *pgxpool.Pool, f FindingFilter) ([]FindingRow, error) {
	status := f.Status
	if status == "" {
		status = "open"
	}
	rows, err := pool.Query(ctx, `
		SELECT af.id::text, af.analysis_id::text, af.axis, af.topic_key,
		       coalesce(af.skill_name, ''), af.title, af.expected_savings_tokens, af.status
		  FROM conversations.analysis_finding af
		  JOIN conversations.conversation_analysis ca ON ca.id = af.analysis_id
		 WHERE af.status = $1
		   AND ($2 = '' OR af.axis = $2)
		   AND ($3 = '' OR af.skill_name = $3)
		 ORDER BY af.expected_savings_tokens DESC, ca.created_at DESC`,
		status, f.Axis, f.Skill)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()
	var out []FindingRow
	for rows.Next() {
		var r FindingRow
		if err := rows.Scan(&r.ID, &r.AnalysisID, &r.Axis, &r.TopicKey, &r.SkillName, &r.Title, &r.ExpectedSavingsTokens, &r.Status); err != nil {
			return nil, fmt.Errorf("list findings: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var findingStatuses = map[string]bool{"open": true, "dismissed": true, "actioned": true}

// SetFindingStatus updates one finding's status, validating the enum in Go
// (rather than relying on a CHECK constraint) so a bad value is a clear Go
// error rather than a raw constraint-violation from Postgres. It returns the
// updated row (via RETURNING) so a caller that wants to echo the change back
// doesn't need a second round-trip that lists every finding and scans for
// the one it just touched.
func SetFindingStatus(ctx context.Context, pool *pgxpool.Pool, id, status string) (FindingRow, error) {
	if !findingStatuses[status] {
		return FindingRow{}, fmt.Errorf("set finding status: invalid status %q (want open, dismissed or actioned)", status)
	}
	var r FindingRow
	err := pool.QueryRow(ctx, `
		UPDATE conversations.analysis_finding SET status = $2 WHERE id = $1::uuid
		RETURNING id::text, analysis_id::text, axis, topic_key, coalesce(skill_name, ''), title,
		          expected_savings_tokens, status`,
		id, status,
	).Scan(&r.ID, &r.AnalysisID, &r.Axis, &r.TopicKey, &r.SkillName, &r.Title, &r.ExpectedSavingsTokens, &r.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return FindingRow{}, fmt.Errorf("set finding status: no finding with id %q", id)
		}
		return FindingRow{}, fmt.Errorf("set finding status: %w", err)
	}
	return r, nil
}

func nullify(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullifyBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// jsonbSafe makes a JSON value storable in a Postgres jsonb column, which
// cannot represent U+0000: a `\u0000` escape triggers SQLSTATE 22P05 on
// insert. Analysis payloads are LLM output over arbitrary captured
// conversation text, so strip U+0000 from every string. Values without the
// escape are returned verbatim — only NUL-bearing content pays the
// decode/re-encode. Invalid JSON is returned unchanged so the insert surfaces
// the real error.
func jsonbSafe(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u0000`)) {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // preserve integer precision through the round-trip
	var v any
	if err := dec.Decode(&v); err != nil {
		return b
	}
	out, err := json.Marshal(stripNUL(v))
	if err != nil {
		return b
	}
	return out
}

// stripNUL recursively removes U+0000 from every string (and map key) in a
// decoded JSON value. json.Number and other scalars pass through unchanged.
func stripNUL(v any) any {
	switch t := v.(type) {
	case string:
		if strings.IndexByte(t, 0) < 0 {
			return t
		}
		return strings.ReplaceAll(t, "\x00", "")
	case []any:
		for i, e := range t {
			t[i] = stripNUL(e)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[strings.ReplaceAll(k, "\x00", "")] = stripNUL(e)
		}
		return out
	default:
		return v
	}
}
