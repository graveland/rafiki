// Package server dispatch routes ctrl_* frames to typed handlers backed by a
// Controller interface, so the logic is testable without the real daemon.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"graveland.dev/pi-controller/internal/protocol"
	"graveland.dev/pi-controller/internal/store"
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
	Limit   int
	Since   int64
	Include []string
	Exclude []string
}

// RecentResult is returned by Controller.GetRecent.
type RecentResult = protocol.GetRecentResponseData

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
// The real implementation lives in cmd/pi-controller; tests use a fake.
type Controller interface {
	// Read-only queries.
	List(filter protocol.ListFilter) []store.Snapshot
	Get(childID string) (store.Snapshot, bool)
	GetRecent(childID string, q RecentQuery) (RecentResult, error)
	Search(q SearchQuery) SearchResult
	Status() ControllerStatus

	// Lifecycle mutations.
	Spawn(ctx context.Context, req protocol.SpawnRequest) (SpawnResult, error)
	Resume(ctx context.Context, childID string, apiKey string) (SpawnResult, error)
	Kill(ctx context.Context, childID string, shutdownTimeoutMs, killTimeoutMs int64) (KillResult, error)
	Forget(childID string) error
	ForgetAllExited(olderThanMs int64) (int, error)

	// Frame forwarding.
	Send(childID string, frame json.RawMessage) error

	// Per-connection subscriptions. conn is the Connection passed to
	// FrameHandler; it serves as both the event-delivery target and the
	// identity key for subsequent Unsubscribe calls.
	Subscribe(childID string, conn Connection, filter protocol.SubscribeFilter) error
	Unsubscribe(childID string, conn Connection) error
	GlobalSubscribe(conn Connection) error
	GlobalUnsubscribe(conn Connection) error
}

// ─── Dispatch factory ─────────────────────────────────────────────────────────

// NewDispatch returns a FrameHandler that routes ctrl_* frames to typed handlers
// backed by c. The returned function is safe for concurrent use.
func NewDispatch(c Controller) FrameHandler {
	d := &dispatcher{c: c}
	return d.handle
}

type dispatcher struct{ c Controller }

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
	case protocol.TypeCtrlSearch:
		return d.search(frame, hdr.ID)
	case protocol.TypeCtrlStatus:
		return d.ctrlStatus(frame, hdr.ID)
	case protocol.TypeCtrlSend:
		return d.ctrlSend(frame, hdr.ID)
	case protocol.TypeCtrlSubscribe:
		return d.subscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlUnsubscribe:
		return d.unsubscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlGlobalSubscribe:
		return d.globalSubscribe(conn, frame, hdr.ID)
	case protocol.TypeCtrlGlobalUnsubscribe:
		return d.globalUnsubscribe(conn, frame, hdr.ID)
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

// snapshotToSummary converts a store.Snapshot to the wire ChildSummary shape.
// PID is omitted (nil) when the child has exited. Model is formatted as
// "provider/model" when both fields are present.
func snapshotToSummary(snap store.Snapshot) protocol.ChildSummary {
	model := snap.Model
	if snap.Provider != "" && snap.Model != "" {
		model = snap.Provider + "/" + snap.Model
	}
	var pid *int
	if snap.Status != protocol.StatusExited {
		pid = &snap.PID
	}
	return protocol.ChildSummary{
		ChildID:      snap.ChildID,
		PID:          pid,
		Cwd:          snap.Cwd,
		Name:         snap.Name,
		Model:        model,
		SessionID:    snap.SessionID,
		SessionFile:  snap.SessionFile,
		Status:       string(snap.Status),
		StartedAt:    snap.StartedAt.UnixMilli(),
		LastActivity: snap.LastActivity.UnixMilli(),
		ExitCode:     snap.ExitCode,
		ExitSignal:   snap.ExitSignal,
	}
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
		children[i] = snapshotToSummary(s)
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
	return okResponse(protocol.TypeCtrlGet, id, snapshotToSummary(snap))
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
		Limit:   req.Limit,
		Since:   req.Since,
		Include: req.Include,
		Exclude: req.Exclude,
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
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlSubscribe, id, protocol.ErrInvalidArgs, "childId required")
	}
	var filter protocol.SubscribeFilter
	if req.Filter != nil {
		filter = *req.Filter
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
