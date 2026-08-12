// Package server dispatch routes ctrl_* frames to typed handlers backed by a
// Controller interface, so the logic is testable without the real daemon.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ─── ControllerError ──────────────────────────────────────────────────────────

// ControllerError carries a machine-readable protocol error code from a
// Controller method, letting dispatch translate it to the correct
// ctrl_response error body without inspecting error strings.
type ControllerError struct {
	Code    string
	Message string
}

func (e *ControllerError) Error() string { return e.Message }

// ─── Per-call types ───────────────────────────────────────────────────────────

// SpawnResult is returned by Controller.Spawn and Controller.Resume.
type SpawnResult = protocol.SpawnResponseData

// KillResult is returned by Controller.Kill.
type KillResult = protocol.KillResponseData

// RecentQuery carries parameters for Controller.GetRecent.
type RecentQuery struct {
	Limit    int
	Since    int64
	Include  []string
	Exclude  []string
	Rendered bool
}

// RecentResult is returned by Controller.GetRecent.
type RecentResult = protocol.GetRecentResponseData

// GetStreamsResult is returned by Controller.GetStreams.
type GetStreamsResult = protocol.GetStreamsResponseData

// SearchQuery carries parameters for Controller.Search.
type SearchQuery struct {
	Query         string
	Regex         bool
	Limit         int
	Context       int
	SessionFilter protocol.SearchSessionFilter
}

// SearchResult is returned by Controller.Search.
type SearchResult = protocol.SearchResponseData

// ControllerStatus is returned by Controller.Status.
type ControllerStatus = protocol.StatusResponseData

// ─── Controller interface ─────────────────────────────────────────────────────

// Controller is the surface dispatch needs from the rest of the daemon.
// The real implementation lives in cmd/rafikid; tests use a fake.
type Controller interface {
	// Read-only queries.
	List(filter protocol.ListFilter) []childstore.Snapshot
	Get(childID string) (childstore.Snapshot, bool)
	GetRecent(childID string, q RecentQuery) (RecentResult, error)
	// GetStreams returns a live child's in-memory stdin/stderr capture.
	// Returns Alive:false (no error) when the child has exited.
	GetStreams(childID string, which string) (GetStreamsResult, error)
	Search(q SearchQuery) SearchResult
	Status() ControllerStatus

	// Conversation insights, backed by the daemon's agent database.
	// Implementations return a *ControllerError with Code: protocol.ErrNoAgentDB
	// when no database is configured (RAFIKI_DB unset).
	ConversationStats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error)
	ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error)
	ConversationSearch(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error)
	ConversationExport(ctx context.Context, id string) (*insights.Transcript, error)

	// Lifecycle mutations.
	Spawn(ctx context.Context, req protocol.SpawnRequest) (SpawnResult, error)
	Resume(ctx context.Context, childID string, apiKey string) (SpawnResult, error)
	Kill(ctx context.Context, childID string, shutdownTimeoutMs, killTimeoutMs int64) (KillResult, error)
	Forget(childID string) error
	ForgetAllExited(olderThanMs int64) (int, error)

	// Frame forwarding.
	Send(childID string, frame json.RawMessage) error

	// SetLabels mutates labels on the named child. Returns the full post-mutation
	// map. Rejects keys with the fundi/ prefix and invalid key characters.
	SetLabels(childID string, set map[string]string, remove []string) (map[string]string, error)

	// ListModels enumerates LLM models from all configured sources (user-config,
	// builtin list, Ollama, LM Studio). When provider is non-empty only models
	// from that provider are returned. Best-effort: missing or unreachable
	// sources return no entries rather than errors.
	ListModels(ctx context.Context, provider string) ([]protocol.ModelInfo, error)

	// ContextWindow resolves model's context window and max completion
	// tokens from the daemon's shared model catalog, for ChildSummary's
	// ContextWindow/MaxCompletionTokens fields. ok is false when no catalog
	// is configured or model isn't in the current snapshot — callers must
	// leave those fields unset rather than publish a false zero.
	ContextWindow(model string) (contextLen, maxCompletion int, ok bool)

	// ListPresets enumerates presets from <config dir>/presets.json (paths.PresetsFile()).
	// labels and hasLabel apply the same AND-match filter semantics as ctrl_list.
	ListPresets(labels map[string]string, hasLabel []string) ([]protocol.PresetInfo, error)

	// Per-connection subscriptions. conn is the Connection passed to
	// FrameHandler; it serves as both the event-delivery target and the
	// identity key for subsequent Unsubscribe calls.
	Subscribe(childID string, conn Connection, filter protocol.SubscribeFilter) error
	Unsubscribe(childID string, conn Connection) error
	GlobalSubscribe(conn Connection) error
	GlobalUnsubscribe(conn Connection) error

	// SubscribeLabeled subscribes conn to events from every child whose labels
	// match the given filter (AND-match across labels; key-presence for
	// hasLabel).  Delivery is dynamic: matching starts or stops as labels
	// change.  Cleanup occurs when OnConnectionClose is called for conn.
	SubscribeLabeled(conn Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error

	// OnConnectionClose is called when a client connection closes. The
	// controller removes any subscriptions (global and per-child) held by
	// this connection so they do not leak.
	OnConnectionClose(conn Connection)

	// ─── Executor management ────────────────────────────────────────────────

	// ExecutorEnroll mints a one-time enrollment token for a new executor.
	ExecutorEnroll(req protocol.ExecutorEnrollRequest) (protocol.ExecutorEnrollResponseData, error)
	// ExecutorList returns enrolled executors, optionally filtered.
	ExecutorList(req protocol.ExecutorListRequest) ([]executors.Executor, error)
	// ExecutorLabel sets or removes labels on an executor row.
	ExecutorLabel(req protocol.ExecutorLabelRequest) (executors.Executor, error)
	// ExecutorDisable disables an executor.
	ExecutorDisable(req protocol.ExecutorDisableRequest) error
	// ExecutorEnable re-enables a disabled executor.
	ExecutorEnable(req protocol.ExecutorEnableRequest) error
}

