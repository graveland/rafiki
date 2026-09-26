// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/routing"
)

// aliasProviders returns a registry whose prov entry carries one alias with
// declared window metadata, so Controller.ModelInfo's alias path (not the
// OpenRouter catalog, not providers.toml on disk) answers deterministically.
func aliasProviders() *providers.Set {
	return &providers.Set{
		DefaultProvider: "prov",
		Providers: map[string]providers.Provider{
			"prov": {
				Name: "prov",
				Models: map[string]providers.ModelAlias{
					"alias": {ID: "real-id", ContextWindow: 200000, MaxCompletionTokens: 32000},
				},
			},
		},
	}
}

// TestChildOpsModelInfoMatchesFramed pins the one answer a client consumes
// daemon-side (so it never reads OpenRouter itself): the Connect adapter's
// ModelInfo must equal, field by field, the ModelInfoResponseData the framed
// ctrl_model_info handler served from the same Controller. Both sides of the
// comparison run against the SAME Controller here, so a mapping that drops
// or misnames a field fails loudly rather than agreeing with itself by
// accident.
func TestChildOpsModelInfoMatchesFramed(t *testing.T) {
	ctrl := &Controller{providers: aliasProviders()}
	s := connectapi.NewServer(nil)
	s.SetChildOps(connectChildOps{c: ctrl})

	for _, model := range []string{"prov/alias", "prov/unknown", "unconfigured/model", "bare-id"} {
		t.Run(model, func(t *testing.T) {
			want := ctrl.ModelInfo(model) // the framed payload, verbatim
			resp, err := s.ModelInfo(context.Background(),
				connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: model}))
			if err != nil {
				t.Fatalf("ModelInfo(%q): %v", model, err)
			}
			got := resp.Msg
			if got.GetModel() != want.Model {
				t.Errorf("model = %q, want %q", got.GetModel(), want.Model)
			}
			if got.GetResolvedId() != want.ResolvedID {
				t.Errorf("resolved_id = %q, want %q", got.GetResolvedId(), want.ResolvedID)
			}
			if got.GetContextWindow() != int32(want.ContextWindow) {
				t.Errorf("context_window = %d, want %d", got.GetContextWindow(), want.ContextWindow)
			}
			if got.GetMaxCompletionTokens() != int32(want.MaxCompletionTokens) {
				t.Errorf("max_completion_tokens = %d, want %d", got.GetMaxCompletionTokens(), want.MaxCompletionTokens)
			}
			if got.GetAutoCompactWindow() != int32(want.AutoCompactWindow) {
				t.Errorf("auto_compact_window = %d, want %d", got.GetAutoCompactWindow(), want.AutoCompactWindow)
			}
			if got.GetKnown() != want.Known {
				t.Errorf("known = %v, want %v", got.GetKnown(), want.Known)
			}
		})
	}

	// The alias case above must actually be known — a mapping that zeroed
	// everything would otherwise pass every field comparison against a
	// zero-value want.
	if known := ctrl.ModelInfo("prov/alias"); !known.Known || known.ResolvedID != "prov/real-id" {
		t.Fatalf("fixture broke: ModelInfo(prov/alias) = %+v, want known with resolved id", known)
	}
}

func TestChildOpsModelInfoRoutingAutoCompactMatchesCatalog(t *testing.T) {
	// The formula lives in one binary by design (the alias path computes it
	// daemon-side); pin that the adapter's answer is exactly
	// routing.AutoCompactWindow of the declared windows.
	ctrl := &Controller{providers: aliasProviders()}
	got, err := (connectChildOps{c: ctrl}).ModelInfo(context.Background(), "prov/alias")
	if err != nil {
		t.Fatalf("ModelInfo: %v", err)
	}
	want := routing.AutoCompactWindow(200000, 32000)
	if got.AutoCompactWindow != int32(want) {
		t.Errorf("auto_compact_window = %d, want routing.AutoCompactWindow = %d", got.AutoCompactWindow, want)
	}
}

func TestChildOpsSearchQueryMapping(t *testing.T) {
	t.Run("fields pass through", func(t *testing.T) {
		q := buildSearchQuery(&rafikiv1.SearchRequest{
			Query: "needle", Regex: true, Limit: 7, Context: 2,
			SessionFilter: &rafikiv1.SearchRequest_SearchSessionFilter{
				CwdContains:  "/work",
				NameContains: "fix",
				Since:        123456,
				Labels:       map[string]string{"team": "core"},
				HasLabel:     []string{"urgent"},
			},
		})
		if q.Query != "needle" || !q.Regex || q.Limit != 7 || q.Context != 2 {
			t.Errorf("scalar fields = %+v, want the request's values", q)
		}
		sf := q.SessionFilter
		if sf.CwdContains != "/work" || sf.NameContains != "fix" || sf.Since != 123456 ||
			sf.Labels["team"] != "core" || len(sf.HasLabel) != 1 || sf.HasLabel[0] != "urgent" {
			t.Errorf("session filter = %+v, want every field mapped", sf)
		}
	})
	t.Run("absent session filter means every child", func(t *testing.T) {
		q := buildSearchQuery(&rafikiv1.SearchRequest{Query: "needle"})
		if q.SessionFilter.CwdContains != "" || q.SessionFilter.NameContains != "" ||
			q.SessionFilter.Since != 0 || q.SessionFilter.Labels != nil || q.SessionFilter.HasLabel != nil {
			t.Errorf("session filter = %+v, want the zero value (nil proto filter)", q.SessionFilter)
		}
	})
}

