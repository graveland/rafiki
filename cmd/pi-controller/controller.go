package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"graveland.dev/pi-controller/internal/child"
	"graveland.dev/pi-controller/internal/intercept"
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
	st          *store.Store
	cm          *ChildManager
	records     *persist.RecordWriter
	dumper      *persist.LogDumper
	startedAt   time.Time
	socketPath  string
	logsDir     string
	stateDir    string
	graceWindow time.Duration
}

// NewController constructs a Controller. Call loadOrphans() after construction
// to pre-populate the store from persisted state.
//
// dumper may be nil; when nil, no log dumps are written on child exit.
// The grace window defaults to 7 days but can be overridden with the
// PI_CONTROLLER_GRACE_HOURS environment variable.
func NewController(st *store.Store, stateDir, logsDir, socketPath string, dumper *persist.LogDumper) *Controller {
	gw := 7 * 24 * time.Hour
	if h := os.Getenv("PI_CONTROLLER_GRACE_HOURS"); h != "" {
		if n, err := strconv.ParseFloat(h, 64); err == nil && n > 0 {
			gw = time.Duration(n * float64(time.Hour))
		}
	}
	return &Controller{
		st:          st,
		cm:          newChildManager(),
		records:     persist.NewRecordWriter(stateDir),
		dumper:      dumper,
		startedAt:   time.Now(),
		socketPath:  socketPath,
		logsDir:     logsDir,
		stateDir:    stateDir,
		graceWindow: gw,
	}
}

// startSweeper launches a background goroutine that periodically forgets
// exited children whose age exceeds the configured grace window. It stops
// when ctx is cancelled.
func (c *Controller) startSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.sweepExpired()
			}
		}
	}()
}

