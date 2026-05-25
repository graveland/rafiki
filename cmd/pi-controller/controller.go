package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"graveland.dev/pi-controller/internal/child"
	"graveland.dev/pi-controller/internal/persist"
	"graveland.dev/pi-controller/internal/protocol"
	"graveland.dev/pi-controller/internal/ring"
	"graveland.dev/pi-controller/internal/server"
	"graveland.dev/pi-controller/internal/store"
)

const version = "0.1.0"

// Controller wires together the store, child lifecycle, persistence and the
// server.Controller interface. It is safe for concurrent use.
type Controller struct {
	st         *store.Store
	cm         *ChildManager
	records    *persist.RecordWriter
	startedAt  time.Time
	socketPath string
	logsDir    string
	stateDir   string
}

// NewController constructs a Controller. Call loadOrphans() after construction
// to pre-populate the store from persisted state.
func NewController(st *store.Store, stateDir, logsDir, socketPath string) *Controller {
	return &Controller{
		st:         st,
		cm:         newChildManager(),
		records:    persist.NewRecordWriter(stateDir),
		startedAt:  time.Now(),
		socketPath: socketPath,
		logsDir:    logsDir,
		stateDir:   stateDir,
	}
}

// loadOrphans inserts persisted records into the store as exited sessions and
// SIGTERMs any process whose PID is still alive.
func (c *Controller) loadOrphans(records []persist.Record) {
	for _, rec := range records {
		if rec.PID > 0 {
			if err := syscall.Kill(rec.PID, 0); err == nil {
				// Process is still alive — SIGTERM it.
				_ = syscall.Kill(rec.PID, syscall.SIGTERM)
				slog.Info("sigterm orphan", "childId", rec.ChildID, "pid", rec.PID)
			}
		}

		sess := sessionFromRecord(rec)
		sess.Status = protocol.StatusExited
		c.st.Insert(sess)
	}
}

// ─── server.Controller implementation ────────────────────────────────────────