// ─── Dispatch factory ─────────────────────────────────────────────────────────

// NewDispatch returns a ConnectionLifecycleHandler that routes ctrl_* frames
// to typed handlers backed by c, and notifies c when connections close.
// The returned handler is safe for concurrent use.
func NewDispatch(c Controller) ConnectionLifecycleHandler {
	return &dispatcher{c: c}
}

type dispatcher struct{ c Controller }

// HandleFrame implements ConnectionLifecycleHandler.
func (d *dispatcher) HandleFrame(conn Connection, frame []byte) []byte {
	return d.handle(conn, frame)
}

// HandleClose implements ConnectionLifecycleHandler. It notifies the
// controller so it can release any subscriptions held by this connection.
func (d *dispatcher) HandleClose(conn Connection) {
	d.c.OnConnectionClose(conn)
}

func (d *dispatcher) handle(conn Connection, frame []byte) []byte {
	var hdr struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return errResponse("", "", protocol.ErrInvalidArgs, "malformed JSON")
	}
	switch hdr.Type {
	case protocol.TypeCtrlList:
		return d.list(frame, hdr.ID)
	case protocol.TypeCtrlGet:
		return d.get(frame, hdr.ID)
	case protocol.TypeCtrlSpawn:
		return d.spawn(frame, hdr.ID)
	case protocol.TypeCtrlResume:
		return d.resume(frame, hdr.ID)
	case protocol.TypeCtrlKill:
		return d.kill(frame, hdr.ID)
	case protocol.TypeCtrlForget:
		return d.forget(frame, hdr.ID)
	case protocol.TypeCtrlForgetAllExited:
		return d.forgetAllExited(frame, hdr.ID)
	case protocol.TypeCtrlGetRecent:
		return d.getRecent(frame, hdr.ID)
	case protocol.TypeCtrlGetStreams:
		return d.getStreams(frame, hdr.ID)
	case protocol.TypeCtrlSearch:
		return d.search(frame, hdr.ID)
	case protocol.TypeCtrlStatus:
		return d.ctrlStatus(frame, hdr.ID)
	case protocol.TypeCtrlConversationStats:
		return d.conversationStats(frame, hdr.ID)
	case protocol.TypeCtrlConversationSearch:
		return d.conversationSearch(frame, hdr.ID)
	case protocol.TypeCtrlConversationExport:
		return d.conversationExport(frame, hdr.ID)
	case protocol.TypeCtrlSend:
		return d.ctrlSend(frame, hdr.ID)
	case protocol.TypeCtrlSetLabels:
		return d.setLabels(frame, hdr.ID)
	case protocol.TypeCtrlListModels:
		return d.listModels(frame, hdr.ID)
	case protocol.TypeCtrlListPresets:
		return d.listPresets(frame, hdr.ID)
	case protocol.TypeCtrlSubscribe:
		return d.subscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlUnsubscribe:
		return d.unsubscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlGlobalSubscribe:
		return d.globalSubscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlGlobalUnsubscribe:
		return d.globalUnsubscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlExecutorEnroll:
		return d.executorEnroll(frame, hdr.ID)
	case protocol.TypeCtrlExecutorList:
		return d.executorList(frame, hdr.ID)
	case protocol.TypeCtrlExecutorLabel:
		return d.executorLabel(frame, hdr.ID)
	case protocol.TypeCtrlExecutorDisable:
		return d.executorDisable(frame, hdr.ID)
	case protocol.TypeCtrlExecutorEnable:
		return d.executorEnable(frame, hdr.ID)
	default:
		return errResponse(hdr.Type, hdr.ID, protocol.ErrInvalidArgs, "unknown command type: "+hdr.Type)
	}
}

