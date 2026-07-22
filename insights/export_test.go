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
