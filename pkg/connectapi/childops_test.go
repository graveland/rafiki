// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
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
	resumeID    string
	resumeErr   error
	exitedIDs   []string
	exitedErr   error
	labelsOut   map[string]string
	labelsErr   error
	statusOut   *rafikiv1.StatusResponse
	statusErr   error
	searchOut   *rafikiv1.SearchResponse
	searchErr   error
	shutdownErr error
	modelOut    *rafikiv1.ModelInfoResponse
	modelErr    error
	statsJSON   string
	statsErr    error
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
		return nil, f.exitedErr
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

func (f *fakeChildOps) ShutdownDaemon(_ context.Context) error {
	f.shutdownCalled = true
	return f.shutdownErr
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
	f := &fakeChildOps{resumeID: "c_resumed"}
	resp, err := newChildOpsServer(f).Resume(context.Background(),
		connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: "c_exit", ApiKey: "sk-key"}))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if f.resumeChildID != "c_exit" || f.resumeAPIKey != "sk-key" {
		t.Errorf("seam got childID=%q apiKey=%q, want c_exit/sk-key", f.resumeChildID, f.resumeAPIKey)
	}
	if resp.Msg.GetChildId() != "c_resumed" {
		t.Errorf("resp child_id = %q, want c_resumed", resp.Msg.GetChildId())
	}
}

func TestChildOpsCloseAllExitedPassesOlderThanThrough(t *testing.T) {
	f := &fakeChildOps{exitedIDs: []string{"c_a", "c_b"}}
	resp, err := newChildOpsServer(f).CloseAllExited(context.Background(),
		connect.NewRequest(&rafikiv1.CloseAllExitedRequest{OlderThanMs: 5000}))
	if err != nil {
		t.Fatalf("CloseAllExited: %v", err)
	}
	if f.exitedOlderMs != 5000 {
		t.Errorf("seam got older_than_ms = %d, want 5000", f.exitedOlderMs)
	}
	if len(resp.Msg.GetChildIds()) != 2 || resp.Msg.GetChildIds()[0] != "c_a" {
		t.Errorf("resp child_ids = %v, want [c_a c_b]", resp.Msg.GetChildIds())
	}
}

func TestChildOpsSetLabelsPassesSetAndRemoveThrough(t *testing.T) {
	f := &fakeChildOps{labelsOut: map[string]string{"team": "core"}}
	resp, err := newChildOpsServer(f).SetLabels(context.Background(),
		connect.NewRequest(&rafikiv1.SetLabelsRequest{
			ChildId: "c_1",
			Set:     map[string]string{"team": "core"},
			Remove:  []string{"stale"},
		}))
	if err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	if f.labelsChildID != "c_1" || f.labelsSet["team"] != "core" || len(f.labelsRemove) != 1 || f.labelsRemove[0] != "stale" {
		t.Errorf("seam got childID=%q set=%v remove=%v, want c_1/{team:core}/[stale]", f.labelsChildID, f.labelsSet, f.labelsRemove)
	}
	if resp.Msg.GetLabels()["team"] != "core" {
		t.Errorf("resp labels = %v, want team=core", resp.Msg.GetLabels())
	}
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
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if resp.Msg.GetVersion() != "test" || resp.Msg.GetStartedAt() != 1234 ||
		resp.Msg.GetChildren().GetLive() != 2 || resp.Msg.GetChildren().GetExited() != 1 ||
		resp.Msg.GetMemoryBytes() != 64 || resp.Msg.GetSocket() != "/tmp/s.sock" || resp.Msg.GetLogsDir() != "/tmp/logs" {
		t.Errorf("resp = %+v, want %+v", resp.Msg, want)
	}
}

func TestChildOpsSearchReceivesTheRequest(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if f.searchReq == nil {
		t.Fatal("the seam was never called")
	}
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

func TestChildOpsShutdownDaemonAnswersAndCallsThrough(t *testing.T) {
	f := &fakeChildOps{}
	resp, err := newChildOpsServer(f).ShutdownDaemon(context.Background(),
		connect.NewRequest(&rafikiv1.ShutdownDaemonRequest{}))
	if err != nil {
		t.Fatalf("ShutdownDaemon: %v", err)
	}
	if resp.Msg == nil {
		t.Error("resp = nil, want an empty ShutdownDaemonResponse")
	}
	if !f.shutdownCalled {
		t.Error("the seam's ShutdownDaemon was never called")
	}
}

func TestChildOpsModelInfoReturnsTheSeamAnswer(t *testing.T) {
	f := &fakeChildOps{modelOut: &rafikiv1.ModelInfoResponse{
		ResolvedId:          "prov/real-id",
		ContextWindow:       200000,
		MaxCompletionTokens: 32000,
		AutoCompactWindow:   180000,
		Known:               true,
	}}
	resp, err := newChildOpsServer(f).ModelInfo(context.Background(),
		connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: "prov/alias"}))
	if err != nil {
		t.Fatalf("ModelInfo: %v", err)
	}
	m := resp.Msg
	if m.GetModel() != "prov/alias" || m.GetResolvedId() != "prov/real-id" ||
		m.GetContextWindow() != 200000 || m.GetMaxCompletionTokens() != 32000 ||
		m.GetAutoCompactWindow() != 180000 || !m.GetKnown() {
		t.Errorf("resp = %+v, want the seam answer with the requested model echoed", m)
	}
}

