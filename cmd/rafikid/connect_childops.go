// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// connectChildOps adapts *Controller to connectapi.ChildOps, the
// operator-side slice behind the Connect Control service's Resume /
// CloseAllExited / SetLabels / Status / Search / ShutdownDaemon / ModelInfo /
// ConversationStats RPCs. Each method reproduces what the framed
// dispatcher's handler (pkg/control/dispatch.go) did between decoding its
// frame and writing the response — request validation stays on the connectapi
// side, so an adapter method is pure convert-and-delegate — and converts with
// the Task 1.1 mapping rule in reverse: one proto field per protocol JSON
// field, in declaration order, skipping the framed envelope's Type and ID.
//
// The ControllerError values these methods return keep their authored
// messages under their protocol codes: connectapi.ConnectErr maps them, the
// same promise mapErr honors on the framed plane.
type connectChildOps struct{ c *Controller }

// Resume re-spawns an exited child. The framed handler answered with the
// SpawnResult it got from Controller.Resume; the proto response carries only
// the child id, which is all ResumeResponse has.
func (a connectChildOps) Resume(ctx context.Context, childID, apiKey string) (string, error) {
	res, err := a.c.Resume(ctx, childID, apiKey)
	if err != nil {
		return "", err
	}
	return res.ChildID, nil
}

// CloseAllExited closes every exited child older than olderThan. It takes
// the seam's ctx for symmetry but Controller.CloseAllExited has never needed
// one (its per-child deletes bound themselves internally).
func (a connectChildOps) CloseAllExited(_ context.Context, olderThan time.Duration) ([]string, error) {
	return a.c.CloseAllExited(olderThan)
}

// SetLabels applies set entries then remove entries, and returns the full
// post-mutation map. The controller handles nothing beyond the store update;
// this adapter adds nothing on top.
func (a connectChildOps) SetLabels(_ context.Context, childID string, set map[string]string, remove []string) (map[string]string, error) {
	return a.c.SetLabels(childID, set, remove)
}

// Status converts Controller.Status into the proto vitals message, nested
// ChildCounts included. started_at is a Timestamp, matching
// Controller.startedAt.
func (a connectChildOps) Status(_ context.Context) (*rafikiv1.StatusResponse, error) {
	return statusResponseFrom(a.c.Status()), nil
}

// Search converts the proto request onto the framed SearchQuery the
// Controller answers — limit and context pass through as-is, the session
// filter value-copies only when present (a nil proto filter means "every
// child", the zero value of the framed filter), and Controller.Search's own
// limit<=0 floor is the default limit, exactly as on the framed plane.
func (a connectChildOps) Search(_ context.Context, req *rafikiv1.SearchRequest) (*rafikiv1.SearchResponse, error) {
	return searchResponseFrom(a.c.Search(buildSearchQuery(req))), nil
}

// wireTime maps a wire Timestamp onto a time.Time, treating an unset message
// AND the epoch (the message's zero value) as the zero time — the filter's
// "unbounded", matching the old integer 0.
func wireTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil || (ts.GetSeconds() == 0 && ts.GetNanos() == 0) {
		return time.Time{}
	}
	return ts.AsTime()
}

// wireTimePtr is wireTime as a *time.Time: nil for an unset message or the
// epoch, matching StatsFilter's nil-means-unbounded Since/Until.
func wireTimePtr(ts *timestamppb.Timestamp) *time.Time {
	t := wireTime(ts)
	if t.IsZero() {
		return nil
	}
	return &t
}

// buildSearchQuery maps the proto SearchRequest onto the framed SearchQuery.
// Extracted so the field mapping is testable without a Controller behind it.
func buildSearchQuery(req *rafikiv1.SearchRequest) searchQuery {
	q := searchQuery{
		Query:   req.GetQuery(),
		Regex:   req.GetRegex(),
		Limit:   int(req.GetLimit()),
		Context: int(req.GetContext()),
	}
	if sf := req.GetSessionFilter(); sf != nil {
		q.SessionFilter = protocol.SearchSessionFilter{
			CwdContains:  sf.GetCwdContains(),
			NameContains: sf.GetNameContains(),
			Since:        wireTime(sf.GetSince()),
			Labels:       sf.GetLabels(),
			HasLabel:     sf.GetHasLabel(),
		}
	}
	return q
}