// ─── Response helpers ─────────────────────────────────────────────────────────

// okResponse builds a success ctrl_response. When data is nil the data field
// is omitted from the wire frame (json.RawMessage omitempty).
func okResponse(cmd, id string, data any) []byte {
	resp := protocol.Response{
		Type:    protocol.TypeCtrlResponse,
		Command: cmd,
		ID:      id,
		Success: true,
	}
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			resp.Data = b
		}
	}
	b, _ := json.Marshal(resp)
	return b
}

// errResponse builds an error ctrl_response.
func errResponse(cmd, id, code, msg string) []byte {
	resp := protocol.Response{
		Type:    protocol.TypeCtrlResponse,
		Command: cmd,
		ID:      id,
		Success: false,
		Error:   &protocol.ErrorBody{Code: code, Message: msg},
	}
	b, _ := json.Marshal(resp)
	return b
}

// mapErr translates a Controller error to a ctrl_response. If err is a
// *ControllerError, its Code is used; otherwise fallbackCode is used (or
// ErrInternal when empty).
func mapErr(cmd, id string, err error, fallbackCode string) []byte {
	var ce *ControllerError
	if errors.As(err, &ce) {
		return errResponse(cmd, id, ce.Code, ce.Message)
	}
	if fallbackCode == "" {
		fallbackCode = protocol.ErrInternal
	}
	return errResponse(cmd, id, fallbackCode, err.Error())
}

// ─── snapshotToSummary ────────────────────────────────────────────────────────

// snapshotToSummary converts a childstore.Snapshot to the wire ChildSummary shape.
// PID is omitted (nil) when the child has exited. Model is formatted as
// "provider/model" when both fields are present.
//
// contextWindow, when non-nil, is consulted for the resolved Model to fill
// ContextWindow/MaxCompletionTokens — a func rather than a *routing.ModelCatalog
// so this package (and its tests) need not depend on pkg/routing; callers pass
// Controller.ContextWindow bound to the real implementation. A nil func or a
// false ok leaves both fields at their zero value (omitted on the wire).
func snapshotToSummary(snap childstore.Snapshot, contextWindow func(model string) (contextLen, maxCompletion int, ok bool)) protocol.ChildSummary {
	model := snap.Model
	if snap.Provider != "" && snap.Model != "" {
		model = snap.Provider + "/" + snap.Model
	}
	var pid *int
	if snap.Status != protocol.StatusExited {
		pid = &snap.PID
	}
	cs := protocol.ChildSummary{
		ChildID:      snap.ChildID,
		PID:          pid,
		Cwd:          snap.Cwd,
		Name:         snap.Name,
		Kind:         snap.Kind,
		Model:        model,
		SessionID:    snap.SessionID,
		SessionFile:  snap.SessionFile,
		Status:       string(snap.Status),
		StartedAt:    snap.StartedAt.UnixMilli(),
		LastActivity: snap.LastActivity.UnixMilli(),
		ExitCode:     snap.ExitCode,
		ExitSignal:   snap.ExitSignal,
	}
	if len(snap.Labels) > 0 {
		cs.Labels = snap.Labels
	}
	if len(snap.SlashCommands) > 0 {
		cs.SlashCommands = snap.SlashCommands
	}
	if contextWindow != nil && model != "" {
		if cl, mc, ok := contextWindow(model); ok {
			cs.ContextWindow = cl
			cs.MaxCompletionTokens = mc
		}
	}
	return cs
}

