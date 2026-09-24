// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedConversationWithSkill creates a client-driven conversation whose assistant
// message invokes the "brainstorming" Skill, with prefix_content that lists an
// available skill catalog. Returns the conversation id.
func seedConversationWithSkill(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	convID := insertConversation(t, pool, "client", "carol")
	one := 1
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 0, model: "claude-fable-5", source: "claude", upstream: "anthropic",
		inTok: 200, outTok: 90, cacheRead: 150, latencyMS: 1500,
		prefixHash:      "prefix-1",
		prefixContent:   `{"system":"You have skills:\n- brainstorming: explore ideas\n- writing-plans: draft a plan\n"}`,
		responseOrdinal: &one,
		createdAt:       time.Now().Add(-time.Minute),
	})
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"help me design a thing"}]`)
	insertMessage(t, pool, convID, 1, "assistant",
		`[{"type":"text","text":"let me brainstorm"},{"type":"tool_use","name":"Skill","input":{"skill":"brainstorming"}}]`)
	return convID
}

func TestExport_SkillUsage(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversationWithSkill(t, pool)

	tr, err := New(pool).Export(ctx, ScopeAll(), convID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(tr.Turns) == 0 {
		t.Fatal("no turns exported")
	}
	if tr.DrivenBy != "client" {
		t.Errorf("driven_by = %q, want client", tr.DrivenBy)
	}

	var found bool
	for _, tn := range tr.Turns {
		for _, sk := range tn.Skills {
			if sk == "brainstorming" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("brainstorming skill not found in any turn's Skills; turns=%+v", tr.Turns)
	}

	// AvailableSkills must be recovered from prefix_content's listing.
	if !contains(tr.AvailableSkills, "brainstorming") || !contains(tr.AvailableSkills, "writing-plans") {
		t.Errorf("available skills = %v, want to include brainstorming and writing-plans", tr.AvailableSkills)
	}
}

// TestSkillsInContentToolNameCase pins the case-insensitive skill-tool match:
// Claude Code's transcripts carry name "Skill" while fundi's carry "skill"
// (pkg/fundi/tools/skill.go registers the lowercase spelling) — both must be
// detected. Pure (no pool) so it cannot silently skip without RAFIKI_TEST_DSN.
func TestSkillsInContentToolNameCase(t *testing.T) {
	for _, name := range []string{"Skill", "skill", "SKILL"} {
		content := `[{"type":"tool_use","name":"` + name + `","input":{"skill":"brainstorming"}}]`
		got := skillsInContent([]byte(content))
		if len(got) != 1 || got[0] != "brainstorming" {
			t.Errorf("tool name %q: skillsInContent = %v, want [brainstorming]", name, got)
		}
	}

	// A non-skill tool_use (even one with a "skill"-shaped input key) must not match.
	content := `[{"type":"tool_use","name":"bash","input":{"skill":"brainstorming"}},` +
		`{"type":"tool_use","name":"skilled","input":{"skill":"brainstorming"}}]`
	if got := skillsInContent([]byte(content)); len(got) != 0 {
		t.Errorf("non-skill tools: skillsInContent = %v, want empty", got)
	}
}

func TestExport_AttachesTurnMetrics(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversationWithSkill(t, pool)

	tr, err := New(pool).Export(ctx, ScopeAll(), convID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// The assistant message at ordinal 1 must carry the producing turn's metrics.
	var assistant *TranscriptTurn
	for idx := range tr.Turns {
		if tr.Turns[idx].Ordinal == 1 {
			assistant = &tr.Turns[idx]
		}
	}
	if assistant == nil {
		t.Fatal("no assistant message at ordinal 1")
	}
	if got := metricsOf(assistant); got != "in 200/out 90/cache 150/latency 1500" || assistant.PrefixHash != "prefix-1" {
		t.Errorf("assistant metrics = %s prefix=%q, want in 200/out 90/cache 150/latency 1500, prefix-1", got, assistant.PrefixHash)
	}
}

func TestExport_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	_, err := New(pool).Export(ctx, ScopeAll(), "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("export of a missing conversation must error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("export missing conversation err = %v, want ErrNotFound", err)
	}
}

// TestExport_MalformedIDIsNotFound pins checkConversationID's call site: a
// child id (the usual mistake) and any other non-UUID must come back as
// ErrNotFound, not as a Postgres uuid syntax error that the control plane
// reports as internal.
func TestExport_MalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	for _, id := range []string{"c_01M3AC3TYYJAW3RX40DQ9GNYYN", "not-a-uuid", ""} {
		_, err := New(pool).Export(ctx, ScopeAll(), id)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Export(%q) err = %v, want ErrNotFound", id, err)
		}
	}
	_, err := New(pool).Export(ctx, ScopeAll(), "c_01M3AC3TYYJAW3RX40DQ9GNYYN")
	if err == nil || !strings.Contains(err.Error(), "child id") {
		t.Errorf("a c_ id should be named as a child id, got %v", err)
	}
}

// metricsOf renders a turn's metrics with nil shown as "null", so one string
// comparison checks both the values and that nothing is missing.
func metricsOf(turn *TranscriptTurn) string {
	i64 := func(p *int64) string {
		if p == nil {
			return "null"
		}
		return fmt.Sprint(*p)
	}
	lat := "null"
	if turn.LatencyMS != nil {
		lat = fmt.Sprint(*turn.LatencyMS)
	}
	return fmt.Sprintf("in %s/out %s/cache %s/latency %s",
		i64(turn.InputTokens), i64(turn.OutputTokens), i64(turn.CacheReadTokens), lat)
}

// TestExport_UnreportedMetricsAreNull pins the nil-vs-zero distinction: a
// message with no turn row (a user message, or a synthetic pre-fill row)
// exports null metrics, a turn row's NULL column exports null for that field
// alone, and a measured zero stays zero.
func TestExport_UnreportedMetricsAreNull(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "server", "erin")
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"hi"}]`)
	insertMessage(t, pool, convID, 1, "assistant", `[{"type":"tool_use","id":"prefill_0001","name":"read","input":{}}]`)
	insertMessage(t, pool, convID, 2, "user", `[{"type":"tool_result","tool_use_id":"prefill_0001","content":"x"}]`)
	insertMessage(t, pool, convID, 3, "assistant", `[{"type":"text","text":"done"}]`)
	turnID := insertTurn(t, pool, convID, seedTurn{ordinal: 3, model: "m", inTok: 40, outTok: 0, latencyMS: 900})
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.conversation_turn SET cache_read_tokens = NULL WHERE id = $1`, turnID); err != nil {
		t.Fatal(err)
	}

	tr, err := New(pool).Export(ctx, ScopeAll(), convID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := map[int]string{
		0: "in null/out null/cache null/latency null",
		1: "in null/out null/cache null/latency null", // synthetic pre-fill row: no turn
		2: "in null/out null/cache null/latency null",
		3: "in 40/out 0/cache null/latency 900", // measured zero output, NULL cache column
	}
	for idx := range tr.Turns {
		turn := &tr.Turns[idx]
		if got := metricsOf(turn); got != want[turn.Ordinal] {
			t.Errorf("ordinal %d metrics = %s, want %s", turn.Ordinal, got, want[turn.Ordinal])
		}
	}
	b, err := json.Marshal(tr.Turns[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"input_tokens":null`) {
		t.Errorf("unreported metrics must marshal as null, got %s", b)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestExport_DirectPathMetrics seeds a conversation the way the in-process llm
// path writes it — NULL response_ordinal, turn.ordinal equal to the assistant
// message ordinal it produced — and asserts the exported assistant turn carries
// the real tokens/latency/model (the proxy-only response_ordinal join missed).
func TestExport_DirectPathMetrics(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "server", "dinah")
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"hi"}]`)
	insertMessage(t, pool, convID, 1, "assistant", `[{"type":"text","text":"hello"}]`)
	// Direct path: response_ordinal left NULL; ordinal is the assistant ordinal.
	insertTurn(t, pool, convID, seedTurn{
		ordinal: 1, model: "claude-fable-5", source: "diagnose",
		inTok: 321, outTok: 88, cacheRead: 200, latencyMS: 1717,
	})

	tr, err := New(pool).Export(ctx, ScopeAll(), convID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var assistant *TranscriptTurn
	for idx := range tr.Turns {
		if tr.Turns[idx].Ordinal == 1 {
			assistant = &tr.Turns[idx]
		}
	}
	if assistant == nil {
		t.Fatal("no assistant turn at ordinal 1")
	}
	if got := metricsOf(assistant); got != "in 321/out 88/cache 200/latency 1717" || assistant.Model != "claude-fable-5" {
		t.Errorf("direct-path metrics = %s model=%q, want in 321/out 88/cache 200/latency 1717, claude-fable-5", got, assistant.Model)
	}
}

// TestExport_DuplicateOrdinalNewestWins seeds two turns producing the same
// assistant ordinal (a resumed re-run); the newer turn's metrics must attach,
// deterministically across runs.
func TestExport_DuplicateOrdinalNewestWins(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := insertConversation(t, pool, "server", "edith")
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"hi"}]`)
	insertMessage(t, pool, convID, 1, "assistant", `[{"type":"text","text":"hello"}]`)
	base := time.Now().Add(-time.Hour)
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "old", inTok: 1, latencyMS: 10, createdAt: base})
	insertTurn(t, pool, convID, seedTurn{ordinal: 1, model: "new", inTok: 999, latencyMS: 20, createdAt: base.Add(time.Minute)})

	for range 3 { // stable across repeated runs
		tr, err := New(pool).Export(ctx, ScopeAll(), convID)
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		var assistant *TranscriptTurn
		for idx := range tr.Turns {
			if tr.Turns[idx].Ordinal == 1 {
				assistant = &tr.Turns[idx]
			}
		}
		if assistant == nil || assistant.Model != "new" || assistant.InputTokens == nil || *assistant.InputTokens != 999 {
			t.Fatalf("assistant = %+v, want newest turn (model new, in 999)", assistant)
		}
	}
}

// TestExportScopeMissReturnsNotFound pins that Export of another owner's
// conversation folds into the same ErrNotFound a missing id gets — Export used
// to take no identity at all, reading any conversation on the daemon.
func TestExportScopeMissReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	bobID := ensureUser(t, pool, "bob")
	carolConvID := seedConversation(t, pool, "client", "carol")

	_, err := New(pool).Export(ctx, ScopeOwner(bobID), carolConvID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Export of another owner's conversation err = %v, want ErrNotFound (a scope miss reads as not-found)", err)
	}
}