// sweepExpired forgets all exited children whose ExitedAt is older than
// graceWindow. Called periodically by the sweeper goroutine.
func (c *Controller) sweepExpired() {
	cutoff := time.Now().Add(-c.graceWindow)
	var toForget []string
	for _, s := range c.st.FindByStatus(protocol.StatusExited) {
		if !s.ExitedAt.IsZero() && s.ExitedAt.Before(cutoff) {
			toForget = append(toForget, s.ChildID)
		}
	}
	for _, id := range toForget {
		_ = c.Forget(id)
	}
	if len(toForget) > 0 {
		slog.Info("sweep: forgot expired children", "count", len(toForget))
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
	snap, ok := c.st.Get(childID)
	if !ok {
		return server.RecentResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	ch, alive := c.cm.Get(childID)
	var events []ring.Event
	var total int
	var oldestTS int64

	if alive {
		// Live child: query the in-memory ring buffer.
		r := ch.Ring()
		events = r.Recent(ring.Query{Limit: q.Limit, Since: q.Since})
		total, _, oldestTS = r.Stats()
	} else {
		// Exited child: fall back to the ring snapshot taken at exit time
		// (spec §11.4). Apply Since + Limit filtering manually.
		all := snap.ExitedRing
		if q.Since > 0 {
			i := 0
			for i < len(all) && all[i].Timestamp < q.Since {
				i++
			}
			all = all[i:]
		}
		if q.Limit > 0 && len(all) > q.Limit {
			all = all[len(all)-q.Limit:]
		}
		events = all
		total = len(snap.ExitedRing)
		if len(snap.ExitedRing) > 0 {
			oldestTS = snap.ExitedRing[0].Timestamp
		}
	}

	out := make([]json.RawMessage, 0, len(events))
	for _, ev := range events {
		if framePassesTypeFilter(ev.Bytes, q.Include, q.Exclude) {
			out = append(out, json.RawMessage(ev.Bytes))
		}
	}

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
		ChildID:     childID,
		Cwd:         req.Cwd,
		PiBinary:    piBin,
		Argv:        argv,
		Env:         env,
		EnvOverride: req.EnvOverride,
	}

	now := time.Now()

	ch, err := child.Spawn(ctx, spec)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: err.Error(),
		}
	}

	// Register the child in the ChildManager immediately after spawn so that any
	// global subscriber that calls ctrl_subscribe in response to
	// ctrl_child_spawned (below) will find the child ready in the manager.
	// monitorChild is still started after Idle(), but the Subscribe endpoint
	// must be non-racy from the moment the spawn event is visible.
	c.cm.Add(childID, ch)

	// FIX 5: Insert a minimal record at StatusSpawning immediately after the
	// process is confirmed running. A crash between exec and Idle() would
	// otherwise leave an orphan pi process with no persisted record.
	sess := &store.Session{
		ChildID:      childID,
		PID:          ch.PID(),
		Status:       protocol.StatusSpawning,
		Name:         req.Name,
		Cwd:          req.Cwd,
		Provider:     req.Provider,
		Model:        req.Model,
		Thinking:     req.Thinking,
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

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record (spawning)", "childId", childID, "error", err)
	}

	// Emit ctrl_child_spawned immediately after the process is running and the
	// state record is persisted (spec §6.3.3, §7.2). Delivered to both global
	// subscribers and per-child subscribers of this child.
	spawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    req.Name,
		Cwd:     req.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(req.Provider, req.Model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(spawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
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

	// FIX 4: If a name was requested and pi has a different one, send
	// set_session_name and poll until the metadata sniffer confirms the rename
	// (up to 5s). This ensures Spawn returns only after the rename is applied.
	if !stalled && req.Name != "" && meta.SessionName != req.Name {
		renameID := "controller-rename-1"
		frame := []byte(fmt.Sprintf(`{"type":"set_session_name","id":%q,"name":%q}`, renameID, req.Name))
		if err := ch.Send(frame); err == nil {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if ch.Metadata().SessionName == req.Name {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if ch.Metadata().SessionName != req.Name {
				slog.Warn("set_session_name timed out", "childId", childID, "want", req.Name)
			}
		}
		meta = ch.Metadata()
	}

	// Determine final provider/model, preferring metadata over request values.
	provider, model := splitModel(meta.Model)
	if provider == "" {
		provider = req.Provider
	}
	if model == "" {
		model = req.Model
	}

	// FIX 5: Update the store record with session metadata learned after Idle.
	// Status is updated via handleStatusChange below (which also fixes the index).
	_ = c.st.Update(childID, func(s *store.Session) {
		s.SessionID = meta.SessionID
		s.SessionFile = meta.SessionFile
		s.Provider = provider
		s.Model = model
	})

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record (after idle)", "childId", childID, "error", err)
	}

	// FIX 3: Emit the spawning→idle ctrl_child_status transition. The SM
	// transitioned inside child.handleFrame; publishing the event here lets
	// global subscribers observe it. handleStatusChange also updates the
	// byStatus index, which Insert left at StatusSpawning.
	if !stalled {
		c.handleStatusChange(childID, protocol.StatusIdle, protocol.StatusSpawning)
	}

	// Start the monitoring goroutine that forwards events and tracks status/exit.
	// monitorChild initialises lastStatus from ch.Status() which is already
	// StatusIdle after the explicit transition above, so the transition is not
	// double-emitted.
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

	// Spawn succeeded — only now remove the old exited entry. Deleting before
	// spawn would lose the session if spawn fails (e.g. bad pi binary path);
	// the entry would only be recoverable on controller restart via state scan.
	c.st.Delete(childID)

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

	// Emit ctrl_child_spawned for the resumed child (spec §7.2 resume case).
	// Delivered to both global subscribers and per-child subscribers of this child.
	resumeSpawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    snap.Name,
		Cwd:     snap.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(provider, model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(resumeSpawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
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

// RespawnChild kills the existing child (the caller must have already done so
// and waited for StatusExited) and starts a replacement with a session
// override. sessionPath controls session continuity:
//
//   - "" (empty): fresh pi session — no --session flag, pi creates a new one.
//   - non-empty: pi resumes that specific session file via --session <path>.
//
// All other spawn configuration is inherited from the child's persisted
// snapshot. The childID is preserved across the respawn. This is the
// implementation path for new_session and switch_session interception (spec §5.1).
func (c *Controller) RespawnChild(ctx context.Context, childID, sessionPath string) (server.SpawnResult, error) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	if snap.Status != protocol.StatusExited {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrNotResumable,
			Message: "child is not exited (status: " + string(snap.Status) + ")",
		}
	}

	req := protocol.SpawnRequest{
		Name:     snap.Name,
		Cwd:      snap.Cwd,
		Provider: snap.Provider,
		Model:    snap.Model,
		Thinking: snap.Thinking,
		// Session: fresh start (no --no-session, no --fork).
		// sessionPath non-empty adds --session <sessionPath>.
		NoSession:          false,
		SessionDir:         snap.SessionDir,
		ResumeSession:      sessionPath,
		ForkSession:        "",
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

	// Spawn succeeded — remove the old exited entry only after the new process
	// is confirmed running (so a failed spawn doesn't lose the record).
	c.st.Delete(childID)

	stalled := false
	select {
	case <-ch.Idle():
	case <-time.After(5 * time.Second):
		stalled = true
		slog.Warn("respawned child stalled", "childId", childID)
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
		NoSession:          false,
		SessionDir:         snap.SessionDir,
		ResumeSession:      sessionPath,
		ForkSession:        "",
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
		slog.Warn("write state record (respawn)", "childId", childID, "error", err)
	}

	// Emit ctrl_child_spawned for the respawned child (spec §7.2).
	// Delivered to both global subscribers and per-child subscribers of this child.
	respawnedSpawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    snap.Name,
		Cwd:     snap.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(provider, model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(respawnedSpawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
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

	// Drive the SM to shutting_down so ctrl_child_status subscribers see the
	// transition before the graceful-shutdown sequence begins (spec §6.5).
	if changed, prev := ch.BeginShutdown(); changed {
		c.handleStatusChange(childID, protocol.StatusShuttingDown, prev)
	}

	shutdownTimeout := durOrDefault(shutdownTimeoutMs, 180*time.Second)
	killTimeout := durOrDefault(killTimeoutMs, 30*time.Second)

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
	// new_session and switch_session are handled via kill+respawn rather than
	// forwarded to pi (spec §5.1).
	if decision, ok := intercept.Inspect(frame); ok {
		return c.handleInterceptedSend(childID, decision)
	}

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

	// Detect extension_ui_response frames and update the SM so the blocked_ui
	// state is cleared when the last pending dialog is resolved (spec §10).
	var uiResp struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(frame, &uiResp) == nil &&
		uiResp.Type == "extension_ui_response" &&
		uiResp.ID != "" {
		ch.NotifyExtensionUIResponse(uiResp.ID)
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

// handleInterceptedSend handles new_session and switch_session by killing the
// current child process and re-spawning it with the same childId (spec §5.1).
// Per-child subscriptions are preserved across the kill+resume cycle so that
// clients observe a seamless transition. A synthesized pi-level response is
// delivered to subscribers after the new process is ready.
func (c *Controller) handleInterceptedSend(childID string, decision intercept.Decision) error {
	if _, ok := c.st.Get(childID); !ok {
		return &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	// Save per-child subscribers before Kill. monitorChild.Remove (called by
	// handleChildExit) will clear the list when the old process exits.
	savedSubs := c.cm.GetSubscribers(childID)

	// Gracefully shut down the current child.
	if _, err := c.Kill(context.Background(), childID, 3000, 500); err != nil {
		var ce *server.ControllerError
		if !errors.As(err, &ce) ||
			(ce.Code != protocol.ErrChildExited && ce.Code != protocol.ErrChildShuttingDown) {
			return fmt.Errorf("intercept kill: %w", err)
		}
	}

	// Spin-wait for handleChildExit (running on the monitorChild goroutine) to
	// call cm.Remove. Kill returns once c.done is closed (process reaped), but
	// monitorChild runs concurrently and calls handleChildExit shortly after.
	// We must wait for cm.Remove to complete; otherwise RespawnChild.cm.Add
	// races with handleChildExit.cm.Remove and the new entry can be deleted
	// immediately, causing the restored subscribers to be silently dropped.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := c.cm.Get(childID); !alive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Respawn the child with the same childId, applying the session override
	// dictated by the intercepted command (spec §5.1).
	sessionPath := "" // new_session: let pi create a fresh session
	if decision.Type == intercept.InterceptSwitchSession {
		sessionPath = decision.SessionPath
	}
	if _, err := c.RespawnChild(context.Background(), childID, sessionPath); err != nil {
		return fmt.Errorf("intercept respawn: %w", err)
	}

	// Restore preserved subscriptions on the new child instance and deliver
	// the synthetic pi-level response so subscribers observe the transition.
	// Wrap in ctrl_event so subscribers see the correct envelope shape (§7.1).
	c.cm.RestoreSubscribers(childID, savedSubs)
	synthFrame := intercept.SynthesizeResponse(string(decision.Type), decision.PiRequestID)
	c.cm.DeliverToChild(childID, wrapCtrlEvent(childID, synthFrame))

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

// OnConnectionClose is called by the server when a client connection closes.
// It removes any global subscriptions held by this connection.
//
// TODO: per-child subscribers for this connection are not cleaned up here;
// they accumulate until the child is removed from the ChildManager on exit.
// This is a known limitation — per-child sub sets are bounded by the child
// lifetime and the subscriber count is small in practice.
func (c *Controller) OnConnectionClose(conn server.Connection) {
	c.cm.GlobalUnsubscribe(conn)
}

// ─── monitorChild ─────────────────────────────────────────────────────────────

// monitorChild runs as a goroutine for each live child. It forwards bus events
// to per-child subscribers (wrapped in ctrl_event envelopes per §7.1), tracks
// status transitions, handles rename detection, and handles child exit.
func (c *Controller) monitorChild(childID string, ch *child.Child) {
	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	lastStatus := ch.Status()

	// Initialise last-known name from the store so we can detect renames.
	lastKnownName := ""
	if snap, ok := c.st.Get(childID); ok {
		lastKnownName = snap.Name
	}

	for {
		select {
		case frame, ok := <-busCh:
			if !ok {
				// Bus was closed (shouldn't happen in normal operation).
				c.handleChildExit(childID, ch)
				return
			}
			// Wrap in ctrl_event envelope so subscribers can correlate events to
			// their source child (spec §7.1).
			c.cm.DeliverToChild(childID, wrapCtrlEvent(childID, frame))

			// Check for status change and keep store + global subs in sync.
			if newStatus := ch.Status(); newStatus != lastStatus {
				c.handleStatusChange(childID, newStatus, lastStatus)
				lastStatus = newStatus
			}

			// Detect session name changes produced by the sniffer. The sniffer
			// updates Metadata().SessionName when set_session_name completes.
			// Polling on each bus event is the right moment because the sniffer
			// update happens in the same readStdout goroutine that feeds the bus
			// (spec §7.5).
			if md := ch.Metadata(); md.SessionName != "" && md.SessionName != lastKnownName {
				c.handleChildRenamed(childID, md.SessionName, lastKnownName)
				lastKnownName = md.SessionName
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
						c.cm.DeliverToChild(childID, wrapCtrlEvent(childID, frame))
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

// wrapCtrlEvent wraps a raw pi event in a ctrl_event envelope (spec §7.1).
// This lets per-child subscribers correlate events to their source child and
// filter by inner event type. If raw cannot be marshalled (non-JSON input),
// raw is returned unchanged to keep the delivery stream flowing.
func wrapCtrlEvent(childID string, raw []byte) []byte {
	env := protocol.CtrlEvent{
		Type:    protocol.TypeCtrlEvent,
		ChildID: childID,
		Event:   json.RawMessage(raw),
	}
	b, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	return b
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
		// Deliver to global subscribers AND to per-child subscribers (spec §7.4).
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
	}
}

// handleChildRenamed updates the store and emits ctrl_child_renamed when the
// sniffer detects that pi changed the session name (spec §7.5).
func (c *Controller) handleChildRenamed(childID, newName, previous string) {
	_ = c.st.Rename(childID, newName)
	evt := protocol.CtrlChildRenamed{
		Type:     protocol.TypeCtrlChildRenamed,
		ChildID:  childID,
		Name:     newName,
		Previous: previous,
		At:       time.Now().UnixMilli(),
	}
	if b, err := json.Marshal(evt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
	}
}

func (c *Controller) handleChildExit(childID string, ch *child.Child) {
	res := ch.ExitResult()
	now := time.Now()

	// Determine last known status before marking exited.
	snap, _ := c.st.Get(childID)
	lastStatus := string(snap.Status)

	// Snapshot the ring before removing the child so ctrl_get_recent continues
	// to work after the child is gone (spec §11.4).
	ringSnapshot := ch.Ring().Recent(ring.Query{})

	c.st.SetStatus(childID, protocol.StatusExited)
	_ = c.st.Update(childID, func(sess *store.Session) {
		sess.ExitedAt = now
		code := res.ExitCode
		sess.ExitCode = &code
		sess.ExitSignal = res.Signal
		sess.ExitedRing = ringSnapshot
	})

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record on exit", "childId", childID, "error", err)
	}

	// Dump logs before removing from the manager so per-child subscribers are
	// still reachable for the exit event delivery below.
	if c.dumper != nil {
		dumpSnap, _ := c.st.Get(childID)
		meta := persist.Meta{
			ChildID:     childID,
			Name:        dumpSnap.Name,
			Cwd:         dumpSnap.Cwd,
			Model:       joinModel(dumpSnap.Provider, dumpSnap.Model),
			SessionFile: dumpSnap.SessionFile,
			SpawnedAt:   dumpSnap.StartedAt.Unix(),
			ExitedAt:    now.Unix(),
			ExitCode:    res.ExitCode,
			ExitSignal:  res.Signal,
			Argv:        dumpSnap.ExtraArgs,
		}
		exitInfo := persist.ExitInfo{
			ExitCode:   res.ExitCode,
			Signal:     res.Signal,
			LastStatus: lastStatus,
		}
		if err := c.dumper.Dump(childID, ch.InSnapshot(), ch.RingSnapshot(), ch.StderrSnapshot(), meta, exitInfo); err != nil {
			slog.Warn("log dump failed", "child", childID, "error", err)
		}
	}

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
		// Deliver to per-child subscribers BEFORE Remove so the subscriber list
		// is still reachable. Deliver to global subscribers too (spec §7.3).
		c.cm.DeliverToChild(childID, b)
		c.cm.DeliverToGlobal(b)
	}

	c.cm.Remove(childID)
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

// buildEnv assembles the per-process env var additions for a child process.
// The slice is passed to SpawnSpec.Env. Whether these additions replace or
// extend the parent environment is controlled by SpawnSpec.EnvOverride
// (honoured in child.Spawn, not here).
//
// The two reserved controller vars are always injected regardless of mode.
func buildEnv(req protocol.SpawnRequest, childID, socketPath string) []string {
	var env []string
	for k, v := range req.Env {
		env = append(env, k+"="+v)
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