// ─── Read-only handlers ───────────────────────────────────────────────────────

func (d *dispatcher) list(frame []byte, id string) []byte {
	var req protocol.ListRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlList, id, protocol.ErrInvalidArgs, "malformed request")
	}
	var filter protocol.ListFilter
	if req.Filter != nil {
		filter = *req.Filter
	}
	snaps := d.c.List(filter)
	children := make([]protocol.ChildSummary, len(snaps))
	for i, s := range snaps {
		children[i] = snapshotToSummary(s, d.c.ContextWindow)
	}
	return okResponse(protocol.TypeCtrlList, id, protocol.ListResponseData{Children: children})
}

func (d *dispatcher) get(frame []byte, id string) []byte {
	var req protocol.GetRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlGet, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlGet, id, protocol.ErrInvalidArgs, "childId required")
	}
	snap, ok := d.c.Get(req.ChildID)
	if !ok {
		return errResponse(protocol.TypeCtrlGet, id, protocol.ErrChildNotFound, "child not found: "+req.ChildID)
	}
	return okResponse(protocol.TypeCtrlGet, id, snapshotToSummary(snap, d.c.ContextWindow))
}

func (d *dispatcher) getRecent(frame []byte, id string) []byte {
	var req protocol.GetRecentRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlGetRecent, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlGetRecent, id, protocol.ErrInvalidArgs, "childId required")
	}
	q := RecentQuery{
		Limit:    req.Limit,
		Since:    req.Since,
		Include:  req.Include,
		Exclude:  req.Exclude,
		Rendered: req.Rendered,
	}
	result, err := d.c.GetRecent(req.ChildID, q)
	if err != nil {
		return mapErr(protocol.TypeCtrlGetRecent, id, err, protocol.ErrChildNotFound)
	}
	// Ensure events is always an array, never null.
	if result.Events == nil {
		result.Events = []json.RawMessage{}
	}
	return okResponse(protocol.TypeCtrlGetRecent, id, result)
}

func (d *dispatcher) getStreams(frame []byte, id string) []byte {
	var req protocol.GetStreamsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlGetStreams, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlGetStreams, id, protocol.ErrInvalidArgs, "childId required")
	}
	switch req.Which {
	case "", "in", "err", "all":
	default:
		return errResponse(protocol.TypeCtrlGetStreams, id, protocol.ErrInvalidArgs, `which must be one of "in", "err", "all"`)
	}
	result, err := d.c.GetStreams(req.ChildID, req.Which)
	if err != nil {
		return mapErr(protocol.TypeCtrlGetStreams, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlGetStreams, id, result)
}

func (d *dispatcher) search(frame []byte, id string) []byte {
	var req protocol.SearchRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlSearch, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.Query == "" {
		return errResponse(protocol.TypeCtrlSearch, id, protocol.ErrInvalidArgs, "query required")
	}
	var sf protocol.SearchSessionFilter
	if req.SessionFilter != nil {
		sf = *req.SessionFilter
	}
	q := SearchQuery{
		Query:         req.Query,
		Regex:         req.Regex,
		Limit:         req.Limit,
		Context:       req.Context,
		SessionFilter: sf,
	}
	result := d.c.Search(q)
	// Ensure hits is always an array, never null.
	if result.Hits == nil {
		result.Hits = []protocol.SearchHit{}
	}
	return okResponse(protocol.TypeCtrlSearch, id, result)
}