func (c *Controller) List(filter protocol.ListFilter) []store.Snapshot {
	snaps := c.st.List()
	if filter.Status == "" && filter.Name == "" && filter.NameContains == "" &&
		filter.CwdContains == "" && filter.Since == 0 {
		return snaps
	}
	out := snaps[:0]
	for _, s := range snaps {
		if filter.Status != "" && string(s.Status) != filter.Status {
			continue
		}
		if filter.Name != "" && s.Name != filter.Name {
			continue
		}
		if filter.NameContains != "" && !strings.Contains(s.Name, filter.NameContains) {
			continue
		}
		if filter.CwdContains != "" && !strings.Contains(s.Cwd, filter.CwdContains) {
			continue
		}
		if filter.Since > 0 && s.StartedAt.UnixMilli() < filter.Since {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (c *Controller) Get(childID string) (store.Snapshot, bool) {
	return c.st.Get(childID)
}

func (c *Controller) GetRecent(childID string, q server.RecentQuery) (server.RecentResult, error) {
	if _, ok := c.st.Get(childID); !ok {
		return server.RecentResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	ch, alive := c.cm.Get(childID)
	if !alive {
		// Exited child has no ring buffer in memory.
		return server.RecentResult{Events: []json.RawMessage{}}, nil
	}

	r := ch.Ring()
	events := r.Recent(ring.Query{Limit: q.Limit, Since: q.Since})

	out := make([]json.RawMessage, 0, len(events))
	for _, ev := range events {
		if framePassesTypeFilter(ev.Bytes, q.Include, q.Exclude) {
			out = append(out, json.RawMessage(ev.Bytes))
		}
	}

	total, _, oldestTS := r.Stats()
	return server.RecentResult{
		Events:           out,
		TotalInBuffer:    total,
		OldestTimestamp:  oldestTS,
		TruncatedByLimit: q.Limit > 0 && len(out) == q.Limit,
	}, nil
}

func (c *Controller) Search(q server.SearchQuery) server.SearchResult {
	start := time.Now()
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	snaps := c.st.List()
	var hits []protocol.SearchHit
	scanned := 0

	for _, snap := range snaps {
		if !matchesSessionFilter(snap, q.SessionFilter) {
			continue
		}
		ch, alive := c.cm.Get(snap.ChildID)
		if !alive {
			continue // exited children have no ring buffer in v1
		}

		events := ch.Ring().Recent(ring.Query{})
		for _, ev := range events {
			scanned++
			idx := strings.Index(string(ev.Bytes), q.Query)
			if idx < 0 {
				continue
			}
			snippet := string(ev.Bytes)
			const maxSnippet = 256
			if len(snippet) > maxSnippet {
				snippet = snippet[:maxSnippet]
			}
			hits = append(hits, protocol.SearchHit{
				ChildID:     snap.ChildID,
				SessionFile: snap.SessionFile,
				SessionID:   snap.SessionID,
				SessionName: snap.Name,
				Timestamp:   ev.Timestamp,
				Snippet:     snippet,
				MatchStart:  idx,
				MatchEnd:    idx + len(q.Query),
			})
			if len(hits) >= limit {
				return server.SearchResult{
					Hits:      hits,
					TotalHits: len(hits),
					Scanned:   scanned,
					Elapsed:   time.Since(start).Milliseconds(),
				}
			}
		}
	}
	return server.SearchResult{
		Hits:      hits,
		TotalHits: len(hits),
		Scanned:   scanned,
		Elapsed:   time.Since(start).Milliseconds(),
	}
}

func (c *Controller) Status() server.ControllerStatus {
	snaps := c.st.List()
	var live, exited int
	for _, s := range snaps {
		if s.Status == protocol.StatusExited {
			exited++
		} else {
			live++
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return server.ControllerStatus{
		Version:     version,
		StartedAt:   c.startedAt.UnixMilli(),
		Children:    protocol.ChildCounts{Live: live, Exited: exited},
		MemoryBytes: int64(ms.Sys),
		Socket:      c.socketPath,
		LogsDir:     c.logsDir,
	}
}

func (c *Controller) Spawn(ctx context.Context, req protocol.SpawnRequest) (server.SpawnResult, error) {
	// Validate cwd (dispatch already checks it's absolute; check it exists).
	if _, err := os.Stat(req.Cwd); err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "cwd: " + err.Error(),
		}
	}

	piBin, err := resolvePiBinary(req.PiBinary)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "pi binary not found: " + err.Error(),
		}
	}

	childID := newChildID()
	argv := buildArgv(req)
	env := buildEnv(req, childID, c.socketPath)

	spec := child.SpawnSpec{
		ChildID:  childID,
		Cwd:      req.Cwd,
		PiBinary: piBin,
		Argv:     argv,
		Env:      env,
	}

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// Wait for pi to respond to the bootstrap get_state, with a 5-second timeout.
	stalled := false
	select {
	case <-ch.Idle():
	case <-time.After(5 * time.Second):
		stalled = true
		slog.Warn("child stalled (no get_state response)", "childId", childID)
	}

	meta := ch.Metadata()

	// If a name was requested and pi has a different one, send set_session_name.
	if !stalled && req.Name != "" && meta.SessionName != req.Name {
		b, _ := json.Marshal(map[string]string{
			"type": "set_session_name",
			"name": req.Name,
		})
		_ = ch.Send(b)
	}

	now := time.Now()
	provider, model := splitModel(meta.Model)
	if provider == "" {
		provider = req.Provider
	}
	if model == "" {
		model = req.Model
	}

	initialStatus := ch.Status()

	sess := &store.Session{
		ChildID:      childID,
		PID:          ch.PID(),
		Name:         req.Name,
		Cwd:          req.Cwd,
		Provider:     provider,
		Model:        model,
		Thinking:     req.Thinking,
		SessionID:    meta.SessionID,
		SessionFile:  meta.SessionFile,
		Status:       initialStatus,
		StartedAt:    now,
		LastActivity: now,

		NoSession:          req.NoSession,
		SessionDir:         req.SessionDir,
		ResumeSession:      req.ResumeSession,
		ForkSession:        req.ForkSession,
		Tools:              splitComma(req.Tools),
		NoTools:            req.NoTools,
		NoBuiltinTools:     req.NoBuiltinTools,
		Extensions:         req.Extensions,
		NoExtensions:       req.NoExtensions,
		Skills:             req.Skills,
		NoSkills:           req.NoSkills,
		PromptTemplates:    req.PromptTemplates,
		NoPromptTemplates:  req.NoPromptTemplates,
		Themes:             req.Themes,
		NoThemes:           req.NoThemes,
		NoContextFiles:     req.NoContextFiles,
		SystemPrompt:       req.SystemPrompt,
		AppendSystemPrompt: req.AppendSystemPrompt,
		Verbose:            req.Verbose,
		PiBinary:           piBin,
		ExtraArgs:          req.ExtraArgs,
	}
	c.st.Insert(sess)
	c.cm.Add(childID, ch)

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record", "childId", childID, "error", err)
	}

	// Emit ctrl_child_spawned to global subscribers.
	spawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    req.Name,
		Cwd:     req.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(provider, model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(spawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
	}

	// Start the monitoring goroutine that forwards events and tracks status/exit.
	go c.monitorChild(childID, ch)

	return server.SpawnResult{
		ChildID:     childID,
		SessionID:   meta.SessionID,
		SessionFile: meta.SessionFile,
		Model:       joinModel(provider, model),
		Stalled:     stalled,
	}, nil
}

func (c *Controller) Resume(ctx context.Context, childID string, apiKey string) (server.SpawnResult, error) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: "child not found: " + childID,
		}
	}
	if snap.Status != protocol.StatusExited {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "child is not exited (status: " + string(snap.Status) + ")",
		}
	}

	// Verify session file exists if this session has one.
	if !snap.NoSession && snap.SessionFile != "" {
		if _, err := os.Stat(snap.SessionFile); err != nil {
			return server.SpawnResult{}, &server.ControllerError{
				Code:    protocol.ErrSessionFileMissing,
				Message: "session file not found: " + snap.SessionFile,
			}
		}
	}

	// Rebuild spawn request from the snapshot.
	req := protocol.SpawnRequest{
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Provider:           snap.Provider,
		Model:              snap.Model,
		Thinking:           snap.Thinking,
		APIKey:             apiKey,
		NoSession:          snap.NoSession,
		SessionDir:         snap.SessionDir,
		ResumeSession:      snap.SessionFile, // re-open the existing session file
		ForkSession:        snap.ForkSession,
		Tools:              strings.Join(snap.Tools, ","),
		NoTools:            snap.NoTools,
		NoBuiltinTools:     snap.NoBuiltinTools,
		Extensions:         snap.Extensions,
		NoExtensions:       snap.NoExtensions,
		Skills:             snap.Skills,
		NoSkills:           snap.NoSkills,
		PromptTemplates:    snap.PromptTemplates,
		NoPromptTemplates:  snap.NoPromptTemplates,
		Themes:             snap.Themes,
		NoThemes:           snap.NoThemes,
		NoContextFiles:     snap.NoContextFiles,
		SystemPrompt:       snap.SystemPrompt,
		AppendSystemPrompt: snap.AppendSystemPrompt,
		Verbose:            snap.Verbose,
		PiBinary:           snap.PiBinary,
		ExtraArgs:          snap.ExtraArgs,
	}

	// Delete the old store entry and spawn under the same childID.
	c.st.Delete(childID)

	piBin, err := resolvePiBinary(req.PiBinary)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "pi binary not found: " + err.Error(),
		}
	}

	argv := buildArgv(req)
	env := buildEnv(req, childID, c.socketPath)

	spec := child.SpawnSpec{
		ChildID:  childID,
		Cwd:      req.Cwd,
		PiBinary: piBin,
		Argv:     argv,
		Env:      env,
	}

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	stalled := false
	select {
	case <-ch.Idle():
	case <-time.After(5 * time.Second):
		stalled = true
		slog.Warn("resumed child stalled", "childId", childID)
	}

	meta := ch.Metadata()
	now := time.Now()
	provider, model := splitModel(meta.Model)
	if provider == "" {
		provider = snap.Provider
	}
	if model == "" {
		model = snap.Model
	}

	sess := &store.Session{
		ChildID:            childID,
		PID:                ch.PID(),
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Provider:           provider,
		Model:              model,
		Thinking:           snap.Thinking,
		SessionID:          meta.SessionID,
		SessionFile:        meta.SessionFile,
		Status:             ch.Status(),
		StartedAt:          now,
		LastActivity:       now,
		NoSession:          snap.NoSession,
		SessionDir:         snap.SessionDir,
		ResumeSession:      snap.SessionFile,
		ForkSession:        snap.ForkSession,
		Tools:              snap.Tools,
		NoTools:            snap.NoTools,
		NoBuiltinTools:     snap.NoBuiltinTools,
		Extensions:         snap.Extensions,
		NoExtensions:       snap.NoExtensions,
		Skills:             snap.Skills,
		NoSkills:           snap.NoSkills,
		PromptTemplates:    snap.PromptTemplates,
		NoPromptTemplates:  snap.NoPromptTemplates,
		Themes:             snap.Themes,
		NoThemes:           snap.NoThemes,
		NoContextFiles:     snap.NoContextFiles,
		SystemPrompt:       snap.SystemPrompt,
		AppendSystemPrompt: snap.AppendSystemPrompt,
		Verbose:            snap.Verbose,
		PiBinary:           piBin,
		ExtraArgs:          snap.ExtraArgs,
	}
	c.st.Insert(sess)
	c.cm.Add(childID, ch)

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record (resume)", "childId", childID, "error", err)
	}

	go c.monitorChild(childID, ch)

	return server.SpawnResult{
		ChildID:     childID,
		SessionID:   meta.SessionID,
		SessionFile: meta.SessionFile,
		Model:       joinModel(provider, model),
		Stalled:     stalled,
	}, nil
}

