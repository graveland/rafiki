package recall

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SummarizerOptions configures a Summarizer.
type SummarizerOptions struct {
	Store            Store
	Completer        Completer
	MaxSegmentTokens int // 0 = derive only
	Logger           *slog.Logger
	Now              func() time.Time // nil = time.Now
}

// Summarizer runs the rolling conversation-summary pass: segments of
// extracted messages are summarized in order, each rolling the previous
// segment's summary forward, then reduced to one conversation-level row.
type Summarizer struct {
	store       Store
	completer   Completer
	maxTokens   int
	logger      *slog.Logger
	now         func() time.Time
	unknownOnce sync.Once
}

// NewSummarizer builds a Summarizer.
func NewSummarizer(o SummarizerOptions) *Summarizer {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Summarizer{
		store:     o.Store,
		completer: o.Completer,
		maxTokens: o.MaxSegmentTokens,
		logger:    logger,
		now:       now,
	}
}

// Pass runs one summarizer pass. The first pass only records the
// summaries_enabled_at state; afterwards each eligible conversation gets new
// segments summarized, rolled onto the stored chain, and reduced to a
// conversation-level row. Failures are recorded per conversation and never
// stop the pass; context cancellation aborts without recording a failure.
func (s *Summarizer) Pass(ctx context.Context) error {
	enabled, ok, err := stateValue(ctx, s.store, "summaries_enabled_at")
	if err != nil {
		return err
	}
	if !ok {
		return s.store.SetState(ctx, "summaries_enabled_at", s.now().UTC().Format(time.RFC3339))
	}
	enabledAt, err := time.Parse(time.RFC3339, enabled)
	if err != nil {
		enabledAt = time.Time{} // unparseable: never treat work as backfill
	}
	if err := s.clearSpentBackfill(ctx); err != nil {
		return err
	}
	convs, err := s.store.EligibleForSummary(ctx, ExcludedEntrypoints, EligibleOpts{
		PromptVersion: SummaryPromptVersion,
		Model:         s.completer.Model(),
		Limit:         5,
	})
	if err != nil {
		return err
	}
	for _, conv := range convs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.summarizeConversation(ctx, enabledAt, conv); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.recordFailure(ctx, conv.ID, err)
			continue
		}
	}
	return nil
}

// SegmentBudgetTokens is the per-segment prompt budget in tokens: 90% of the
// model's context window minus summary overhead and output, capped by
// MaxSegmentTokens, floored at 4000.
func (s *Summarizer) SegmentBudgetTokens() int {
	tokens, ok := s.completer.ContextWindow()
	if !ok {
		tokens = UnknownContextTokens
		s.unknownOnce.Do(func() {
			s.logger.Warn("recall summaries: model context window unknown; using fallback",
				"model", s.completer.Model(), "tokens", UnknownContextTokens)
		})
	}
	budget := int(float64(tokens-SummaryPromptOverheadTokens-SummaryMaxOutputTokens) * 0.9)
	if s.maxTokens > 0 && s.maxTokens < budget {
		budget = s.maxTokens
	}
	if budget < 4000 {
		budget = 4000
	}
	return budget
}

