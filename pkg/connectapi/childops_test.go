// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// fakeChildOps records what each handler passes and answers with injected
// values, so a handler's success path, its argument pass-through and its
// error mapping are all observable without a daemon.
type fakeChildOps struct {
	// Recorded arguments.
	resumeChildID  string
	resumeAPIKey   string
	exitedOlderMs  int64
	labelsChildID  string
	labelsSet      map[string]string
	labelsRemove   []string
	searchReq      *rafikiv1.SearchRequest
	statsReq       *rafikiv1.ConversationStatsRequest
	shutdownCalled bool

	// Injected answers.
	resumeID  string
	resumeErr error
	exitedIDs []string
	exitedErr error
	labelsOut map[string]string
	labelsErr error
	statusOut *rafikiv1.StatusResponse
	statusErr error
	searchOut *rafikiv1.SearchResponse
	searchErr error
	modelOut  *rafikiv1.ModelInfoResponse
	modelErr  error
	statsJSON string
	statsErr  error
}

func (f *fakeChildOps) Resume(_ context.Context, childID, apiKey string) (string, error) {
	f.resumeChildID, f.resumeAPIKey = childID, apiKey
	if f.resumeErr != nil {
		return "", f.resumeErr
	}
	return f.resumeID, nil
}

func (f *fakeChildOps) CloseAllExited(_ context.Context, olderThanMs int64) ([]string, error) {
	f.exitedOlderMs = olderThanMs
	if f.exitedErr != nil {
		return f.exitedIDs, f.exitedErr
	}
	return f.exitedIDs, nil
}

func (f *fakeChildOps) SetLabels(_ context.Context, childID string, set map[string]string, remove []string) (map[string]string, error) {
	f.labelsChildID, f.labelsSet, f.labelsRemove = childID, set, remove
	if f.labelsErr != nil {
		return nil, f.labelsErr
	}
	return f.labelsOut, nil
}

func (f *fakeChildOps) Status(_ context.Context) (*rafikiv1.StatusResponse, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.statusOut, nil
}

func (f *fakeChildOps) Search(_ context.Context, req *rafikiv1.SearchRequest) (*rafikiv1.SearchResponse, error) {
	f.searchReq = req
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.searchOut, nil
}

// ShutdownDaemon only records that it was reached — the handler refuses
// CodeUnimplemented before the seam this wave (the drain it used to trigger
// poisons recovery state), so no test drives an error through it; the
// recording stays so a future test that calls the seam directly still
// observes it.
func (f *fakeChildOps) ShutdownDaemon(_ context.Context) error {
	f.shutdownCalled = true
	return nil
}

func (f *fakeChildOps) ModelInfo(_ context.Context, model string) (*rafikiv1.ModelInfoResponse, error) {
	f.modelOut.Model = model
	if f.modelErr != nil {
		return nil, f.modelErr
	}
	return f.modelOut, nil
}

func (f *fakeChildOps) ConversationStats(_ context.Context, req *rafikiv1.ConversationStatsRequest) (string, error) {
	f.statsReq = req
	if f.statsErr != nil {
		return "", f.statsErr
	}
	return f.statsJSON, nil
}

func newChildOpsServer(o *fakeChildOps) *connectapi.Server {
	s := connectapi.NewServer(nil)
	s.SetChildOps(o)
	return s
}

// ─── Success paths ────────────────────────────────────────────────────────────

func TestChildOpsResumePassesChildAndKeyThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{resumeID: "c_resumed"}
	resp, err := newChildOpsServer(f).Resume(context.Background(),
		connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: "c_exit", ApiKey: "sk-key"}))
	c.Require().NoError(err, "Resume")
	if f.resumeChildID != "c_exit" || f.resumeAPIKey != "sk-key" {
		t.Errorf("seam got childID=%q apiKey=%q, want c_exit/sk-key", f.resumeChildID, f.resumeAPIKey)
	}
	c.Eq("c_resumed", resp.Msg.GetChildId(), "resp child_id")
}