func TestChildOpsSearchResponseMapping(t *testing.T) {
	t.Run("hits map field by field", func(t *testing.T) {
		resp := searchResponseFrom(controlSearchResult())
		if len(resp.Hits) != 1 {
			t.Fatalf("hits = %d, want 1", len(resp.Hits))
		}
		h, want := resp.Hits[0], controlSearchResult().Hits[0]
		if h.ChildId != want.ChildID || h.SessionFile != want.SessionFile ||
			h.SessionId != want.SessionID || h.SessionName != want.SessionName ||
			h.EntryId != want.EntryID || h.Timestamp != want.Timestamp ||
			h.Role != want.Role || h.Snippet != want.Snippet ||
			h.MatchStart != int32(want.MatchStart) || h.MatchEnd != int32(want.MatchEnd) {
			t.Errorf("hit = %+v, want %+v", h, want)
		}
		if resp.TotalHits != 1 || resp.Scanned != 4 || resp.Elapsed != 9 {
			t.Errorf("totals = %d/%d/%d, want 1/4/9", resp.TotalHits, resp.Scanned, resp.Elapsed)
		}
	})
	t.Run("no hits is an empty array, never null", func(t *testing.T) {
		resp := searchResponseFrom(protocol.SearchResponseData{})
		if resp.Hits == nil {
			t.Error("hits = nil, want an empty repeated field")
		}
		if len(resp.Hits) != 0 {
			t.Errorf("hits = %d entries, want 0", len(resp.Hits))
		}
	})
}

// controlSearchResult is one framed hit with every field set, so a dropped
// mapping is visible.
func controlSearchResult() protocol.SearchResponseData {
	return protocol.SearchResponseData{
		Hits: []protocol.SearchHit{{
			ChildID:     "c_1",
			SessionFile: "/sessions/s.jsonl",
			SessionID:   "sid",
			SessionName: "name",
			EntryID:     "e1",
			Timestamp:   1700000000000,
			Role:        "user",
			Snippet:     "the match",
			MatchStart:  4,
			MatchEnd:    9,
		}},
		TotalHits: 1,
		Scanned:   4,
		Elapsed:   9,
	}
}

func TestChildOpsStatusMapping(t *testing.T) {
	resp := statusResponseFrom(protocol.StatusResponseData{
		Version:     "v1",
		StartedAt:   1700000000000,
		Children:    protocol.ChildCounts{Live: 3, Exited: 2},
		MemoryBytes: 1 << 20,
		Socket:      "/tmp/r.sock",
		LogsDir:     "/tmp/logs",
	})
	if resp.Version != "v1" || resp.StartedAt != 1700000000000 ||
		resp.Children.GetLive() != 3 || resp.Children.GetExited() != 2 ||
		resp.MemoryBytes != 1<<20 || resp.Socket != "/tmp/r.sock" || resp.LogsDir != "/tmp/logs" {
		t.Errorf("status = %+v, want every field mapped", resp)
	}
}

func TestChildOpsStatsFilterMapping(t *testing.T) {
	t.Run("fields pass through", func(t *testing.T) {
		f := buildStatsFilter(&rafikiv1.ConversationStatsRequest{
			SinceUnix: 1700000000, UntilUnix: 1700003600,
			Owner: "u1", Persona: "impl", Source: "cli", Model: "m", Path: "proxy",
		})
		if f.Since == nil || f.Since.Unix() != 1700000000 || f.Until == nil || f.Until.Unix() != 1700003600 {
			t.Errorf("since/until = %v/%v, want the unix seconds", f.Since, f.Until)
		}
		if f.Owner != "u1" || f.Persona != "impl" || f.Source != "cli" || f.Model != "m" || f.Path != insights.Path("proxy") {
			t.Errorf("filter = %+v, want every field mapped", f)
		}
	})
	t.Run("zero unix means unbounded", func(t *testing.T) {
		f := buildStatsFilter(&rafikiv1.ConversationStatsRequest{})
		if f.Since != nil || f.Until != nil {
			t.Errorf("since/until = %v/%v, want nil (unbounded)", f.Since, f.Until)
		}
	})
}

// TestChildOpsShutdownDaemonBudgetsMatchMain pins the drain budgets to the
// framed signal path's values. The drain itself fires from a goroutine the
// response does not wait on; this pin is the compile-adjacent guarantee that
// a drift between the two shutdown sequences is at least a reviewed
// constant, not a silent one.
func TestChildOpsShutdownDaemonBudgetsMatchMain(t *testing.T) {
	if daemonShutdownChildTimeout != 120*time.Second {
		t.Errorf("daemonShutdownChildTimeout = %v, want main.go's 120s", daemonShutdownChildTimeout)
	}
	if daemonShutdownKillTimeout != 30*time.Second {
		t.Errorf("daemonShutdownKillTimeout = %v, want main.go's 30s", daemonShutdownKillTimeout)
	}
	if daemonShutdownGlobalBound != 180*time.Second {
		t.Errorf("daemonShutdownGlobalBound = %v, want main.go's 180s", daemonShutdownGlobalBound)
	}
}