func TestChildOpsConversationStatsWrapsTheJSON(t *testing.T) {
	f := &fakeChildOps{statsJSON: `{"volume":{"conversations":3}}`}
	resp, err := newChildOpsServer(f).ConversationStats(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationStatsRequest{Owner: "u1", SinceUnix: 42}))
	if err != nil {
		t.Fatalf("ConversationStats: %v", err)
	}
	if resp.Msg.GetStatsJson() != f.statsJSON {
		t.Errorf("stats_json = %q, want the seam JSON verbatim", resp.Msg.GetStatsJson())
	}
	if f.statsReq == nil {
		t.Fatal("the seam was never called")
	}
	if f.statsReq.GetOwner() != "u1" || f.statsReq.GetSinceUnix() != 42 {
		t.Errorf("seam got %+v, want the request fields intact", f.statsReq)
	}
}

// ─── Request-shape validation ─────────────────────────────────────────────────

func TestChildOpsResumeRequiresChildID(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).Resume(context.Background(),
		connect.NewRequest(&rafikiv1.ResumeRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestChildOpsSetLabelsRequiresChildID(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).SetLabels(context.Background(),
		connect.NewRequest(&rafikiv1.SetLabelsRequest{Set: map[string]string{"a": "b"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestChildOpsSetLabelsRequiresSetOrRemove(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).SetLabels(context.Background(),
		connect.NewRequest(&rafikiv1.SetLabelsRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestChildOpsSearchRequiresQuery(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).Search(context.Background(),
		connect.NewRequest(&rafikiv1.SearchRequest{Limit: 10}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestChildOpsModelInfoRequiresModel(t *testing.T) {
	_, err := newChildOpsServer(&fakeChildOps{}).ModelInfo(context.Background(),
		connect.NewRequest(&rafikiv1.ModelInfoRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// ─── Unwired fails closed ─────────────────────────────────────────────────────

// Every handler in childops.go reads the same seam, so every one of them must
// answer CodeUnavailable — not Unimplemented, not a panic — before the daemon
// attaches the Controller. SetChildOps(nil) is refused, so an explicit nil
// store lands here too: this doubles as the handler-shaped pin newseams_test
// asks Wave 2 to grow.
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
		{"ShutdownDaemon", func(s *connectapi.Server) error {
			_, err := s.ShutdownDaemon(context.Background(), connect.NewRequest(&rafikiv1.ShutdownDaemonRequest{}))
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
			if connect.CodeOf(err) != connect.CodeUnavailable {
				t.Errorf("code = %v, want Unavailable (err = %v)", connect.CodeOf(err), err)
			}
		})
	}
}

// ─── Error mapping ────────────────────────────────────────────────────────────

// The code the daemon attached at the source IS the classification:
// Controller.Resume's ErrNotResumable reads as FailedPrecondition with the
// precise reason riding the detail, so a client can branch on not_resumable
// without parsing message text.
func TestChildOpsResumeNotResumableBecomesFailedPrecondition(t *testing.T) {
	f := &fakeChildOps{resumeErr: &control.ControllerError{
		Code:    protocol.ErrNotResumable,
		Message: "child is not exited (status: running)",
	}}
	_, err := newChildOpsServer(f).Resume(context.Background(),
		connect.NewRequest(&rafikiv1.ResumeRequest{ChildId: "c_1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if got := rpcreason.Reason(err); got != "not_resumable" {
		t.Errorf("reason = %q, want not_resumable", got)
	}
	if err == nil || !strings.Contains(err.Error(), "child is not exited") {
		t.Errorf("message = %v, want the daemon's authored text", err)
	}
}

// A generic error — something that is not a ControllerError — keeps the
// blanket Internal, with the raw cause redacted: the peer sees only the fixed
// internal text, never infrastructure text like a pgx failure naming the
// database.
func TestChildOpsUncodedErrorIsRedactedInternal(t *testing.T) {
	f := &fakeChildOps{exitedErr: errors.New("pq: relation does not exist")}
	_, err := newChildOpsServer(f).CloseAllExited(context.Background(),
		connect.NewRequest(&rafikiv1.CloseAllExitedRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
	if err == nil || strings.Contains(err.Error(), "relation does not exist") {
		t.Errorf("err.Error() = %v, want the raw cause redacted", err)
	}
}

// An already-coded *connect.Error — the ConversationStats adapter's scopeFor
// refusal — must reach the wire as it was coded, not be re-wrapped into
// internal by ConnectErr's non-ControllerError path.
func TestChildOpsCodedErrorPassesThrough(t *testing.T) {
	f := &fakeChildOps{statsErr: connect.NewError(connect.CodePermissionDenied,
		errors.New("conversation queries require a user credential"))}
	_, err := newChildOpsServer(f).ConversationStats(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationStatsRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
	if err == nil || !strings.Contains(err.Error(), "require a user credential") {
		t.Errorf("message = %v, want the authored text", err)
	}
}

// The authored ControllerError text rides the FailedPrecondition on the
// conversation stats face too, same as resume: ErrNoAgentDB is the daemon
// configuration gap the Controller writes a curated message for.
func TestChildOpsConversationStatsNoAgentDBKeepsAuthoredText(t *testing.T) {
	f := &fakeChildOps{statsErr: &control.ControllerError{
		Code:    protocol.ErrNoAgentDB,
		Message: "no agent database configured (RAFIKI_DB unset)",
	}}
	_, err := newChildOpsServer(f).ConversationStats(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationStatsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable (errmap's ErrNoAgentDB row)", connect.CodeOf(err))
	}
	if err == nil || !strings.Contains(err.Error(), "no agent database configured") {
		t.Errorf("message = %v, want the daemon's authored text", err)
	}
}