func TestChildOpsCloseAllExitedPassesOlderThanThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{exitedIDs: []string{"c_a", "c_b"}}
	resp, err := newChildOpsServer(f).CloseAllExited(context.Background(),
		connect.NewRequest(&rafikiv1.CloseAllExitedRequest{OlderThanMs: 5000}))
	c.Require().NoError(err, "CloseAllExited")
	c.Eq(5000, f.exitedOlderMs, "seam got older_than_ms")
	if len(resp.Msg.GetChildIds()) != 2 || resp.Msg.GetChildIds()[0] != "c_a" {
		t.Errorf("resp child_ids = %v, want [c_a c_b]", resp.Msg.GetChildIds())
	}
}

// A PARTIAL CloseAllExited — some children closed, others failed to persist —
// must not read as a bare failure: the wire carries only the error, so the
// count closed must reach the log or a caller cannot tell anything happened.
// Fails against the pre-change handler, which discarded the closed list.
func TestChildOpsPartialCloseAllExitedLogsCountClosed(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{
		exitedIDs: []string{"c_a", "c_b"},
		exitedErr: errors.New("close-all-exited: 1 child(ren) not persisted"),
	}
	logs, err := captureSlog(t, func() error {
		_, err := newChildOpsServer(f).CloseAllExited(context.Background(),
			connect.NewRequest(&rafikiv1.CloseAllExitedRequest{}))
		return err
	})
	c.Require().Error(err, "a partial close must still return an error")
	c.StrContains(logs, "closed=2", "the log must record the count closed; got:\n%s", logs)
}

func TestChildOpsSetLabelsPassesSetAndRemoveThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{labelsOut: map[string]string{"team": "core"}}
	resp, err := newChildOpsServer(f).SetLabels(context.Background(),
		connect.NewRequest(&rafikiv1.SetLabelsRequest{
			ChildId: "c_1",
			Set:     map[string]string{"team": "core"},
			Remove:  []string{"stale"},
		}))
	c.Require().NoError(err, "SetLabels")
	if f.labelsChildID != "c_1" || f.labelsSet["team"] != "core" || len(f.labelsRemove) != 1 || f.labelsRemove[0] != "stale" {
		t.Errorf("seam got childID=%q set=%v remove=%v, want c_1/{team:core}/[stale]", f.labelsChildID, f.labelsSet, f.labelsRemove)
	}
	c.Eq("core", resp.Msg.GetLabels()["team"], "resp labels = %v, want team=core", resp.Msg.GetLabels())
}

func TestChildOpsStatusReturnsTheSeamAnswer(t *testing.T) {
	want := &rafikiv1.StatusResponse{
		Version:     "test",
		StartedAt:   1234,
		Children:    &rafikiv1.StatusResponse_ChildCounts{Live: 2, Exited: 1},
		MemoryBytes: 64,
		Socket:      "/tmp/s.sock",
		LogsDir:     "/tmp/logs",
	}
	resp, err := newChildOpsServer(&fakeChildOps{statusOut: want}).Status(context.Background(),
		connect.NewRequest(&rafikiv1.StatusRequest{}))
	assert.NewAborting(t).NoError(err, "Status")
	if resp.Msg.GetVersion() != "test" || resp.Msg.GetStartedAt() != 1234 ||
		resp.Msg.GetChildren().GetLive() != 2 || resp.Msg.GetChildren().GetExited() != 1 ||
		resp.Msg.GetMemoryBytes() != 64 || resp.Msg.GetSocket() != "/tmp/s.sock" || resp.Msg.GetLogsDir() != "/tmp/logs" {
		t.Errorf("resp = %+v, want %+v", resp.Msg, want)
	}
}