// summarizeConversation summarizes one eligible conversation: extend the
// segment chain from the last current-version segment, then reduce the chain
// into the conversation-level row. Backfill work (last activity before
// summaries_enabled_at) adds this pass's cost to backfill_spent_usd.
func (s *Summarizer) summarizeConversation(ctx context.Context, enabledAt time.Time, c ConversationMeta) error {
	existing, err := s.store.Summaries(ctx, c.ID)
	if err != nil {
		return err
	}
	kept := make([]Summary, 0, len(existing))
	stale := false
	for _, sum := range existing {
		if sum.Level != "segment" {
			continue
		}
		if sum.PromptVersion != SummaryPromptVersion {
			stale = true
			continue
		}
		kept = append(kept, sum)
	}
	if stale {
		if err := s.store.DeleteSegmentsFrom(ctx, c.ID, 0); err != nil {
			return err
		}
		kept = nil
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Seq < kept[j].Seq })

	msgs, err := s.store.MessagesFrom(ctx, c.ID, fromOrdinal(kept))
	if err != nil {
		return err
	}
	segs := s.buildSegments(msgs)
	if len(segs) == 0 {
		if len(kept) == 0 {
			return nil
		}
		// No new segments: re-reducing an unchanged chain costs one reduce call
		// plus an embedding clear/re-embed every pass. Skip only when a
		// current-version conversation-level row already COVERS the chain; a
		// missing, stale, or short row (a pass that upserted segments but
		// crashed before the row upsert) still reduces.
		if hasCurrentConversationRow(existing, kept[len(kept)-1].OrdinalTo) {
			return nil
		}
	}

	spent := 0.0
	base := kept
	var prev string
	if len(kept) > 0 {
		prev = kept[len(kept)-1].Summary
	}
	chain := append([]Summary{}, kept...)
	for i, seg := range segs {
		if err := ctx.Err(); err != nil {
			return err
		}
		completion, err := s.completer.Complete(ctx, c.OwnerUserID, summarySegmentPrompt, rollingUserPrompt(prev, seg.text), SummaryMaxOutputTokens)
		if err != nil {
			return err
		}
		title, body := parseSummaryOutput(completion.Text)
		sum := Summary{
			ConversationID: c.ID,
			OwnerUserID:    c.OwnerUserID,
			Level:          "segment",
			Seq:            nextSeq(base, i),
			OrdinalFrom:    seg.from,
			OrdinalTo:      seg.to,
			Title:          title,
			Summary:        body,
			PromptVersion:  SummaryPromptVersion,
			Model:          completion.Model,
			InputTokens:    completion.InputTokens,
			OutputTokens:   completion.OutputTokens,
			CostUSD:        completion.CostUSD,
		}
		if err := s.store.UpsertSummary(ctx, sum); err != nil {
			return err
		}
		spent += completion.CostUSD
		chain = append(chain, sum)
		prev = sum.Summary
	}

	if len(chain) == 1 {
		if err := s.store.UpsertSummary(ctx, conversationRow(chain[0])); err != nil {
			return err
		}
	} else {
		if err := ctx.Err(); err != nil {
			return err
		}
		reduction, err := s.completer.Complete(ctx, c.OwnerUserID, summaryReducePrompt, reduceUserPrompt(chain), SummaryMaxOutputTokens)
		if err != nil {
			return err
		}
		title, body := parseSummaryOutput(reduction.Text)
		sum := Summary{
			ConversationID: c.ID,
			OwnerUserID:    c.OwnerUserID,
			Level:          "conversation",
			Seq:            0,
			OrdinalFrom:    chain[0].OrdinalFrom,
			OrdinalTo:      chain[len(chain)-1].OrdinalTo,
			Title:          title,
			Summary:        body,
			PromptVersion:  SummaryPromptVersion,
			Model:          reduction.Model,
			InputTokens:    reduction.InputTokens,
			OutputTokens:   reduction.OutputTokens,
			CostUSD:        reduction.CostUSD,
		}
		if err := s.store.UpsertSummary(ctx, sum); err != nil {
			return err
		}
		spent += reduction.CostUSD
	}
	if c.LastAt.Before(enabledAt) {
		if err := s.store.AddState(ctx, "backfill_spent_usd", spent); err != nil {
			return err
		}
	}
	return nil
}

// hasCurrentConversationRow reports whether existing already holds a
// conversation-level row at the current prompt version whose OrdinalTo
// covers the kept chain (>= coveredTo). A current-version row that stops
// short means a previous pass upserted segments but crashed before the row
// upsert; treating it as current would skip the healing reduce forever.
func hasCurrentConversationRow(existing []Summary, coveredTo int) bool {
	for _, sum := range existing {
		if sum.Level == "conversation" && sum.PromptVersion == SummaryPromptVersion && sum.OrdinalTo >= coveredTo {
			return true
		}
	}
	return false
}

// clearSpentBackfill resets backfill once its budget is spent; "" reads as
// unset everywhere.
func (s *Summarizer) clearSpentBackfill(ctx context.Context) error {
	_, ok, err := stateValue(ctx, s.store, "backfill_since")
	if err != nil || !ok {
		return err
	}
	spent := stateDecimal(ctx, s.store, "backfill_spent_usd")
	budgetRaw, ok, err := stateValue(ctx, s.store, "backfill_budget_usd")
	if err != nil {
		return err
	}
	budget, parseErr := strconv.ParseFloat(budgetRaw, 64)
	if !ok || parseErr != nil {
		// backfill_since is armed but the budget is missing or unparseable, so
		// it reads as 0 and this branch disables backfill; surface the
		// operator's mistake instead of failing silently.
		s.logger.Warn("recall summaries: backfill active without backfill_budget_usd; disabling backfill")
		budget = 0
	}
	if spent >= budget {
		return s.store.SetState(ctx, "backfill_since", "")
	}
	return nil
}