func (c *Controller) Kill(ctx context.Context, childID string, shutdownTimeoutMs, killTimeoutMs int64) (server.KillResult, error) {
	ch, ok := c.cm.Get(childID)
	if !ok {
		if snap, ok2 := c.st.Get(childID); ok2 && snap.Status == protocol.StatusExited {
			return server.KillResult{}, &server.ControllerError{
				Code:    protocol.ErrChildExited,
				Message: "child has already exited",
			}
		}
		return server.KillResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	c.st.SetStatus(childID, protocol.StatusShuttingDown)

	shutdownTimeout := durOrDefault(shutdownTimeoutMs, 30*time.Second)
	killTimeout := durOrDefault(killTimeoutMs, 5*time.Second)

	res, err := ch.Shutdown(shutdownTimeout, killTimeout)
	if err != nil {
		return server.KillResult{}, fmt.Errorf("shutdown: %w", err)
	}

	var exitCode *int
	if res.Signal == "" {
		code := res.ExitCode
		exitCode = &code
	}
	return server.KillResult{
		ExitCode:   exitCode,
		Signal:     res.Signal,
		DurationMs: res.Duration.Milliseconds(),
		Escalated:  res.Escalated,
	}, nil
}

func (c *Controller) Forget(childID string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return &server.ControllerError{Code: protocol.ErrNotFound, Message: "child not found: " + childID}
	}
	if snap.Status != protocol.StatusExited {
		return &server.ControllerError{Code: protocol.ErrNotExited, Message: "child is still running"}
	}
	c.st.Delete(childID)
	if err := persist.DeleteRecord(c.stateDir, childID); err != nil {
		slog.Warn("delete state record", "childId", childID, "error", err)
	}
	return nil
}

