package insights

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// cacheWasteInputThreshold is the input-token floor above which a turn that saw
// zero cache_read is counted as cache waste: a large prompt that should have hit
// a warm prefix cache but didn't.
const cacheWasteInputThreshold int64 = 4096

// StatsFilter narrows the population for GlobalStats. Zero-value fields are ignored.
type StatsFilter struct {
	Since, Until *time.Time
	Owner        string
	Persona      string
	Source       string
	Model        string
	Path         Path
}

// Stats is the aggregate bundle. Every facet is computed over the same filtered
// population; ByPath additionally splits the token facet by capture path.
type Stats struct {
	Volume     VolumeStats           `json:"volume"`
	Adoption   AdoptionStats         `json:"adoption"`
	Tokens     TokenStats            `json:"tokens"`
	Cost       []CostRow             `json:"cost"`
	Failures   FailureStats          `json:"failures"`
	Latency    LatencyStats          `json:"latency"`
	CacheWaste CacheWasteStats       `json:"cache_waste"`
	Prefix     PrefixStats           `json:"prefix"`
	ByPath     map[string]TokenStats `json:"by_path"` // keyed by "proxy"/"direct"
}

type VolumeStats struct {
	Conversations int64 `json:"conversations"`
	Turns         int64 `json:"turns"`
}

type AdoptionStats struct {
	DistinctOwners int64        `json:"distinct_owners"`
	PerOwner       []OwnerCount `json:"per_owner"`
}

type OwnerCount struct {
	Owner         string `json:"owner"`
	Conversations int64  `json:"conversations"`
	Turns         int64  `json:"turns"`
}

type TokenStats struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheHitRatio       float64 `json:"cache_hit_ratio"` // cache_read / (input + cache_read)
}

// CostRow is a per-model token rollup. CostUSD is best-effort: it is 0 until a
// per-model price table is wired in, so callers should treat 0 as "unpriced".
type CostRow struct {
	Model               string  `json:"model"`
	Turns               int64   `json:"turns"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CostUSD             float64 `json:"cost_usd"`
}

type FailureStats struct {
	Turns        int64   `json:"turns"`
	Errors       int64   `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	FailoverRate float64 `json:"failover_rate"` // fraction of turns served by the openrouter upstream
}

type LatencyStats struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type CacheWasteStats struct {
	WastedTurns       int64 `json:"wasted_turns"`
	WastedInputTokens int64 `json:"wasted_input_tokens"`
	Threshold         int64 `json:"threshold"`
}

// PrefixStats describes cache-prefix reuse. Cross-conversation facets
// (CrossUserPrefixes) are only meaningful for GlobalStats.
type PrefixStats struct {
	DistinctPrefixes     int64   `json:"distinct_prefixes"`
	TurnsWithPrefix      int64   `json:"turns_with_prefix"`
	ReuseRatio           float64 `json:"reuse_ratio"`           // turns-with-prefix / distinct-prefixes
	CrossUserPrefixes    int64   `json:"cross_user_prefixes"`   // prefixes reused across more than one owner
	DriftedConversations int64   `json:"drifted_conversations"` // conversations whose prefix_hash changed across turns
}

// statsScope is a shared WHERE clause plus its args over the turn⋈conversation
// join. Both entry points build one and hand it to compute.
type statsScope struct {
	where  string
	args   []any
	global bool
}

const statsFrom = `FROM conversations.conversation_turn t
	JOIN conversations.conversation c ON c.id = t.conversation_id`

// pathForDrivenBy labels a driven_by value with the Path string used in
// per-path result maps ("proxy"/"direct"); unknown values pass through so an
// unexpected driven_by stays visible.
func pathForDrivenBy(drivenBy string) string {
	switch drivenBy {
	case "client":
		return string(PathProxy)
	case "server":
		return string(PathDirect)
	default:
		return drivenBy
	}
}

// GlobalStats computes the aggregate bundle over conversations matching f.
func (i *Insights) GlobalStats(ctx context.Context, f StatsFilter) (*Stats, error) {
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
	if f.Model != "" {
		conds = append(conds, "t.model = "+a.next(f.Model))
	}
	if f.Source != "" {
		conds = append(conds, "t.source = "+a.next(f.Source))
	}
	if f.Since != nil {
		conds = append(conds, "t.created_at >= "+a.next(*f.Since))
	}
	if f.Until != nil {
		conds = append(conds, "t.created_at < "+a.next(*f.Until))
	}
	return i.compute(ctx, statsScope{where: strings.Join(conds, " AND "), args: a.args, global: true})
}

