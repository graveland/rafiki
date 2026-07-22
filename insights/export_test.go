package insights

import (
	"context"
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

	tr, err := New(pool).Export(ctx, convID)
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

func TestExport_AttachesTurnMetrics(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	convID := seedConversationWithSkill(t, pool)

	tr, err := New(pool).Export(ctx, convID)
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
	if assistant.InputTokens != 200 || assistant.OutputTokens != 90 || assistant.CacheReadTokens != 150 {
		t.Errorf("assistant metrics = in %d/out %d/cache %d, want 200/90/150",
			assistant.InputTokens, assistant.OutputTokens, assistant.CacheReadTokens)
	}
	if assistant.LatencyMS != 1500 || assistant.PrefixHash != "prefix-1" {
		t.Errorf("assistant latency=%d prefix=%q, want 1500/prefix-1", assistant.LatencyMS, assistant.PrefixHash)
	}
}

func TestExport_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	_, err := New(pool).Export(ctx, "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("export of a missing conversation must error")
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

	tr, err := New(pool).Export(ctx, convID)
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
	if assistant.InputTokens != 321 || assistant.OutputTokens != 88 || assistant.CacheReadTokens != 200 {
		t.Errorf("direct-path metrics = in %d/out %d/cache %d, want 321/88/200",
			assistant.InputTokens, assistant.OutputTokens, assistant.CacheReadTokens)
	}
	if assistant.LatencyMS != 1717 || assistant.Model != "claude-fable-5" {
		t.Errorf("direct-path latency=%d model=%q, want 1717/claude-fable-5", assistant.LatencyMS, assistant.Model)
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
		tr, err := New(pool).Export(ctx, convID)
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		var assistant *TranscriptTurn
		for idx := range tr.Turns {
			if tr.Turns[idx].Ordinal == 1 {
				assistant = &tr.Turns[idx]
			}
		}
		if assistant == nil || assistant.Model != "new" || assistant.InputTokens != 999 {
			t.Fatalf("assistant = %+v, want newest turn (model new, in 999)", assistant)
		}
	}
}