func (d *dispatcher) ctrlStatus(_ []byte, id string) []byte {
	return okResponse(protocol.TypeCtrlStatus, id, d.c.Status())
}

// ─── Conversation insight handlers ────────────────────────────────────────────

// conversationQueryTimeout bounds Controller calls made by the three
// conversation-insight handlers below. Every other handler in this file uses
// a bare context.Background(), matching the daemon's existing convention —
// but these three run unbounded aggregate queries against the daemon's
// shared agent-database pool (also handed to every in-process agent child),
// so a slow query must not be left uncancellable just because the requesting
// client disconnected.
const conversationQueryTimeout = 30 * time.Second

// maxConversationSearchLimit bounds ctrl_conversation_search's effective row
// limit so a large or absent client-supplied limit can't produce a response
// that risks protocol.MaxFrameBytes. insights.SearchFilter already floors
// Limit<=0 to a default of 50 internally; this only adds the missing ceiling.
const maxConversationSearchLimit = 500

// conversationExportSizeBudget bounds the marshaled size of a
// ctrl_conversation_export response, leaving headroom below
// protocol.MaxFrameBytes for the response envelope.
const conversationExportSizeBudget = protocol.MaxFrameBytes - 4096

// unixToTime converts a wire Unix-seconds value to *time.Time, treating 0 as
// unset — matches insights.StatsFilter/SearchFilter's nil-means-unbounded
// convention for Since/Until.
func unixToTime(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

func (d *dispatcher) conversationStats(frame []byte, id string) []byte {
	var req protocol.ConversationStatsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlConversationStats, id, protocol.ErrInvalidArgs, "malformed request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), conversationQueryTimeout)
	defer cancel()
	if req.ConversationID != "" {
		st, err := d.c.ConversationStatsByID(ctx, req.ConversationID)
		if err != nil {
			return mapErr(protocol.TypeCtrlConversationStats, id, err, protocol.ErrInternal)
		}
		return okResponse(protocol.TypeCtrlConversationStats, id, st)
	}
	f := insights.StatsFilter{
		Since:   unixToTime(req.SinceUnix),
		Until:   unixToTime(req.UntilUnix),
		Owner:   req.Owner,
		Persona: req.Persona,
		Source:  req.Source,
		Model:   req.Model,
		Path:    insights.Path(req.Path),
	}
	st, err := d.c.ConversationStats(ctx, f)
	if err != nil {
		return mapErr(protocol.TypeCtrlConversationStats, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlConversationStats, id, st)
}

func (d *dispatcher) conversationSearch(frame []byte, id string) []byte {
	var req protocol.ConversationSearchRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlConversationSearch, id, protocol.ErrInvalidArgs, "malformed request")
	}
	limit := req.Limit
	if limit > maxConversationSearchLimit {
		limit = maxConversationSearchLimit
	}
	f := insights.SearchFilter{
		Since:     unixToTime(req.SinceUnix),
		Until:     unixToTime(req.UntilUnix),
		Owner:     req.Owner,
		Persona:   req.Persona,
		Source:    req.Source,
		Model:     req.Model,
		Status:    req.Status,
		Path:      insights.Path(req.Path),
		MinTokens: req.MinTokens,
		Text:      req.Text,
		Limit:     limit,
	}
	ctx, cancel := context.WithTimeout(context.Background(), conversationQueryTimeout)
	defer cancel()
	rows, err := d.c.ConversationSearch(ctx, f)
	if err != nil {
		return mapErr(protocol.TypeCtrlConversationSearch, id, err, protocol.ErrInternal)
	}
	if rows == nil {
		rows = []insights.ConversationSummary{}
	}
	return okResponse(protocol.TypeCtrlConversationSearch, id, struct {
		Rows []insights.ConversationSummary `json:"rows"`
	}{Rows: rows})
}