func (c *Controller) ForgetAllExited(olderThanMs int64) (int, error) {
	snaps := c.st.FindByStatus(protocol.StatusExited)
	now := time.Now().UnixMilli()
	count := 0
	for _, s := range snaps {
		if olderThanMs > 0 && !s.ExitedAt.IsZero() {
			age := now - s.ExitedAt.UnixMilli()
			if age < olderThanMs {
				continue
			}
		}
		c.st.Delete(s.ChildID)
		if err := persist.DeleteRecord(c.stateDir, s.ChildID); err != nil {
			slog.Warn("delete state record", "childId", s.ChildID, "error", err)
		}
		count++
	}
	return count, nil
}

func (c *Controller) Send(childID string, frame json.RawMessage) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return &server.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if snap.Status == protocol.StatusShuttingDown {
		return &server.ControllerError{Code: protocol.ErrChildShuttingDown, Message: "child is shutting down"}
	}
	if snap.Status == protocol.StatusExited {
		return &server.ControllerError{Code: protocol.ErrChildExited, Message: "child has exited"}
	}

	ch, ok := c.cm.Get(childID)
	if !ok {
		return &server.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}

	if err := ch.Send(frame); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "backpressure") {
			return &server.ControllerError{Code: protocol.ErrBackpressure, Message: msg}
		}
		if strings.Contains(msg, "shutting down") {
			return &server.ControllerError{Code: protocol.ErrChildShuttingDown, Message: msg}
		}
		return err
	}
	return nil
}