func TestChildOpsSearchReceivesTheRequest(t *testing.T) {
	c := assert.NewAborting(t)
	f := &fakeChildOps{searchOut: &rafikiv1.SearchResponse{
		Hits:      []*rafikiv1.SearchResponse_SearchHit{{ChildId: "c_1", Snippet: "hit"}},
		TotalHits: 1, Scanned: 3, Elapsed: 9,
	}}
	resp, err := newChildOpsServer(f).Search(context.Background(),
		connect.NewRequest(&rafikiv1.SearchRequest{
			Query: "needle", Regex: true, Limit: 7, Context: 2,
			SessionFilter: &rafikiv1.SearchRequest_SearchSessionFilter{
				CwdContains: "/work", HasLabel: []string{"team"},
			},
		}))
	c.NoError(err, "Search")
	c.NotNil(f.searchReq, "the seam was never called")
	if f.searchReq.GetQuery() != "needle" || !f.searchReq.GetRegex() ||
		f.searchReq.GetLimit() != 7 || f.searchReq.GetContext() != 2 ||
		f.searchReq.GetSessionFilter().GetCwdContains() != "/work" ||
		len(f.searchReq.GetSessionFilter().GetHasLabel()) != 1 {
		t.Errorf("seam got %+v, want the request fields intact", f.searchReq)
	}
	if resp.Msg.GetHits()[0].GetSnippet() != "hit" || resp.Msg.GetTotalHits() != 1 || resp.Msg.GetElapsed() != 9 {
		t.Errorf("resp = %+v, want the seam answer back", resp.Msg)
	}
}

// ShutdownDaemon is a fail-closed stub this wave: the handler refuses before
// the seam, so the seam is never consulted — even with one wired — and the
// answer is CodeUnimplemented with the stub's own text, not a mapped
// ControllerError or a redacted internal.
func TestChildOpsShutdownDaemonFailsClosedUnimplemented(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{}
	_, err := newChildOpsServer(f).ShutdownDaemon(context.Background(),
		connect.NewRequest(&rafikiv1.ShutdownDaemonRequest{}))
	c.Eq(connect.CodeUnimplemented, connect.CodeOf(err), "code")
	c.False(err == nil || !strings.Contains(err.Error(), "not yet wired"), "message = %v, want the stub's not-yet-wired text", err)
	c.False(f.shutdownCalled, "the seam's ShutdownDaemon was called; the handler must refuse before the seam")
}

func TestChildOpsModelInfoReturnsTheSeamAnswer(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{modelOut: &rafikiv1.ModelInfoResponse{
		ResolvedId:          "prov/real-id",
		ContextWindow:       200000,
		MaxCompletionTokens: 32000,
		AutoCompactWindow:   180000,
		Known:               true,
	}}
	resp, err := newChildOpsServer(f).ModelInfo(context.Background(),
		connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: "prov/alias"}))
	c.Require().NoError(err, "ModelInfo")
	m := resp.Msg
	c.False(m.GetModel() != "prov/alias" || m.GetResolvedId() != "prov/real-id" ||
		m.GetContextWindow() != 200000 || m.GetMaxCompletionTokens() != 32000 ||
		m.GetAutoCompactWindow() != 180000 || !m.GetKnown(), "resp = %+v, want the seam answer with the requested model echoed", m)
}

func TestChildOpsConversationStatsWrapsTheJSON(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{statsJSON: `{"volume":{"conversations":3}}`}
	resp, err := newChildOpsServer(f).ConversationStats(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationStatsRequest{Owner: "u1", SinceUnix: 42}))
	c.Require().NoError(err, "ConversationStats")
	c.Eq(f.statsJSON, resp.Msg.GetStatsJson(), "stats_json")
	c.Require().NotNil(f.statsReq, "the seam was never called")
	if f.statsReq.GetOwner() != "u1" || f.statsReq.GetSinceUnix() != 42 {
		t.Errorf("seam got %+v, want the request fields intact", f.statsReq)
	}
}

// ─── Request-shape validation ─────────────────────────────────────────────────