// recordFailure notes one conversation's failure on the summary-failure
// ledger; a recording error is logged, never masked.
func (s *Summarizer) recordFailure(ctx context.Context, conversationID string, cause error) {
	if err := s.store.RecordSummaryFailure(ctx, conversationID, SummaryPromptVersion, s.completer.Model(), cause.Error()); err != nil {
		s.logger.Error("recall summaries: recording failure failed", "conversation", conversationID, "err", err)
	}
	s.logger.Warn("recall summaries: conversation summary failed", "conversation", conversationID, "err", cause)
}

// fromOrdinal is where new segments start: one past the newest kept segment.
func fromOrdinal(kept []Summary) int {
	if len(kept) == 0 {
		return 0
	}
	return kept[len(kept)-1].OrdinalTo + 1
}

// nextSeq is the Seq of the written-th new segment after the kept chain.
func nextSeq(kept []Summary, written int) int {
	if len(kept) == 0 {
		return written
	}
	return kept[len(kept)-1].Seq + 1 + written
}

// conversationRow copies a lone segment into the conversation-level row; no
// reduce call happens, so the copy costs nothing.
func conversationRow(seg Summary) Summary {
	seg.Level = "conversation"
	seg.Seq = 0
	seg.CostUSD = 0
	seg.ID = "" // the INSERT assigns its own uuid; honoring Summary.ID on upsert is a latent trap
	return seg
}

// segment is one slice of extracted conversation text to summarize.
type segment struct {
	from, to int
	text     string
}

// buildSegments slices messages into segments of at most one segment budget's
// worth of characters (runes), cutting at every compaction summary and
// truncating a single over-budget message to the budget.
func (s *Summarizer) buildSegments(msgs []Message) []segment {
	budgetChars := s.SegmentBudgetTokens() * CharsPerToken
	extracted := ExtractAll(msgs)
	var segs []segment
	var cur []string
	curChars, from, to := 0, 0, 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		segs = append(segs, segment{from: from, to: to, text: strings.Join(cur, "\n\n")})
		cur, curChars = nil, 0
	}
	for i, em := range extracted {
		if msgs[i].Kind == "compaction_summary" {
			flush()
			continue
		}
		if em.Skip {
			continue
		}
		n := utf8.RuneCountInString(em.Text)
		if n > budgetChars {
			flush()
			segs = append(segs, segment{
				from: em.Ordinal, to: em.Ordinal,
				text: splitRunes(em.Text, budgetChars)[0] + "\n[… truncated …]",
			})
			continue
		}
		if len(cur) > 0 && curChars+n > budgetChars {
			flush()
		}
		if len(cur) == 0 {
			from = em.Ordinal
		}
		cur = append(cur, em.Text)
		curChars += n
		to = em.Ordinal
	}
	flush()
	return segs
}

// parseSummaryOutput splits model output into title and body: the first line
// "TITLE: <title>", the rest the summary; no TITLE line titles the summary's
// first 80 runes.
func parseSummaryOutput(out string) (title, body string) {
	s := strings.TrimSpace(out)
	if strings.HasPrefix(s, "TITLE:") {
		line, rest, _ := strings.Cut(s, "\n")
		return strings.TrimSpace(strings.TrimPrefix(line, "TITLE:")), strings.TrimSpace(rest)
	}
	return firstRunes(s, 80), s
}

// firstRunes cuts s to at most n runes.
func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return splitRunes(s, n)[0]
}

// rollingUserPrompt builds one segment's user prompt: the story so far when
// there is one, then the next part of the conversation.
func rollingUserPrompt(storySoFar, segmentText string) string {
	if storySoFar == "" {
		return "Next part of the conversation:\n" + segmentText
	}
	return "Story so far:\n" + storySoFar + "\n\n---\n\nNext part of the conversation:\n" + segmentText
}

// reduceUserPrompt joins chain summaries as "Part N:" blocks.
func reduceUserPrompt(chain []Summary) string {
	parts := make([]string, len(chain))
	for i, sum := range chain {
		parts[i] = fmt.Sprintf("Part %d: %s", i+1, sum.Summary)
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// stateValue reads a state key; "" counts as unset everywhere.
func stateValue(ctx context.Context, store Store, key string) (string, bool, error) {
	v, ok, err := store.GetState(ctx, key)
	if err != nil || !ok || v == "" {
		return "", false, err
	}
	return v, true, nil
}

// stateDecimal reads a decimal state value; missing or unparseable is 0.
func stateDecimal(ctx context.Context, store Store, key string) float64 {
	v, ok, err := stateValue(ctx, store, key)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return n
}