func (c *Controller) Subscribe(childID string, conn server.Connection, filter protocol.SubscribeFilter) error {
	if _, ok := c.st.Get(childID); !ok {
		return &server.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	c.cm.Subscribe(childID, conn, filter)
	return nil
}

func (c *Controller) Unsubscribe(childID string, conn server.Connection) error {
	c.cm.Unsubscribe(childID, conn)
	return nil
}

func (c *Controller) GlobalSubscribe(conn server.Connection) error {
	c.cm.GlobalSubscribe(conn)
	return nil
}

func (c *Controller) GlobalUnsubscribe(conn server.Connection) error {
	c.cm.GlobalUnsubscribe(conn)
	return nil
}

// ─── monitorChild ─────────────────────────────────────────────────────────────

// monitorChild runs as a goroutine for each live child. It forwards bus events
// to per-child subscribers, tracks status transitions, and handles child exit.
func (c *Controller) monitorChild(childID string, ch *child.Child) {
	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	lastStatus := ch.Status()

	for {
		select {
		case frame, ok := <-busCh:
			if !ok {
				// Bus was closed (shouldn't happen in normal operation).
				c.handleChildExit(childID, ch)
				return
			}
			c.cm.DeliverToChild(childID, frame)

			// Check for status change and keep store + global subs in sync.
			if newStatus := ch.Status(); newStatus != lastStatus {
				c.handleStatusChange(childID, newStatus, lastStatus)
				lastStatus = newStatus
			}

		case <-ch.Done():
			// Drain any frames that arrived before the done signal.
			drained := false
			for !drained {
				select {
				case frame, ok := <-busCh:
					if !ok {
						drained = true
					} else {
						c.cm.DeliverToChild(childID, frame)
					}
				default:
					drained = true
				}
			}
			c.handleChildExit(childID, ch)
			return
		}
	}
}

func (c *Controller) handleStatusChange(childID string, newStatus, prev protocol.Status) {
	c.st.SetStatus(childID, newStatus)
	now := time.Now()
	evt := protocol.CtrlChildStatus{
		Type:     protocol.TypeCtrlChildStatus,
		ChildID:  childID,
		Status:   string(newStatus),
		Previous: string(prev),
		At:       now.UnixMilli(),
	}
	if b, err := json.Marshal(evt); err == nil {
		c.cm.DeliverToGlobal(b)
	}
}

func (c *Controller) handleChildExit(childID string, ch *child.Child) {
	res := ch.ExitResult()
	now := time.Now()

	// Determine last known status before marking exited.
	snap, _ := c.st.Get(childID)
	lastStatus := string(snap.Status)

	c.st.SetStatus(childID, protocol.StatusExited)
	_ = c.st.Update(childID, func(sess *store.Session) {
		sess.ExitedAt = now
		code := res.ExitCode
		sess.ExitCode = &code
		sess.ExitSignal = res.Signal
	})

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record on exit", "childId", childID, "error", err)
	}

	c.cm.Remove(childID)

	var exitCode *int
	if res.Signal == "" {
		code := res.ExitCode
		exitCode = &code
	}
	exitEvt := protocol.CtrlChildExited{
		Type:       protocol.TypeCtrlChildExited,
		ChildID:    childID,
		ExitCode:   exitCode,
		Signal:     res.Signal,
		LastStatus: lastStatus,
		Duration:   res.Duration.Seconds(),
		At:         now.UnixMilli(),
	}
	if b, err := json.Marshal(exitEvt); err == nil {
		c.cm.DeliverToGlobal(b)
	}
}