func TestChildOpsResumeRequiresChildID(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).Resume(context.Background(),
		connect.NewRequest(&rafikiv1.ResumeRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestChildOpsSetLabelsRequiresChildID(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).SetLabels(context.Background(),
		connect.NewRequest(&rafikiv1.SetLabelsRequest{Set: map[string]string{"a": "b"}}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestChildOpsSetLabelsRequiresSetOrRemove(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).SetLabels(context.Background(),
		connect.NewRequest(&rafikiv1.SetLabelsRequest{ChildId: "c_1"}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestChildOpsSearchRequiresQuery(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).Search(context.Background(),
		connect.NewRequest(&rafikiv1.SearchRequest{Limit: 10}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestChildOpsModelInfoRequiresModel(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).ModelInfo(context.Background(),
		connect.NewRequest(&rafikiv1.ModelInfoRequest{}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// ─── Unwired fails closed ─────────────────────────────────────────────────────

// Every seam-reading handler in childops.go must answer CodeUnavailable —
// not Unimplemented, not a panic — before the daemon attaches the Controller.
// (ShutdownDaemon is not in this table: it refuses CodeUnimplemented before
// its seam is consulted this wave, pinned by
// TestChildOpsShutdownDaemonFailsClosedUnimplemented.) SetChildOps(nil) is
// refused, so an explicit nil store lands here too: this doubles as the
// handler-shaped pin newseams_test asks Wave 2 to grow.
func TestChildOpsUnwiredFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		call func(s *connectapi.Server) error
	}{
		{"Resume", func(s *connectapi.Server) error {
			_, err := s.Resume(context.Background(), connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: "c_1"}))
			return err
		}},
		{"CloseAllExited", func(s *connectapi.Server) error {
			_, err := s.CloseAllExited(context.Background(), connect.NewRequest(&rafikiv1.CloseAllExitedRequest{}))
			return err
		}},
		{"SetLabels", func(s *connectapi.Server) error {
			_, err := s.SetLabels(context.Background(),
				connect.NewRequest(&rafikiv1.SetLabelsRequest{ChildId: "c_1", Set: map[string]string{"a": "b"}}))
			return err
		}},
		{"Status", func(s *connectapi.Server) error {
			_, err := s.Status(context.Background(), connect.NewRequest(&rafikiv1.StatusRequest{}))
			return err
		}},
		{"Search", func(s *connectapi.Server) error {
			_, err := s.Search(context.Background(), connect.NewRequest(&rafikiv1.SearchRequest{Query: "q"}))
			return err
		}},
		{"ModelInfo", func(s *connectapi.Server) error {
			_, err := s.ModelInfo(context.Background(), connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: "m"}))
			return err
		}},
		{"ConversationStats", func(s *connectapi.Server) error {
			_, err := s.ConversationStats(context.Background(), connect.NewRequest(&rafikiv1.ConversationStatsRequest{}))
			return err
		}},
	}
	s := connectapi.NewServer(nil)
	s.SetChildOps(nil) // refused, same as never wired
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(s)
			assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code = %v, want Unavailable (err = %v)", connect.CodeOf(err), err)
		})
	}
}

// ─── Error mapping ────────────────────────────────────────────────────────────

// The code the daemon attached at the source IS the classification:
// Controller.Resume's ErrNotResumable reads as FailedPrecondition with the
// precise reason riding the detail, so a client can branch on not_resumable
// without parsing message text.
func TestChildOpsResumeNotResumableBecomesFailedPrecondition(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{resumeErr: &connectapi.ControllerError{
		Code:    protocol.ErrNotResumable,
		Message: "child is not exited (status: running)",
	}}
	_, err := newChildOpsServer(f).Resume(context.Background(),
		connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: "c_1"}))
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Eq("not_resumable", rpcreason.Reason(err), "reason")
	c.False(err == nil || !strings.Contains(err.Error(), "child is not exited"), "message = %v, want the daemon's authored text", err)
}