// ConversationStats computes the same bundle scoped to one conversation, without
// the cross-conversation prefix analytics.
func (i *Insights) ConversationStats(ctx context.Context, conversationID string) (*Stats, error) {
	var a argList
	where := "c.id = " + a.next(conversationID) + "::uuid"
	return i.compute(ctx, statsScope{where: where, args: a.args, global: false})
}

func (i *Insights) compute(ctx context.Context, sc statsScope) (*Stats, error) {
	s := &Stats{ByPath: map[string]TokenStats{}, CacheWaste: CacheWasteStats{Threshold: cacheWasteInputThreshold}}

	if err := i.volume(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.adoption(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.tokens(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.cost(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.failures(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.latency(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.cacheWaste(ctx, sc, s); err != nil {
		return nil, err
	}
	if err := i.prefix(ctx, sc, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (i *Insights) volume(ctx context.Context, sc statsScope, s *Stats) error {
	err := i.pool.QueryRow(ctx,
		`SELECT count(DISTINCT t.conversation_id), count(t.id) `+statsFrom+` WHERE `+sc.where,
		sc.args...).Scan(&s.Volume.Conversations, &s.Volume.Turns)
	if err != nil {
		return fmt.Errorf("stats: volume: %w", err)
	}
	return nil
}

func (i *Insights) adoption(ctx context.Context, sc statsScope, s *Stats) error {
	if err := i.pool.QueryRow(ctx,
		`SELECT count(DISTINCT c.owner) `+statsFrom+` WHERE `+sc.where,
		sc.args...).Scan(&s.Adoption.DistinctOwners); err != nil {
		return fmt.Errorf("stats: distinct owners: %w", err)
	}
	rows, err := i.pool.Query(ctx,
		`SELECT coalesce(c.owner,''), count(DISTINCT c.id), count(t.id) `+statsFrom+`
		 WHERE `+sc.where+`
		 GROUP BY c.owner ORDER BY count(t.id) DESC, c.owner LIMIT 100`, sc.args...)
	if err != nil {
		return fmt.Errorf("stats: per-owner: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oc OwnerCount
		if err := rows.Scan(&oc.Owner, &oc.Conversations, &oc.Turns); err != nil {
			return fmt.Errorf("stats: scan per-owner: %w", err)
		}
		s.Adoption.PerOwner = append(s.Adoption.PerOwner, oc)
	}
	return rows.Err()
}

func scanTokens(dst *TokenStats) []any {
	return []any{&dst.InputTokens, &dst.OutputTokens, &dst.CacheReadTokens, &dst.CacheCreationTokens}
}

func (t *TokenStats) finalize() {
	if denom := t.InputTokens + t.CacheReadTokens; denom > 0 {
		t.CacheHitRatio = float64(t.CacheReadTokens) / float64(denom)
	}
}

const tokenSums = `coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
	coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_creation_tokens),0)`

func (i *Insights) tokens(ctx context.Context, sc statsScope, s *Stats) error {
	if err := i.pool.QueryRow(ctx,
		`SELECT `+tokenSums+` `+statsFrom+` WHERE `+sc.where,
		sc.args...).Scan(scanTokens(&s.Tokens)...); err != nil {
		return fmt.Errorf("stats: tokens: %w", err)
	}
	s.Tokens.finalize()

	rows, err := i.pool.Query(ctx,
		`SELECT c.driven_by, `+tokenSums+` `+statsFrom+`
		 WHERE `+sc.where+` GROUP BY c.driven_by`, sc.args...)
	if err != nil {
		return fmt.Errorf("stats: tokens by path: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			drivenBy string
			ts       TokenStats
		)
		dst := append([]any{&drivenBy}, scanTokens(&ts)...)
		if err := rows.Scan(dst...); err != nil {
			return fmt.Errorf("stats: scan tokens by path: %w", err)
		}
		ts.finalize()
		s.ByPath[pathForDrivenBy(drivenBy)] = ts
	}
	return rows.Err()
}

func (i *Insights) cost(ctx context.Context, sc statsScope, s *Stats) error {
	rows, err := i.pool.Query(ctx,
		`SELECT coalesce(t.model,''), count(t.id), `+tokenSums+` `+statsFrom+`
		 WHERE `+sc.where+`
		 GROUP BY t.model ORDER BY sum(input_tokens + output_tokens) DESC NULLS LAST`, sc.args...)
	if err != nil {
		return fmt.Errorf("stats: cost: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r CostRow
		if err := rows.Scan(&r.Model, &r.Turns,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens); err != nil {
			return fmt.Errorf("stats: scan cost: %w", err)
		}
		s.Cost = append(s.Cost, r)
	}
	return rows.Err()
}

func (i *Insights) failures(ctx context.Context, sc statsScope, s *Stats) error {
	err := i.pool.QueryRow(ctx,
		`SELECT count(t.id), count(t.id) FILTER (WHERE t.status='error'),
		        coalesce(avg((t.upstream='openrouter')::int), 0) `+statsFrom+`
		 WHERE `+sc.where, sc.args...).Scan(&s.Failures.Turns, &s.Failures.Errors, &s.Failures.FailoverRate)
	if err != nil {
		return fmt.Errorf("stats: failures: %w", err)
	}
	if s.Failures.Turns > 0 {
		s.Failures.ErrorRate = float64(s.Failures.Errors) / float64(s.Failures.Turns)
	}
	return nil
}

func (i *Insights) latency(ctx context.Context, sc statsScope, s *Stats) error {
	err := i.pool.QueryRow(ctx,
		`SELECT coalesce(percentile_cont(0.5)  WITHIN GROUP (ORDER BY t.latency_ms), 0),
		        coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY t.latency_ms), 0),
		        coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY t.latency_ms), 0) `+statsFrom+`
		 WHERE `+sc.where+` AND t.latency_ms IS NOT NULL`,
		sc.args...).Scan(&s.Latency.P50, &s.Latency.P95, &s.Latency.P99)
	if err != nil {
		return fmt.Errorf("stats: latency: %w", err)
	}
	return nil
}

func (i *Insights) cacheWaste(ctx context.Context, sc statsScope, s *Stats) error {
	args := append(slices.Clone(sc.args), cacheWasteInputThreshold)
	threshold := fmt.Sprintf("$%d", len(args))
	err := i.pool.QueryRow(ctx,
		`SELECT count(t.id) FILTER (WHERE coalesce(t.cache_read_tokens,0)=0 AND coalesce(t.input_tokens,0) > `+threshold+`),
		        coalesce(sum(t.input_tokens) FILTER (WHERE coalesce(t.cache_read_tokens,0)=0 AND coalesce(t.input_tokens,0) > `+threshold+`), 0) `+statsFrom+`
		 WHERE `+sc.where, args...).Scan(&s.CacheWaste.WastedTurns, &s.CacheWaste.WastedInputTokens)
	if err != nil {
		return fmt.Errorf("stats: cache waste: %w", err)
	}
	return nil
}

func (i *Insights) prefix(ctx context.Context, sc statsScope, s *Stats) error {
	err := i.pool.QueryRow(ctx,
		`SELECT count(DISTINCT t.prefix_hash), count(t.id) FILTER (WHERE t.prefix_hash IS NOT NULL) `+statsFrom+`
		 WHERE `+sc.where+` AND t.prefix_hash IS NOT NULL`,
		sc.args...).Scan(&s.Prefix.DistinctPrefixes, &s.Prefix.TurnsWithPrefix)
	if err != nil {
		return fmt.Errorf("stats: prefix distinct: %w", err)
	}
	if s.Prefix.DistinctPrefixes > 0 {
		s.Prefix.ReuseRatio = float64(s.Prefix.TurnsWithPrefix) / float64(s.Prefix.DistinctPrefixes)
	}

	// Conversations whose prefix_hash changed across turns (prefix drift).
	if err := i.pool.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT t.conversation_id `+statsFrom+`
		   WHERE `+sc.where+` AND t.prefix_hash IS NOT NULL
		   GROUP BY t.conversation_id HAVING count(DISTINCT t.prefix_hash) > 1
		 ) drift`, sc.args...).Scan(&s.Prefix.DriftedConversations); err != nil {
		return fmt.Errorf("stats: prefix drift: %w", err)
	}

	// Cross-user prefix reuse only makes sense across conversations (global).
	if sc.global {
		if err := i.pool.QueryRow(ctx,
			`SELECT count(*) FROM (
			   SELECT t.prefix_hash `+statsFrom+`
			   WHERE `+sc.where+` AND t.prefix_hash IS NOT NULL
			   GROUP BY t.prefix_hash HAVING count(DISTINCT c.owner) > 1
			 ) x`, sc.args...).Scan(&s.Prefix.CrossUserPrefixes); err != nil {
			return fmt.Errorf("stats: cross-user prefix: %w", err)
		}
	}
	return nil
}