func (d *dispatcher) conversationExport(frame []byte, id string) []byte {
	var req protocol.ConversationExportRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlConversationExport, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ConversationID == "" {
		return errResponse(protocol.TypeCtrlConversationExport, id, protocol.ErrInvalidArgs, "conversationId required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), conversationQueryTimeout)
	defer cancel()
	tr, err := d.c.ConversationExport(ctx, req.ConversationID)
	if err != nil {
		return mapErr(protocol.TypeCtrlConversationExport, id, err, protocol.ErrInternal)
	}
	if b, err := json.Marshal(tr); err == nil && len(b) > conversationExportSizeBudget {
		return errResponse(protocol.TypeCtrlConversationExport, id, protocol.ErrPayloadTooLarge,
			"transcript exceeds the maximum response size; export it via `rafiki agent export` instead")
	}
	return okResponse(protocol.TypeCtrlConversationExport, id, tr)
}

// ─── Lifecycle handlers ───────────────────────────────────────────────────────

func (d *dispatcher) spawn(frame []byte, id string) []byte {
	var req protocol.SpawnRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlSpawn, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.Cwd == "" {
		return errResponse(protocol.TypeCtrlSpawn, id, protocol.ErrInvalidArgs, "cwd required")
	}
	if !filepath.IsAbs(req.Cwd) {
		return errResponse(protocol.TypeCtrlSpawn, id, protocol.ErrInvalidArgs, "cwd must be an absolute path")
	}
	result, err := d.c.Spawn(context.Background(), req)
	if err != nil {
		return mapErr(protocol.TypeCtrlSpawn, id, err, protocol.ErrSpawnFailed)
	}
	return okResponse(protocol.TypeCtrlSpawn, id, result)
}

func (d *dispatcher) resume(frame []byte, id string) []byte {
	var req protocol.ResumeRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlResume, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlResume, id, protocol.ErrInvalidArgs, "childId required")
	}
	result, err := d.c.Resume(context.Background(), req.ChildID, req.APIKey)
	if err != nil {
		return mapErr(protocol.TypeCtrlResume, id, err, protocol.ErrNotFound)
	}
	return okResponse(protocol.TypeCtrlResume, id, result)
}

func (d *dispatcher) kill(frame []byte, id string) []byte {
	var req protocol.KillRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlKill, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlKill, id, protocol.ErrInvalidArgs, "childId required")
	}
	result, err := d.c.Kill(context.Background(), req.ChildID, req.ShutdownTimeoutMs, req.KillTimeoutMs)
	if err != nil {
		return mapErr(protocol.TypeCtrlKill, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlKill, id, result)
}

func (d *dispatcher) forget(frame []byte, id string) []byte {
	var req protocol.ForgetRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlForget, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlForget, id, protocol.ErrInvalidArgs, "childId required")
	}
	if err := d.c.Forget(req.ChildID); err != nil {
		return mapErr(protocol.TypeCtrlForget, id, err, protocol.ErrNotFound)
	}
	return okResponse(protocol.TypeCtrlForget, id, nil)
}

func (d *dispatcher) forgetAllExited(frame []byte, id string) []byte {
	var req protocol.ForgetAllExitedRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlForgetAllExited, id, protocol.ErrInvalidArgs, "malformed request")
	}
	count, err := d.c.ForgetAllExited(req.OlderThanMs)
	if err != nil {
		return mapErr(protocol.TypeCtrlForgetAllExited, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlForgetAllExited, id, protocol.ForgetAllExitedResponseData{Count: count})
}

// ─── Enumeration handlers ────────────────────────────────────────────────────

func (d *dispatcher) listModels(frame []byte, id string) []byte {
	var req protocol.ListModelsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlListModels, id, protocol.ErrInvalidArgs, "malformed request")
	}
	infos, err := d.c.ListModels(context.Background(), req.Provider)
	if err != nil {
		return mapErr(protocol.TypeCtrlListModels, id, err, protocol.ErrInternal)
	}
	if infos == nil {
		infos = []protocol.ModelInfo{}
	}
	return okResponse(protocol.TypeCtrlListModels, id, protocol.ListModelsResponseData{Models: infos})
}

