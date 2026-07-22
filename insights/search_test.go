package insights

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestConversationSummary_JSONTags(t *testing.T) {
	b, err := json.Marshal(ConversationSummary{ID: "x", DrivenBy: "client", InputTokens: 42})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"driven_by"`, `"input_tokens"`, `"cache_read_tokens"`, `"first_message"`} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled summary %s missing %s", got, want)
		}
	}
	if strings.Contains(got, `"DrivenBy"`) || strings.Contains(got, `"InputTokens"`) {
		t.Errorf("marshaled summary %s still has CamelCase keys", got)
	}
}

func TestSearch_FiltersByPath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice") // proxy
	seedConversation(t, pool, "server", "bob")   // direct
	ins := New(pool)

	proxyOnly, err := ins.Search(ctx, SearchFilter{Path: PathProxy, Limit: 10})
	if err != nil {
		t.Fatalf("search proxy: %v", err)
	}
	if len(proxyOnly) != 1 {
		t.Fatalf("proxy results = %d, want 1", len(proxyOnly))
	}
	if proxyOnly[0].DrivenBy != "client" {
		t.Errorf("driven_by = %q, want client", proxyOnly[0].DrivenBy)
	}
	if proxyOnly[0].Turns <= 0 {
		t.Errorf("turns = %d, want > 0", proxyOnly[0].Turns)
	}
	if proxyOnly[0].Owner != "alice" {
		t.Errorf("owner = %q, want alice", proxyOnly[0].Owner)
	}
	// The aggregate must sum both turns (100+120 in, 0+80 cache_read).
	if proxyOnly[0].InputTokens != 220 {
		t.Errorf("input_tokens = %d, want 220", proxyOnly[0].InputTokens)
	}
	if proxyOnly[0].CacheReadTokens != 80 {
		t.Errorf("cache_read_tokens = %d, want 80", proxyOnly[0].CacheReadTokens)
	}
	if proxyOnly[0].FirstMessage == "" {
		t.Error("first message snippet is empty, want the seeded user text")
	}

	directOnly, err := ins.Search(ctx, SearchFilter{Path: PathDirect, Limit: 10})
	if err != nil {
		t.Fatalf("search direct: %v", err)
	}
	if len(directOnly) != 1 || directOnly[0].DrivenBy != "server" {
		t.Fatalf("direct results = %+v, want one server conversation", directOnly)
	}

	all, err := ins.Search(ctx, SearchFilter{Limit: 10})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered results = %d, want 2", len(all))
	}
}

func TestSearch_FiltersByOwnerAndText(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	seedConversation(t, pool, "client", "alice")
	seedConversation(t, pool, "server", "bob")
	ins := New(pool)

	byOwner, err := ins.Search(ctx, SearchFilter{Owner: "bob"})
	if err != nil {
		t.Fatalf("search owner: %v", err)
	}
	if len(byOwner) != 1 || byOwner[0].Owner != "bob" {
		t.Fatalf("owner filter = %+v, want one bob conversation", byOwner)
	}

	byText, err := ins.Search(ctx, SearchFilter{Text: "hello"})
	if err != nil {
		t.Fatalf("search text: %v", err)
	}
	if len(byText) != 2 {
		t.Errorf("text 'hello' matched %d, want 2", len(byText))
	}

	noText, err := ins.Search(ctx, SearchFilter{Text: "nonexistent-substring"})
	if err != nil {
		t.Fatalf("search text miss: %v", err)
	}
	if len(noText) != 0 {
		t.Errorf("text miss matched %d, want 0", len(noText))
	}

	byMinTokens, err := ins.Search(ctx, SearchFilter{MinTokens: 1000})
	if err != nil {
		t.Fatalf("search min tokens: %v", err)
	}
	if len(byMinTokens) != 0 {
		t.Errorf("min tokens 1000 matched %d, want 0 (seeded totals are lower)", len(byMinTokens))
	}
}