// A generic error — something that is not a ControllerError — keeps the
// blanket Internal, with the raw cause redacted: the peer sees only the fixed
// internal text, never infrastructure text like a pgx failure naming the
// database.
func TestChildOpsUncodedErrorIsRedactedInternal(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{exitedErr: errors.New("pq: relation does not exist")}
	_, err := newChildOpsServer(f).CloseAllExited(context.Background(),
		connect.NewRequest(&rafikiv1.CloseAllExitedRequest{}))
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "code")
	c.False(err == nil || strings.Contains(err.Error(), "relation does not exist"), "err.Error() = %v, want the raw cause redacted", err)
}

// The remaining handlers share mapChildOpsErr with CloseAllExited, so their
// uncoded-error path is pinned the same way: blanket Internal with the raw
// cause redacted — the peer never sees infrastructure text naming the
// database. (shutdownErr has no row here: the ShutdownDaemon handler refuses
// before the seam this wave, so its injector is unreachable.)
func TestChildOpsUncodedErrorsRedactedInternalPerHandler(t *testing.T) {
	cases := []struct {
		name   string
		inject func(f *fakeChildOps, err error)
		call   func(s *connectapi.Server) error
	}{
		{"SetLabels", func(f *fakeChildOps, err error) { f.labelsErr = err },
			func(s *connectapi.Server) error {
				_, err := s.SetLabels(context.Background(),
					connect.NewRequest(&rafikiv1.SetLabelsRequest{ChildId: "c_1", Set: map[string]string{"a": "b"}}))
				return err
			}},
		{"Status", func(f *fakeChildOps, err error) { f.statusErr = err },
			func(s *connectapi.Server) error {
				_, err := s.Status(context.Background(), connect.NewRequest(&rafikiv1.StatusRequest{}))
				return err
			}},
		{"Search", func(f *fakeChildOps, err error) { f.searchErr = err },
			func(s *connectapi.Server) error {
				_, err := s.Search(context.Background(), connect.NewRequest(&rafikiv1.SearchRequest{Query: "q"}))
				return err
			}},
		{"ModelInfo", func(f *fakeChildOps, err error) {
			f.modelOut = &rafikiv1.ModelInfoResponse{} // the fake echoes the model onto it before failing
			f.modelErr = err
		},
			func(s *connectapi.Server) error {
				_, err := s.ModelInfo(context.Background(), connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: "m"}))
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			f := &fakeChildOps{}
			tc.inject(f, errors.New("pq: relation does not exist"))
			err := tc.call(newChildOpsServer(f))
			c.Eq(connect.CodeInternal, connect.CodeOf(err), "code")
			c.False(err == nil || strings.Contains(err.Error(), "relation does not exist"), "err.Error() = %v, want the raw cause redacted", err)
		})
	}
}

// An already-coded *connect.Error — the ConversationStats adapter's scopeFor
// refusal — must reach the wire as it was coded, not be re-wrapped into
// internal by ConnectErr's non-ControllerError path.
func TestChildOpsCodedErrorPassesThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{statsErr: connect.NewError(connect.CodePermissionDenied,
		errors.New("conversation queries require a user credential"))}
	_, err := newChildOpsServer(f).ConversationStats(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationStatsRequest{}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
	c.False(err == nil || !strings.Contains(err.Error(), "require a user credential"), "message = %v, want the authored text", err)
}

// The authored ControllerError text rides the FailedPrecondition on the
// conversation stats face too, same as resume: ErrNoAgentDB is the daemon
// configuration gap the Controller writes a curated message for.
func TestChildOpsConversationStatsNoAgentDBKeepsAuthoredText(t *testing.T) {
	c := assert.NewCollecting(t)
	f := &fakeChildOps{statsErr: &connectapi.ControllerError{
		Code:    protocol.ErrNoAgentDB,
		Message: "no agent database configured (RAFIKI_DB unset)",
	}}
	_, err := newChildOpsServer(f).ConversationStats(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationStatsRequest{}))
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
	c.False(err == nil || !strings.Contains(err.Error(), "no agent database configured"), "message = %v, want the daemon's authored text", err)
}