func (d *dispatcher) listPresets(frame []byte, id string) []byte {
	var req protocol.ListPresetsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlListPresets, id, protocol.ErrInvalidArgs, "malformed request")
	}
	presets, err := d.c.ListPresets(req.Labels, req.HasLabel)
	if err != nil {
		return mapErr(protocol.TypeCtrlListPresets, id, err, protocol.ErrInternal)
	}
	if presets == nil {
		presets = []protocol.PresetInfo{}
	}
	return okResponse(protocol.TypeCtrlListPresets, id, protocol.ListPresetsResponseData{Presets: presets})
}

// ─── Labels handler ───────────────────────────────────────────────────────────

func (d *dispatcher) setLabels(frame []byte, id string) []byte {
	var req protocol.SetLabelsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlSetLabels, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlSetLabels, id, protocol.ErrInvalidArgs, "childId required")
	}
	if len(req.Set) == 0 && len(req.Remove) == 0 {
		return errResponse(protocol.TypeCtrlSetLabels, id, protocol.ErrInvalidArgs, "at least one of set or remove is required")
	}
	merged, err := d.c.SetLabels(req.ChildID, req.Set, req.Remove)
	if err != nil {
		return mapErr(protocol.TypeCtrlSetLabels, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlSetLabels, id, protocol.SetLabelsResponseData{Labels: merged})
}

// ─── Frame-forwarding handler ─────────────────────────────────────────────────

func (d *dispatcher) ctrlSend(frame []byte, id string) []byte {
	var req protocol.SendRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlSend, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlSend, id, protocol.ErrInvalidArgs, "childId required")
	}
	if len(req.Frame) == 0 {
		return errResponse(protocol.TypeCtrlSend, id, protocol.ErrInvalidArgs, "frame required")
	}
	if err := d.c.Send(req.ChildID, req.Frame); err != nil {
		return mapErr(protocol.TypeCtrlSend, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlSend, id, nil)
}

// ─── Subscription handlers ────────────────────────────────────────────────────

func (d *dispatcher) subscribe(conn Connection, frame []byte, id string) []byte {
	var req protocol.SubscribeRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlSubscribe, id, protocol.ErrInvalidArgs, "malformed request")
	}

	// childId and label filter are mutually exclusive.
	if req.ChildID != "" && (len(req.Labels) > 0 || len(req.HasLabel) > 0) {
		return errResponse(protocol.TypeCtrlSubscribe, id, protocol.ErrInvalidArgs,
			"subscribe: childId and labels are mutually exclusive")
	}

	var filter protocol.SubscribeFilter
	if req.Filter != nil {
		filter = *req.Filter
	}

	// Label-filtered subscription mode.
	if len(req.Labels) > 0 || len(req.HasLabel) > 0 {
		if err := d.c.SubscribeLabeled(conn, req.Labels, req.HasLabel, filter); err != nil {
			return mapErr(protocol.TypeCtrlSubscribe, id, err, protocol.ErrInternal)
		}
		return okResponse(protocol.TypeCtrlSubscribe, id, nil)
	}

	// Per-child subscription (existing behaviour).
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlSubscribe, id, protocol.ErrInvalidArgs, "childId required")
	}
	if err := d.c.Subscribe(req.ChildID, conn, filter); err != nil {
		return mapErr(protocol.TypeCtrlSubscribe, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlSubscribe, id, nil)
}

func (d *dispatcher) unsubscribe(conn Connection, frame []byte, id string) []byte {
	var req protocol.UnsubscribeRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlUnsubscribe, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlUnsubscribe, id, protocol.ErrInvalidArgs, "childId required")
	}
	if err := d.c.Unsubscribe(req.ChildID, conn); err != nil {
		return mapErr(protocol.TypeCtrlUnsubscribe, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlUnsubscribe, id, nil)
}