// ─── persistence ─────────────────────────────────────────────────────────────

func (c *Controller) writeRecord(childID string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return nil
	}
	rec := recordFromSnapshot(snap)
	return c.records.Write(rec)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func newChildID() string {
	return "c_" + ulid.Make().String()
}

func resolvePiBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("PI_BINARY"); env != "" {
		return env, nil
	}
	return exec.LookPath("pi")
}

// buildArgv converts a SpawnRequest into the pi CLI argument list (excluding
// the binary itself). Always starts with --mode rpc.
func buildArgv(req protocol.SpawnRequest) []string {
	var argv []string
	argv = append(argv, "--mode", "rpc")

	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if req.Provider != "" {
		argv = append(argv, "--provider", req.Provider)
	}
	if req.Thinking != "" {
		argv = append(argv, "--thinking", req.Thinking)
	}
	if req.APIKey != "" {
		argv = append(argv, "--api-key", req.APIKey)
	}

	// Session flags.
	if req.NoSession {
		argv = append(argv, "--no-session")
	}
	if req.SessionDir != "" {
		argv = append(argv, "--session-dir", req.SessionDir)
	}
	if req.ResumeSession != "" {
		argv = append(argv, "--session", req.ResumeSession)
	}
	if req.ForkSession != "" {
		argv = append(argv, "--fork", req.ForkSession)
	}

	// Tool / extension / skill scoping.
	if req.Tools != "" {
		argv = append(argv, "--tools", req.Tools)
	}
	if req.NoTools {
		argv = append(argv, "--no-tools")
	}
	if req.NoBuiltinTools {
		argv = append(argv, "--no-builtin-tools")
	}
	for _, ext := range req.Extensions {
		argv = append(argv, "--extensions", ext)
	}
	if req.NoExtensions {
		argv = append(argv, "--no-extensions")
	}
	for _, sk := range req.Skills {
		argv = append(argv, "--skills", sk)
	}
	if req.NoSkills {
		argv = append(argv, "--no-skills")
	}
	for _, pt := range req.PromptTemplates {
		argv = append(argv, "--prompt-templates", pt)
	}
	if req.NoPromptTemplates {
		argv = append(argv, "--no-prompt-templates")
	}
	for _, th := range req.Themes {
		argv = append(argv, "--themes", th)
	}
	if req.NoThemes {
		argv = append(argv, "--no-themes")
	}
	if req.NoContextFiles {
		argv = append(argv, "--no-context-files")
	}

	// System prompt.
	if req.SystemPrompt != "" {
		argv = append(argv, "--system-prompt", req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", req.AppendSystemPrompt)
	}

	if req.Verbose {
		argv = append(argv, "--verbose")
	}

	// Extra args are appended last (last-flag-wins override).
	argv = append(argv, req.ExtraArgs...)
	return argv
}

// buildEnv constructs the environment for a child process.
func buildEnv(req protocol.SpawnRequest, childID, socketPath string) []string {
	var env []string
	if req.EnvOverride && len(req.Env) > 0 {
		// Override: use only the specified env vars plus controller metadata.
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
	} else {
		// Default: inherit os env plus the specified additions.
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
	}
	env = append(env,
		"PI_CONTROLLER_CHILD_ID="+childID,
		"PI_CONTROLLER_SOCKET="+socketPath,
	)
	return env
}

// splitModel splits "provider/model" into provider and model. If no slash is
// present the entire string is the model and provider is empty.
func splitModel(s string) (provider, model string) {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// joinModel returns "provider/model" if both are non-empty, otherwise just model.
func joinModel(provider, model string) string {
	if provider != "" && model != "" {
		return provider + "/" + model
	}
	return model
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func durOrDefault(ms int64, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

func framePassesTypeFilter(frame []byte, include, exclude []string) bool {
	if len(include) == 0 && len(exclude) == 0 {
		return true
	}
	var hdr struct {
		Type string `json:"type"`
	}
	if err := parseEventType(frame, &hdr); err != nil {
		return true
	}
	for _, ex := range exclude {
		if ex == hdr.Type {
			return false
		}
	}
	if len(include) > 0 {
		for _, inc := range include {
			if inc == hdr.Type {
				return true
			}
		}
		return false
	}
	return true
}

func matchesSessionFilter(snap store.Snapshot, f protocol.SearchSessionFilter) bool {
	if f.CwdContains != "" && !strings.Contains(snap.Cwd, f.CwdContains) {
		return false
	}
	if f.NameContains != "" && !strings.Contains(snap.Name, f.NameContains) {
		return false
	}
	if f.Since > 0 && snap.StartedAt.UnixMilli() < f.Since {
		return false
	}
	return true
}

// parseEventType partially decodes a JSON frame into hdr. Using a shared helper
// avoids duplicating json.Unmarshal calls in hot paths.
func parseEventType(frame []byte, hdr any) error {
	return json.Unmarshal(frame, hdr)
}

// sessionFromRecord rebuilds a store.Session from a persisted Record.
func sessionFromRecord(rec persist.Record) *store.Session {
	return &store.Session{
		ChildID:            rec.ChildID,
		PID:                rec.PID,
		Name:               rec.Name,
		Cwd:                rec.Cwd,
		Provider:           rec.Provider,
		Model:              rec.Model,
		Thinking:           rec.Thinking,
		SessionID:          rec.SessionID,
		SessionFile:        rec.SessionFile,
		SessionDir:         rec.SessionDir,
		NoSession:          rec.NoSession,
		Tools:              rec.Tools,
		NoTools:            rec.NoTools,
		NoBuiltinTools:     rec.NoBuiltinTools,
		Extensions:         rec.Extensions,
		NoExtensions:       rec.NoExtensions,
		Skills:             rec.Skills,
		NoSkills:           rec.NoSkills,
		PromptTemplates:    rec.PromptTemplates,
		NoPromptTemplates:  rec.NoPromptTemplates,
		Themes:             rec.Themes,
		NoThemes:           rec.NoThemes,
		NoContextFiles:     rec.NoContextFiles,
		SystemPrompt:       rec.SystemPrompt,
		AppendSystemPrompt: rec.AppendSystemPrompt,
		Verbose:            rec.Verbose,
		PiBinary:           rec.PiBinary,
		ExtraArgs:          rec.ExtraArgs,
		StartedAt:          time.UnixMilli(rec.SpawnedAt),
		LastActivity:       time.UnixMilli(rec.LastSeenAlive),
	}
}

// recordFromSnapshot builds a persist.Record from a store Snapshot.
func recordFromSnapshot(snap store.Snapshot) persist.Record {
	return persist.Record{
		ChildID:            snap.ChildID,
		PID:                snap.PID,
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Provider:           snap.Provider,
		Model:              snap.Model,
		Thinking:           snap.Thinking,
		SessionID:          snap.SessionID,
		SessionFile:        snap.SessionFile,
		SessionDir:         snap.SessionDir,
		NoSession:          snap.NoSession,
		Tools:              snap.Tools,
		NoTools:            snap.NoTools,
		NoBuiltinTools:     snap.NoBuiltinTools,
		Extensions:         snap.Extensions,
		NoExtensions:       snap.NoExtensions,
		Skills:             snap.Skills,
		NoSkills:           snap.NoSkills,
		PromptTemplates:    snap.PromptTemplates,
		NoPromptTemplates:  snap.NoPromptTemplates,
		Themes:             snap.Themes,
		NoThemes:           snap.NoThemes,
		NoContextFiles:     snap.NoContextFiles,
		SystemPrompt:       snap.SystemPrompt,
		AppendSystemPrompt: snap.AppendSystemPrompt,
		Verbose:            snap.Verbose,
		PiBinary:           snap.PiBinary,
		ExtraArgs:          snap.ExtraArgs,
		SpawnedAt:          snap.StartedAt.UnixMilli(),
		LastSeenAlive:      snap.LastActivity.UnixMilli(),
		LastStatus:         string(snap.Status),
	}
}
