package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.graveland.dev/brent/rafiki/agentcli"
	"git.graveland.dev/brent/rafiki/analyze"
	"git.graveland.dev/brent/rafiki/insights"
	"git.graveland.dev/brent/rafiki/store"
)

// analyzeItem is one conversation this run will consider processing.
// transcript is nil for a DB-backed item (Export fetches it lazily in
// analyzeOne) and non-nil for a corpus item (already loaded from disk).
// A non-nil transcript is also the signal that this item has no backing
// conversations.conversation row: nothing exists for an analysis_finding row
// to foreign-key against, so analyzeOne treats it as forced NoStore
// regardless of the request's own NoStore flag.
type analyzeItem struct {
	id         string
	transcript *insights.Transcript
}

// analyzedConversation is one conversation that finished Detect successfully
// in THIS run (not one loaded from storage as already-analyzed): enough to
// feed analyze.Rank and, for DB-backed conversations, to call
// store.ReplaceFindings afterward.
type analyzedConversation struct {
	conversationID string
	analysis       *analyze.Analysis
	analysisID     string // "" when noStore — nothing to attach findings to
	prior          map[store.FindingKey]string
	noStore        bool
}

// runAnalyze is the Analyze goroutine body: it owns ch end-to-end (always
// closes it, exactly once, via defer) and turns any fatal error into a
// single terminal EventError rather than a panic or a silently-truncated
// channel.
func (b *Backend) runAnalyze(ctx context.Context, req agentcli.AnalyzeRequest, profile *analyze.Profile, ch chan<- agentcli.AnalyzeEvent) {
	defer close(ch)
	// Recovers a panic anywhere below into a terminal EventError instead of
	// letting it crash the producer goroutine and leave the consumer's range
	// over ch hanging until the runtime kills the process. Registered after
	// defer close(ch) above so — per Go's LIFO defer order — this one runs
	// FIRST on the way out: the EventError is always sent before the channel
	// closes, matching AnalyzeEvent's doc contract that EventError is the
	// last event and close follows immediately after.
	defer func() {
		if r := recover(); r != nil {
			ch <- agentcli.AnalyzeEvent{Kind: agentcli.EventError, Err: fmt.Errorf("agentcli/local: analyze: panic: %v", r)}
		}
	}()

	// send reports whether the event was delivered; false means the
	// consumer's ctx was cancelled (the only reason the buffered send
	// below can block indefinitely), and every call site treats that as
	// "stop the batch now." send is only ever used for non-terminal
	// (progress/analysis) events, which are fine to drop on a cancelled ctx —
	// the caller is walking away regardless.
	send := func(ev agentcli.AnalyzeEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// sendTerminal delivers EventSummary/EventError unconditionally: these
	// are the run's single terminal event, and a select racing ctx.Done()
	// against the channel send (as send above does) picks uniformly among
	// ready cases — with a cancelled ctx, that raced away the terminal event
	// close to half the time in practice (measured ~105/200), leaving a
	// consumer's range over ch see a channel close with no EventError/Summary
	// at all, violating the "last event is always terminal" contract. A
	// plain blocking send is safe here: ch is closed exactly once, by the
	// defer above, strictly after this call returns.
	sendTerminal := func(ev agentcli.AnalyzeEvent) { ch <- ev }
	fail := func(err error) { sendTerminal(agentcli.AnalyzeEvent{Kind: agentcli.EventError, Err: err}) }

	items, populationCount, unparseable, err := b.population(ctx, req, profile, send)
	if err != nil {
		fail(err)
		return
	}

	promptHash := profile.PromptHash()

	// Corpus items (transcript != nil) have no conversation row, so nothing
	// backs an AnalyzedSet lookup keyed on their id — only DB-backed items
	// participate in skip-detection.
	var dbIDs []string
	for _, it := range items {
		if it.transcript == nil {
			dbIDs = append(dbIDs, it.id)
		}
	}

	analyzed := map[string]bool{}
	if !req.Force && len(dbIDs) > 0 {
		var aerr error
		analyzed, aerr = store.AnalyzedSet(ctx, b.pool, dbIDs, analyze.DetectorVersion, profile.DetectorModel, promptHash)
		if aerr != nil {
			fail(fmt.Errorf("agentcli/local: analyzed set: %w", aerr))
			return
		}
	}

	var toProcess []analyzeItem
	var skippedIDs []string
	for _, it := range items {
		if it.transcript == nil && analyzed[it.id] {
			skippedIDs = append(skippedIDs, it.id)
			if !send(progressEvent(it.id, agentcli.StateSkipped, "", 0, 0, 0)) {
				return
			}
			continue
		}
		toProcess = append(toProcess, it)
	}
	// unparseable (corpus files that failed to read/decode) already got their
	// own StateSkipped progress event from corpusPopulation; they count
	// toward Summary.Skipped here too, since they consumed none of the
	// batch's Limit and were never candidates for analysis in the first
	// place.
	skipped := len(skippedIDs) + unparseable

	limit := req.Limit
	if limit <= 0 {
		limit = profile.Limit
	}
	if limit > 0 && len(toProcess) > limit {
		toProcess = toProcess[:limit]
	}
	remaining := populationCount - skipped - len(toProcess)

	var successes []analyzedConversation
	failed := 0
	for _, it := range toProcess {
		if ctx.Err() != nil {
			fail(ctx.Err())
			return
		}

		noStore := req.NoStore || it.transcript != nil
		outcome, oerr := b.analyzeOne(ctx, send, it, profile, promptHash, req.StopAfter, noStore)
		if oerr != nil {
			fail(oerr)
			return
		}
		switch {
		case outcome == nil:
			// stop_after == "compact" halted before Detect ever ran: not a
			// success, not a failure, just not counted.
		case outcome.failed:
			failed++
		default:
			successes = append(successes, analyzedConversation{
				conversationID: it.id,
				analysis:       outcome.analysis, analysisID: outcome.analysisID,
				prior: outcome.prior, noStore: noStore,
			})
		}
	}

	totals := agentcli.Totals{}
	for _, s := range successes {
		totals.InputTokens += s.analysis.InputTokens
		totals.OutputTokens += s.analysis.OutputTokens
		totals.CostUSD += s.analysis.CostUSD
	}
	summary := agentcli.Summary{
		Analyzed: len(successes), Skipped: skipped, Failed: failed,
		Remaining: remaining, Population: populationCount, Totals: totals,
	}

	// Parity with the server: stop_after of "compact" or "detect" both mean
	// the caller only wanted per-conversation output up through that stage —
	// ranking, finding-persistence, and drafting never run.
	if req.StopAfter == "compact" || req.StopAfter == "detect" {
		sendTerminal(agentcli.AnalyzeEvent{Kind: agentcli.EventSummary, Summary: &summary})
		return
	}

	if err := b.rankAndDraft(ctx, req, profile, successes, skippedIDs, promptHash, &summary, send); err != nil {
		fail(err)
		return
	}
	sendTerminal(agentcli.AnalyzeEvent{Kind: agentcli.EventSummary, Summary: &summary})
}

// population resolves req's population, in the priority order the CLI
// documents: explicit ConversationIDs, else a corpus directory, else a
// search Filter (or, if nil, the default unfiltered search). It returns the
// resolved items, the TOTAL population count — which, for corpus runs,
// includes files that failed to parse (already reported via send) and so
// never became an item — and, separately, how many of those never became an
// item (always 0 outside corpus mode).
func (b *Backend) population(ctx context.Context, req agentcli.AnalyzeRequest, profile *analyze.Profile, send func(agentcli.AnalyzeEvent) bool) (items []analyzeItem, populationCount, unparseable int, err error) {
	switch {
	case len(req.ConversationIDs) > 0:
		items = make([]analyzeItem, len(req.ConversationIDs))
		for i, id := range req.ConversationIDs {
			items[i] = analyzeItem{id: id}
		}
		return items, len(items), 0, nil

	case req.CorpusDir != "":
		return b.corpusPopulation(req.CorpusDir, send)

	default:
		f := insights.SearchFilter{}
		if req.Filter != nil {
			f = *req.Filter
		}
		f, ferr := mergeProfileFilter(f, profile)
		if ferr != nil {
			return nil, 0, 0, ferr
		}
		// Mirrors the server's searchFilterForAnalyze: an analyze run must
		// not re-ingest its own prior analyze-entrypoint conversations as
		// fresh population unless the caller explicitly asked for a
		// specific entrypoint (request- or profile-set — mergeProfileFilter
		// already folded the profile's entrypoint filter key in above).
		if f.Entrypoint == "" {
			f.ExcludeEntrypoint = "analyze"
		}

		rows, serr := b.ins.Search(ctx, f)
		if serr != nil {
			return nil, 0, 0, fmt.Errorf("agentcli/local: analyze search: %w", serr)
		}

		// Interestingness ordering: error-status conversations first, then
		// by total token volume descending — so a Limit that truncates the
		// batch keeps the conversations most worth spending detector budget
		// on. Stable so ties (e.g. equal tokens) keep Search's own order.
		sort.SliceStable(rows, func(i, j int) bool {
			iErr, jErr := isErrorStatus(rows[i].Status), isErrorStatus(rows[j].Status)
			if iErr != jErr {
				return iErr
			}
			return rows[i].InputTokens+rows[i].OutputTokens > rows[j].InputTokens+rows[j].OutputTokens
		})

		items = make([]analyzeItem, len(rows))
		for i, r := range rows {
			items[i] = analyzeItem{id: r.ID}
		}
		return items, len(items), 0, nil
	}
}

// mergeProfileFilter layers profile.Filters under f — the request's own
// explicit fields always win — mirroring the server's searchFilterForAnalyze
// exactly: owner, persona, source, model, status, since, until, and
// entrypoint (not an insights.SearchFilter field on the request side, so
// profile.Filters carries it as a bare key; a profile-set entrypoint
// suppresses the caller's runAnalyze from also defaulting
// ExcludeEntrypoint, same as a request-set Entrypoint does). An unparseable
// since/until in profile.Filters is an error, never silently dropped.
func mergeProfileFilter(f insights.SearchFilter, profile *analyze.Profile) (insights.SearchFilter, error) {
	applyField := func(cur *string, key string) {
		if *cur == "" {
			if v, ok := profile.Filters[key]; ok {
				*cur = v
			}
		}
	}
	applyField(&f.Owner, "owner")
	applyField(&f.Persona, "persona")
	applyField(&f.Source, "source")
	applyField(&f.Model, "model")
	applyField(&f.Status, "status")

	if f.Since == nil {
		if v, ok := profile.Filters["since"]; ok {
			ts, err := agentcli.ParseTime(v)
			if err != nil {
				return f, fmt.Errorf("agentcli/local: analyze profile filter since=%q: %w", v, err)
			}
			f.Since = ts
		}
	}
	if f.Until == nil {
		if v, ok := profile.Filters["until"]; ok {
			ts, err := agentcli.ParseTime(v)
			if err != nil {
				return f, fmt.Errorf("agentcli/local: analyze profile filter until=%q: %w", v, err)
			}
			f.Until = ts
		}
	}

	if v, ok := profile.Filters["entrypoint"]; ok && v != "" {
		f.Entrypoint = v
	}
	return f, nil
}

// isErrorStatus reports whether a conversation's status marks it as having
// errored, for the interestingness sort in population.
func isErrorStatus(status string) bool {
	return status == "failed" || status == "error"
}

// corpusPopulation reads dir's *.json files as pre-built *insights.Transcript
// values — a local-only mode (see analyzeItem's doc comment: there is no
// conversations.conversation row backing these, so nothing exists for an
// analysis_finding row to FK against). A file that can't be read or doesn't
// decode as a Transcript is skipped rather than aborting the run: it emits a
// StateSkipped progress event carrying the error as Detail, and is counted
// in the returned unparseable total (which the caller folds into
// Summary.Skipped, since these files never became analyzeItems and so never
// went through the DB-backed AnalyzedSet skip path). Every *.json file
// counts toward the returned populationCount, whether or not it became an
// item, so Summary.Population reflects everything the run considered.
func (b *Backend) corpusPopulation(dir string, send func(agentcli.AnalyzeEvent) bool) (items []analyzeItem, populationCount, unparseable int, err error) {
	entries, rderr := os.ReadDir(dir)
	if rderr != nil {
		return nil, 0, 0, fmt.Errorf("agentcli/local: read corpus dir %q: %w", dir, rderr)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// A corpus run writes its own *.compact.json/*.detect.json/*.rank.json/
		// *.draft.json artifacts (WriteArtifacts) and _prompts.md sidecar into
		// --out — which, unless a caller is careful, is easy to point right
		// back at --corpus. Without this check, re-running --corpus against a
		// directory already containing a prior run's output re-ingests those
		// artifacts as if they were fresh conversations, corrupting Population
		// with data the run itself produced. looksLikeArtifact name-matches
		// them out before ever attempting to parse.
		if looksLikeArtifact(e.Name()) {
			continue
		}
		populationCount++

		id := e.Name()
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			unparseable++
			if !send(progressEvent(id, agentcli.StateSkipped, rerr.Error(), 0, 0, 0)) {
				return items, populationCount, unparseable, nil
			}
			continue
		}

		var tr insights.Transcript
		if uerr := json.Unmarshal(raw, &tr); uerr != nil {
			unparseable++
			if !send(progressEvent(id, agentcli.StateSkipped, uerr.Error(), 0, 0, 0)) {
				return items, populationCount, unparseable, nil
			}
			continue
		}
		// A well-formed but empty transcript (no Turns) is not a real
		// conversation — most often it's some other JSON document (or a
		// pre-existing artifact this run's name-based filter above didn't
		// happen to catch, e.g. a hand-renamed file) that merely happens to
		// unmarshal into insights.Transcript's zero-ish shape without error.
		// Treat it the same as any other unparseable file rather than handing
		// analyzeOne/Detect zero turns to work with.
		if len(tr.Turns) == 0 {
			unparseable++
			if !send(progressEvent(id, agentcli.StateSkipped, "transcript has no turns", 0, 0, 0)) {
				return items, populationCount, unparseable, nil
			}
			continue
		}
		if tr.ConversationID != "" {
			id = tr.ConversationID
		}
		items = append(items, analyzeItem{id: id, transcript: &tr})
	}
	return items, populationCount, unparseable, nil
}

// artifactSuffixes are the file-name suffixes a corpus Analyze run's own
// --out writes (WriteArtifacts' per-stage JSON, plus the _prompts.md
// sidecar) — anything matching one of these came from a prior run's output,
// not a real conversation to analyze.
var artifactSuffixes = []string{
	".compact.json", ".detect.json", ".rank.json", ".draft.json",
}

// looksLikeArtifact reports whether name matches one of artifactSuffixes, or
// is the _prompts.md sidecar (checked by name.json variant defensively, since
// corpusPopulation already filters to *.json before calling this).
func looksLikeArtifact(name string) bool {
	if name == "_prompts.md" {
		return true
	}
	for _, suf := range artifactSuffixes {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}