func (d *dispatcher) globalSubscribe(conn Connection, frame []byte, id string) []byte {
	var req protocol.GlobalSubscribeRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlGlobalSubscribe, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if err := d.c.GlobalSubscribe(conn); err != nil {
		return mapErr(protocol.TypeCtrlGlobalSubscribe, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlGlobalSubscribe, id, nil)
}

func (d *dispatcher) globalUnsubscribe(conn Connection, frame []byte, id string) []byte {
	var req protocol.GlobalUnsubscribeRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlGlobalUnsubscribe, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if err := d.c.GlobalUnsubscribe(conn); err != nil {
		return mapErr(protocol.TypeCtrlGlobalUnsubscribe, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlGlobalUnsubscribe, id, nil)
}

// ─── Executor handlers ─────────────────────────────────────────────────────────

// maxExecutorListLimit bounds ctrl_executor_list's effective limit so a large
// or absent client-supplied limit can't produce a response that risks
// protocol.MaxFrameBytes.
const maxExecutorListLimit = 100

func (d *dispatcher) executorEnroll(frame []byte, id string) []byte {
	var req protocol.ExecutorEnrollRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlExecutorEnroll, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.TTLSeconds <= 0 {
		return errResponse(protocol.TypeCtrlExecutorEnroll, id, protocol.ErrInvalidArgs, "ttlSeconds must be positive")
	}
	result, err := d.c.ExecutorEnroll(req)
	if err != nil {
		return mapErr(protocol.TypeCtrlExecutorEnroll, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlExecutorEnroll, id, result)
}

func (d *dispatcher) executorList(frame []byte, id string) []byte {
	var req protocol.ExecutorListRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlExecutorList, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.Limit > maxExecutorListLimit {
		req.Limit = maxExecutorListLimit
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	execs, err := d.c.ExecutorList(req)
	if err != nil {
		return mapErr(protocol.TypeCtrlExecutorList, id, err, protocol.ErrInternal)
	}
	if execs == nil {
		execs = []executors.Executor{}
	}
	// Clamp against MaxFrameBytes.
	type listPayload struct {
		Executors []executors.Executor `json:"executors"`
	}
	payload := listPayload{Executors: execs}
	if b, err := json.Marshal(payload); err == nil && len(b) > protocol.MaxFrameBytes-4096 {
		for len(execs) > 0 {
			execs = execs[:len(execs)-1]
			payload.Executors = execs
			if b, err2 := json.Marshal(payload); err2 == nil && len(b) <= protocol.MaxFrameBytes-4096 {
				break
			}
		}
	}
	return okResponse(protocol.TypeCtrlExecutorList, id, payload)
}

func (d *dispatcher) executorLabel(frame []byte, id string) []byte {
	var req protocol.ExecutorLabelRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlExecutorLabel, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ExecutorID == "" {
		return errResponse(protocol.TypeCtrlExecutorLabel, id, protocol.ErrInvalidArgs, "executorId required")
	}
	if len(req.Set) == 0 && len(req.Remove) == 0 {
		return errResponse(protocol.TypeCtrlExecutorLabel, id, protocol.ErrInvalidArgs, "at least one of set or remove is required")
	}
	result, err := d.c.ExecutorLabel(req)
	if err != nil {
		return mapErr(protocol.TypeCtrlExecutorLabel, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlExecutorLabel, id, result)
}

func (d *dispatcher) executorDisable(frame []byte, id string) []byte {
	var req protocol.ExecutorDisableRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlExecutorDisable, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ExecutorID == "" {
		return errResponse(protocol.TypeCtrlExecutorDisable, id, protocol.ErrInvalidArgs, "executorId required")
	}
	if err := d.c.ExecutorDisable(req); err != nil {
		return mapErr(protocol.TypeCtrlExecutorDisable, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlExecutorDisable, id, nil)
}

func (d *dispatcher) executorEnable(frame []byte, id string) []byte {
	var req protocol.ExecutorEnableRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlExecutorEnable, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ExecutorID == "" {
		return errResponse(protocol.TypeCtrlExecutorEnable, id, protocol.ErrInvalidArgs, "executorId required")
	}
	if err := d.c.ExecutorEnable(req); err != nil {
		return mapErr(protocol.TypeCtrlExecutorEnable, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlExecutorEnable, id, nil)
}