// searchResponseFrom converts the framed SearchResult onto the proto
// response, hit by hit. Hits is never nil: an empty result is an empty
// repeated field, never a missing one.
func searchResponseFrom(result protocol.SearchResponseData) *rafikiv1.SearchResponse {
	hits := make([]*rafikiv1.SearchResponse_SearchHit, 0, len(result.Hits))
	for _, h := range result.Hits {
		hits = append(hits, &rafikiv1.SearchResponse_SearchHit{
			ChildId:     h.ChildID,
			SessionFile: h.SessionFile,
			SessionId:   h.SessionID,
			SessionName: h.SessionName,
			EntryId:     h.EntryID,
			Timestamp:   timestamppb.New(h.Timestamp),
			Role:        h.Role,
			Snippet:     h.Snippet,
			MatchStart:  int32(h.MatchStart),
			MatchEnd:    int32(h.MatchEnd),
		})
	}
	return &rafikiv1.SearchResponse{
		Hits:      hits,
		TotalHits: int32(result.TotalHits),
		Scanned:   int32(result.Scanned),
		Elapsed:   durationpb.New(result.Elapsed),
	}
}

// statusResponseFrom converts the framed ControllerStatus onto the proto
// vitals message. Extracted for the same reason as buildSearchQuery.
func statusResponseFrom(st protocol.StatusResponseData) *rafikiv1.StatusResponse {
	out := &rafikiv1.StatusResponse{
		Version:     st.Version,
		Children:    &rafikiv1.StatusResponse_ChildCounts{Live: int32(st.Children.Live), Exited: int32(st.Children.Exited)},
		MemoryBytes: st.MemoryBytes,
		Socket:      st.Socket,
		LogsDir:     st.LogsDir,
	}
	if !st.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(st.StartedAt)
	}
	return out
}

// ShutdownDaemon is a fail-closed stub: the child-drain half of the framed
// signal path is NOT served from here. Calling Controller.ShutdownAllChildren
// while the daemon keeps running flips Controller's one-way stopping latch
// (whose contract is "the daemon is dying"), and from then on every child
// exit skips its status persist — so the next daemon start would auto-resume
// children the operator explicitly killed. The full sequence (broadcast →
// drain → close listeners → exit) is wired into main.go's signal path when
// the daemon shutdown path lands; until then the connectapi handler refuses
// CodeUnimplemented before this method is ever reached, so the seam stays
// declared but inert.
func (a connectChildOps) ShutdownDaemon(_ context.Context) error {
	return errors.New("ShutdownDaemon: not served until the daemon shutdown path lands")
}

// ModelInfo delegates to Controller.ModelInfo and converts it. Never an
// error: an unknown model is known=false, not a failure.
func (a connectChildOps) ModelInfo(_ context.Context, model string) (*rafikiv1.ModelInfoResponse, error) {
	return modelInfoResponseFrom(a.c.ModelInfo(model)), nil
}

// modelInfoResponseFrom converts the framed ModelInfoResponseData onto the
// proto response. Extracted so the field-by-field equality against the
// framed answer is testable directly.
func modelInfoResponseFrom(mi protocol.ModelInfoResponseData) *rafikiv1.ModelInfoResponse {
	return &rafikiv1.ModelInfoResponse{
		Model:               mi.Model,
		ResolvedId:          mi.ResolvedID,
		ContextWindow:       int32(mi.ContextWindow),
		MaxCompletionTokens: int32(mi.MaxCompletionTokens),
		AutoCompactWindow:   int32(mi.AutoCompactWindow),
		Known:               mi.Known,
	}
}

// buildStatsFilter maps the proto request's filter fields onto the framed
// insights.StatsFilter, exactly as the framed conversationStats handler
// builds it: an unset or epoch Timestamp means unbounded, matching
// StatsFilter's nil-means-unbounded Since/Until. Extracted so the field
// mapping is testable without a Controller behind it.
func buildStatsFilter(req *rafikiv1.ConversationStatsRequest) insights.StatsFilter {
	return insights.StatsFilter{
		Since:   wireTimePtr(req.GetSince()),
		Until:   wireTimePtr(req.GetUntil()),
		Owner:   req.GetOwner(),
		Persona: req.GetPersona(),
		Source:  req.GetSource(),
		Model:   req.GetModel(),
		Path:    insights.Path(req.GetPath()),
	}
}

// ConversationStats computes the scope from the caller's own credential —
// scopeFor is THE one place the Connect plane does that (the wire carries a
// filter, never a scope: confused-deputy avoidance) — then dispatches to
// ConversationStatsByID when conversation_id is set, otherwise to
// ConversationStats with the StatsFilter built exactly as the framed
// conversationStats handler built it. The scope error is already-coded
// (CodePermissionDenied) and reaches the handler untouched. The result is
// marshalled as-is: the caller asked for an aggregate, not a schema.
func (a connectChildOps) ConversationStats(ctx context.Context, req *rafikiv1.ConversationStatsRequest) (string, error) {
	scope, err := scopeFor(ctx)
	if err != nil {
		return "", err
	}
	var st *insights.Stats
	if id := req.GetConversationId(); id != "" {
		st, err = a.c.ConversationStatsByID(ctx, scope, id)
	} else {
		st, err = a.c.ConversationStats(ctx, scope, buildStatsFilter(req))
	}
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
