package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"git.graveland.dev/brent/pi-controller/internal/child"
	"git.graveland.dev/brent/pi-controller/internal/intercept"
	"git.graveland.dev/brent/pi-controller/internal/persist"
	"git.graveland.dev/brent/pi-controller/internal/ring"
	"git.graveland.dev/brent/pi-controller/internal/server"
	"git.graveland.dev/brent/pi-controller/internal/store"
	"git.graveland.dev/brent/pi-controller/internal/version"
	"git.graveland.dev/brent/pi-controller/protocol"
)

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
	sweeperWg   sync.WaitGroup
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
// when ctx is cancelled. Call Stop() to wait for the goroutine to exit.
func (c *Controller) startSweeper(ctx context.Context) {
	c.sweeperWg.Add(1)
	go func() {
		defer c.sweeperWg.Done()
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

// Stop waits for background goroutines (currently the sweeper) to exit.
// The caller is responsible for cancelling the context passed to startSweeper
// before calling Stop.
func (c *Controller) Stop() {
	c.sweeperWg.Wait()
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
		filter.CwdContains == "" && filter.Since == 0 &&
		len(filter.Labels) == 0 && len(filter.HasLabel) == 0 {
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
		if !matchesLabelFilter(s.Labels, filter.Labels, filter.HasLabel) {
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

	// Select the source event slice based on raw-vs-rendered and liveness.
	var events []ring.Event
	var total int
	var oldestTS int64

	if alive {
		if q.Rendered && ch.Normalizes() {
			events = ch.RenderRecent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, oldestTS = ch.RenderStats()
		} else {
			r := ch.Ring()
			events = r.Recent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, _, oldestTS = r.Stats()
		}
	} else {
		// Exited: pick the snapshot, falling back to the on-disk dump for
		// orphans reloaded after a restart (in-memory snapshots are lost then).
		var all []ring.Event
		if q.Rendered {
			all = snap.ExitedRenderRing
			if len(all) == 0 {
				all = c.readDiskEvents(childID, "render.jsonl.gz")
			}
		}
		// Fall back to the raw stream UNLESS this is a rendered request for a
		// normalizing (claude) child: claude's raw stdout is NOT renderable, so
		// an empty render-ring must stay empty rather than dumping raw frames
		// into the rendered view (matches the live path). pi's raw ring already
		// IS pi-vocabulary, so pi rendered requests still fall through here.
		if len(all) == 0 && !(q.Rendered && snap.Kind == "claude") {
			all = snap.ExitedRing
			if len(all) == 0 {
				all = c.readDiskEvents(childID, "out.jsonl.gz")
			}
		}
		total = len(all)
		if len(all) > 0 {
			oldestTS = all[0].Timestamp
		}
		if q.Since > 0 {
			i := 0
			// A zero timestamp means "unknown" (render frames sourced from the
			// on-disk render.jsonl.gz carry no timestamp) — keep those rather
			// than dropping the whole disk-sourced rendered backfill.
			for i < len(all) && all[i].Timestamp != 0 && all[i].Timestamp < q.Since {
				i++
			}
			all = all[i:]
		}
		if q.Limit > 0 && len(all) > q.Limit {
			all = all[len(all)-q.Limit:]
		}
		events = all
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

// readDiskEvents reads a per-child on-disk dump file (out.jsonl.gz /
// render.jsonl.gz) into ring.Events with zero timestamps. Returns nil when the
// dump is absent or unreadable (best-effort backfill after a restart).
func (c *Controller) readDiskEvents(childID, name string) []ring.Event {
	if c.logsDir == "" {
		return nil
	}
	frames, err := persist.ReadGzLines(filepath.Join(c.logsDir, childID, name))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("readDiskEvents: backfill dump unreadable", "child", childID, "file", name, "error", err)
		}
		return nil
	}
	out := make([]ring.Event, len(frames))
	for i, f := range frames {
		out[i] = ring.Event{Bytes: f}
	}
	return out
}

func (c *Controller) GetStreams(childID string, which string) (server.GetStreamsResult, error) {
	if _, ok := c.st.Get(childID); !ok {
		return server.GetStreamsResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	ch, alive := c.cm.Get(childID)
	if !alive {
		return server.GetStreamsResult{Alive: false}, nil
	}
	res := server.GetStreamsResult{Alive: true}
	if which == "" || which == "all" || which == "in" {
		res.In = ch.InSnapshot()
	}
	// Live stderr is intentionally omitted: errBuf is an unguarded bytes.Buffer
	// written by the readStderr goroutine, so StderrSnapshot races until Done()
	// is closed. Stderr for a live child is therefore left nil; callers fall
	// back to the on-disk dump, which becomes available after the child exits.
	return res, nil
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
		Version:     version.String(),
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

	// Validate user-supplied labels: no invalid keys, no pic/ prefix.
	if err := validateUserLabelKeys(req.Labels); err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: err.Error(),
		}
	}

	bin, argv, prov, err := resolveSpawnPlan(req)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	childID := newChildID()
	env := buildEnv(req, childID, c.socketPath)
	if req.Kind == "claude" {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	spec := child.SpawnSpec{
		ChildID:     childID,
		Cwd:         req.Cwd,
		PiBinary:    bin,
		Argv:        argv,
		Env:         env,
		EnvOverride: req.EnvOverride,
		Provider:    prov,
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

	// Build initial labels: user-supplied labels (already validated) plus
	// static auto-labels. Model/provider labels are added after Idle() in
	// activateLiveChild once pi reports its resolved model.
	initLabels := copyLabels(req.Labels)
	if initLabels == nil {
		initLabels = make(map[string]string)
	}
	initLabels["pic/cwd"] = req.Cwd
	initLabels["pic/pid"] = strconv.Itoa(ch.PID())
	initLabels["pic/kind"] = spawnKindLabel(req.Kind)
	if req.ConfigDir != "" {
		initLabels["pic/config_dir"] = req.ConfigDir
	}
	if req.ResumedFromSession != "" {
		initLabels["pic/resumed-from-session"] = req.ResumedFromSession
	}

	// FIX 5: Insert a minimal record at StatusSpawning immediately after the
	// process is confirmed running. A crash between exec and Idle() would
	// otherwise leave an orphan pi process with no persisted record.
	sess := &store.Session{
		ChildID:      childID,
		PID:          ch.PID(),
		Status:       protocol.StatusSpawning,
		Name:         req.Name,
		Cwd:          req.Cwd,
		Kind:         req.Kind,
		ConfigDir:    req.ConfigDir,
		Provider:     req.Provider,
		Model:        req.Model,
		Thinking:     req.Thinking,
		StartedAt:    now,
		LastActivity: now,
		Labels:       initLabels,

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
		PiBinary:           bin,
		ExtraArgs:          req.ExtraArgs,
	}
	c.st.Insert(sess)

	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record (spawning)", "childId", childID, "error", err)
	}

	// Emit ctrl_child_spawned immediately after the process is running and the
	// state record is persisted (spec §6.3.3, §7.2). Delivered to global,
	// per-child, and label-filtered subscribers.
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
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
	}

	// Post-Idle: name reconciliation, metadata update, status transition,
	// monitorChild start. Shared with the Resume/RespawnChild paths via
	// activateLiveChild (baseSnap==nil selects the Spawn-specific branch).
	// noSession/resumeSession/forkSession are unused by that branch (req is used
	// instead); pass zero values for clarity.
	return c.activateLiveChild(childID, ch, bin, req, nil, false, "", "")
}

// activateLiveChild handles the post-spawn registration sequence. It waits for
// the child to become idle, resolves provider/model from metadata, and then
// takes one of two paths based on whether baseSnap is nil.
//
// Fresh Spawn (baseSnap == nil): the caller has already inserted a
// StatusSpawning session, called cm.Add, and emitted ctrl_child_spawned.
// This method performs name reconciliation (if req.Name is set), updates the
// existing session with post-Idle metadata, persists a record, emits the
// spawning→idle status transition, and starts monitorChild.
//
// Resume / RespawnChild (baseSnap != nil): the caller has already deleted the
// old exited session. noSession/resumeSession/forkSession are the session-
// continuity fields that differ between the two callers. This method builds
// the full Session from baseSnap with those overrides, inserts it, calls
// cm.Add, persists a record, emits ctrl_child_spawned, and starts monitorChild.
func (c *Controller) activateLiveChild(
	childID string,
	ch *child.Child,
	piBin string,
	req protocol.SpawnRequest,
	baseSnap *store.Snapshot,
	noSession bool,
	resumeSession string,
	forkSession string,
) (server.SpawnResult, error) {
	stalled := false
	select {
	case <-ch.Idle():
	case <-time.After(5 * time.Second):
		stalled = true
		slog.Warn("child did not become idle after spawn", "childId", childID)
	}

	meta := ch.Metadata()
	now := time.Now()

	if baseSnap == nil {
		// Fresh Spawn path. Perform name reconciliation before reading final
		// metadata so the returned SessionName reflects any rename.
		if !stalled && req.Kind != "claude" && req.Name != "" && meta.SessionName != req.Name {
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

		provider, model := splitModel(meta.Model)
		if provider == "" {
			provider = req.Provider
		}
		if model == "" {
			model = req.Model
		}

		// Update the StatusSpawning session inserted before Idle() with the
		// session metadata that is only available after pi responds.
		_ = c.st.Update(childID, func(s *store.Session) {
			s.SessionID = meta.SessionID
			s.SessionFile = meta.SessionFile
			s.Provider = provider
			s.Model = model
			// Update model auto-labels now that pi has reported the resolved model.
			if s.Labels == nil {
				s.Labels = make(map[string]string)
			}
			if provider != "" {
				s.Labels["pic/provider"] = provider
			} else {
				delete(s.Labels, "pic/provider")
			}
			if model != "" {
				s.Labels["pic/model"] = model
			} else {
				delete(s.Labels, "pic/model")
			}
		})
		if err := c.writeRecord(childID); err != nil {
			slog.Warn("write state record (after idle)", "childId", childID, "error", err)
		}
		// Emit the spawning→idle transition. handleStatusChange also fixes the
		// byStatus index that Insert left at StatusSpawning.
		if !stalled {
			c.handleStatusChange(childID, protocol.StatusIdle, protocol.StatusSpawning)
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

	// Resume / RespawnChild path.
	snap := *baseSnap
	provider, model := splitModel(meta.Model)
	if provider == "" {
		provider = snap.Provider
	}
	if model == "" {
		model = snap.Model
	}

	// Recompute auto-labels from the snapshot's user labels with fresh pid/model.
	resumeLabels := copyLabels(snap.Labels)
	if resumeLabels == nil {
		resumeLabels = make(map[string]string)
	}
	resumeLabels["pic/cwd"] = snap.Cwd
	resumeLabels["pic/pid"] = strconv.Itoa(ch.PID())
	resumeLabels["pic/kind"] = spawnKindLabel(snap.Kind)
	if snap.ConfigDir != "" {
		resumeLabels["pic/config_dir"] = snap.ConfigDir
	}
	if provider != "" {
		resumeLabels["pic/provider"] = provider
	} else {
		delete(resumeLabels, "pic/provider")
	}
	if model != "" {
		resumeLabels["pic/model"] = model
	} else {
		delete(resumeLabels, "pic/model")
	}

	sess := &store.Session{
		ChildID:            childID,
		PID:                ch.PID(),
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Kind:               snap.Kind,
		ConfigDir:          snap.ConfigDir,
		Provider:           provider,
		Model:              model,
		Thinking:           snap.Thinking,
		SessionID:          meta.SessionID,
		SessionFile:        meta.SessionFile,
		Status:             ch.Status(),
		StartedAt:          now,
		LastActivity:       now,
		NoSession:          noSession,
		SessionDir:         snap.SessionDir,
		ResumeSession:      resumeSession,
		ForkSession:        forkSession,
		Labels:             resumeLabels,
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
		slog.Warn("write state record", "childId", childID, "error", err)
	}

	// Emit ctrl_child_spawned for the resumed/respawned child (spec §7.2).
	spawnedEvt := protocol.CtrlChildSpawned{
		Type:    protocol.TypeCtrlChildSpawned,
		ChildID: childID,
		Name:    snap.Name,
		Cwd:     snap.Cwd,
		PID:     ch.PID(),
		Model:   joinModel(provider, model),
		At:      now.UnixMilli(),
	}
	if b, err := json.Marshal(spawnedEvt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		if spawnSnap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, spawnSnap.Labels, b)
		}
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

// resumeRequestFromSnapshot rebuilds a SpawnRequest from an exited child's
// snapshot. The resume token differs by kind: pi re-opens its session file via
// --session <path> (ResumeSession=SessionFile); claude re-attaches its stored
// conversation via --resume <id> (ResumeSession=SessionID).
func resumeRequestFromSnapshot(snap store.Snapshot, apiKey string) protocol.SpawnRequest {
	req := protocol.SpawnRequest{
		Kind:               snap.Kind,
		ConfigDir:          snap.ConfigDir,
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Provider:           snap.Provider,
		Model:              snap.Model,
		Thinking:           snap.Thinking,
		APIKey:             apiKey,
		NoSession:          snap.NoSession,
		SessionDir:         snap.SessionDir,
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
	if snap.Kind == "claude" {
		req.ResumeSession = snap.SessionID
	} else {
		req.ResumeSession = snap.SessionFile
	}
	return req
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

	kind := snap.Kind
	if kind == "" {
		kind = "pi"
	}

	// Verify the session file exists for pi children that have one. Claude does
	// not track a session file (it manages its own ~/.claude store keyed by
	// session id), so there is nothing to stat.
	if kind == "pi" && !snap.NoSession && snap.SessionFile != "" {
		if _, err := os.Stat(snap.SessionFile); err != nil {
			return server.SpawnResult{}, &server.ControllerError{
				Code:    protocol.ErrSessionFileMissing,
				Message: "session file not found: " + snap.SessionFile,
			}
		}
	}

	req := resumeRequestFromSnapshot(snap, apiKey)

	bin, argv, prov, err := resolveSpawnPlan(req)
	if err != nil {
		return server.SpawnResult{}, &server.ControllerError{
			Code:    protocol.ErrSpawnFailed,
			Message: "spawn plan: " + err.Error(),
		}
	}

	env := buildEnv(req, childID, c.socketPath)
	if kind == "claude" {
		env = append(env, claudeEnv(req.ConfigDir)...)
	}

	spec := child.SpawnSpec{
		ChildID:     childID,
		Cwd:         req.Cwd,
		PiBinary:    bin,
		Argv:        argv,
		Env:         env,
		EnvOverride: req.EnvOverride,
		Provider:    prov,
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

	// activateLiveChild (baseSnap != nil path): waits for Idle, builds the full
	// Session from snap with Resume's session-continuity values, inserts it,
	// adds to cm, persists, emits ctrl_child_spawned, starts monitorChild.
	return c.activateLiveChild(childID, ch, bin, protocol.SpawnRequest{}, &snap,
		snap.NoSession, snap.SessionFile, snap.ForkSession)
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

	// activateLiveChild (baseSnap != nil path): waits for Idle, builds the full
	// Session from snap with RespawnChild's session-continuity values (fresh
	// start: noSession=false, resumeSession=sessionPath, forkSession=""),
	// inserts, adds to cm, persists, emits ctrl_child_spawned, starts monitorChild.
	return c.activateLiveChild(childID, ch, piBin, protocol.SpawnRequest{}, &snap,
		false, sessionPath, "")
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

// ShutdownAllChildren gracefully shuts down every live child concurrently.
// For each child it:
//  1. Calls BeginShutdown to drive the SM to shutting_down and emit the
//     status-change event to any connected subscribers.
//  2. Launches a goroutine that calls ch.Shutdown(perChildShutdown, perChildKill).
//
// ctx bounds the total wait. If it expires before all children exit the
// function returns ctx.Err() and logs a warning; the outstanding Shutdown
// goroutines continue and will SIGKILL the remaining children on their own
// schedule — they die via pipe-death otherwise.
//
// Per-child errors are logged and collected; all of them are returned as a
// joined error so the caller can decide whether to treat them as fatal.
func (c *Controller) ShutdownAllChildren(ctx context.Context, perChildShutdown, perChildKill time.Duration) error {
	ids := c.cm.LiveIDs()
	if len(ids) == 0 {
		return nil
	}

	slog.Info("shutting down children", "count", len(ids))

	type result struct {
		id  string
		err error
	}
	done := make(chan result, len(ids))

	for _, id := range ids {
		id := id
		ch, ok := c.cm.Get(id)
		if !ok {
			// Already removed between LiveIDs() and Get(); count it as done.
			done <- result{id: id}
			continue
		}
		// Drive SM to shutting_down and emit ctrl_child_status to subscribers
		// before the Shutdown sequence begins (mirrors what Kill does).
		if changed, prev := ch.BeginShutdown(); changed {
			c.handleStatusChange(id, protocol.StatusShuttingDown, prev)
		}
		go func() {
			_, err := ch.Shutdown(perChildShutdown, perChildKill)
			// ch.Shutdown returns when the child *process* is reaped, but
			// handleChildExit — which persists the exit code/signal to the
			// state record — runs asynchronously in monitorChild's goroutine.
			// Wait for that goroutine to finish (signalled by cm.Remove)
			// before reporting this child done, so a racing daemon shutdown
			// doesn't close before exit info is persisted.
			deadline := time.Now().Add(perChildShutdown + perChildKill)
			for time.Now().Before(deadline) {
				if _, alive := c.cm.Get(id); !alive {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			done <- result{id: id, err: err}
		}()
	}

	var errs []error
	remaining := len(ids)
	for remaining > 0 {
		select {
		case r := <-done:
			remaining--
			if r.err != nil {
				slog.Warn("child shutdown error", "childId", r.id, "error", r.err)
				errs = append(errs, fmt.Errorf("child %s: %w", r.id, r.err))
			} else {
				slog.Info("child shut down", "childId", r.id)
			}
		case <-ctx.Done():
			slog.Warn("graceful shutdown deadline exceeded", "remaining", remaining)
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) Forget(childID string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return &server.ControllerError{Code: protocol.ErrNotFound, Message: "child not found: " + childID}
	}
	if snap.Status != protocol.StatusExited {
		return &server.ControllerError{Code: protocol.ErrNotExited, Message: "child is still running"}
	}

	// Wait for handleChildExit (running on monitorChild's goroutine) to finish
	// before we delete on-disk state.  MarkExited (which flips status to
	// exited) and writeRecord (which atomically writes the .json) happen
	// inside handleChildExit, and the FINAL step of that function is
	// cm.Remove(childID).  Without this wait, our delete can race with the
	// atomic-rename: writeRecord's .tmp is in-progress when Forget runs
	// os.Remove(.json) — finds nothing — then writeRecord completes the
	// rename, leaving an orphan .json that pic ls picks up on the next
	// daemon restart via loadOrphans.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, alive := c.cm.Get(childID); !alive {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	c.st.Delete(childID)
	if err := persist.DeleteRecord(c.stateDir, childID); err != nil {
		slog.Warn("delete state record", "childId", childID, "error", err)
	}
	if err := c.deleteLogDump(childID); err != nil {
		slog.Warn("delete log dump", "childId", childID, "error", err)
	}
	return nil
}

// deleteLogDump removes the per-child log dump directory at ~/.pi/run/logs/<childID>.
// Forget calls this so 'pic forget' fully removes the child's footprint rather
// than leaving orphan dumps to accumulate forever.  Missing directory is not
// an error (no dump was written, e.g. for a child that crashed pre-Idle).
func (c *Controller) deleteLogDump(childID string) error {
	if c.logsDir == "" {
		return nil
	}
	path := filepath.Join(c.logsDir, childID)
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
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
		if err := c.deleteLogDump(s.ChildID); err != nil {
			slog.Warn("delete log dump", "childId", s.ChildID, "error", err)
		}
		count++
	}
	return count, nil
}

// SetLabels mutates labels on the named child. Rejects keys with the pic/
// prefix or invalid characters. Emits ctrl_child_labeled to subscribers.
func (c *Controller) SetLabels(childID string, set map[string]string, remove []string) (map[string]string, error) {
	if _, ok := c.st.Get(childID); !ok {
		return nil, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	if err := validateUserLabelKeys(set); err != nil {
		return nil, &server.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	if err := validateUserRemoveKeys(remove); err != nil {
		return nil, &server.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	merged, err := c.st.SetLabels(childID, set, remove)
	if err != nil {
		return nil, &server.ControllerError{Code: protocol.ErrChildNotFound, Message: "child not found: " + childID}
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after set_labels", "childId", childID, "error", err)
	}
	c.emitChildLabeled(childID, merged)
	return merged, nil
}

// emitChildLabeled broadcasts a ctrl_child_labeled event carrying the full
// post-mutation label map to global, per-child, and label-filtered subscribers.
//
// Label-filtered delivery uses the NEW (post-mutation) labels for matching.
// Subscribers that matched the old labels but not the new simply stop
// receiving future events — no synthetic "left filter" event is emitted (v1).
func (c *Controller) emitChildLabeled(childID string, labels map[string]string) {
	evt := protocol.CtrlChildLabeled{
		Type:    protocol.TypeCtrlChildLabeled,
		ChildID: childID,
		Labels:  labels,
	}
	if b, err := json.Marshal(evt); err == nil {
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		// Use new labels for dynamic filter evaluation.
		c.cm.DeliverToMatching(childID, labels, b)
	}
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

// SubscribeLabeled registers conn as a label-filtered subscriber. Events are
// delivered from every child whose labels match the filter, evaluated
// dynamically on each event. A nil filter means "pass everything".
// Cleanup occurs automatically in OnConnectionClose.
func (c *Controller) SubscribeLabeled(conn server.Connection, labels map[string]string, hasLabel []string, filter protocol.SubscribeFilter) error {
	var fp *protocol.SubscribeFilter
	if filter.Profile != "" || len(filter.Include) > 0 || len(filter.Exclude) > 0 {
		f := filter
		fp = &f
	}
	c.cm.RegisterLabeled(conn, labels, hasLabel, fp)
	return nil
}

// OnConnectionClose is called by the server when a client connection closes.
// It removes global and label-filtered subscriptions held by this connection.
//
// Note: per-child subscribers for this connection are not cleaned up here;
// they accumulate until the child is removed from the ChildManager on exit.
// This is a known limitation — per-child sub sets are bounded by the child
// lifetime and the subscriber count is small in practice.
func (c *Controller) OnConnectionClose(conn server.Connection) {
	c.cm.GlobalUnsubscribe(conn)
	c.cm.RemoveLabeledSubsForConn(conn)
}

// ─── monitorChild ─────────────────────────────────────────────────────────────

// monitorChild runs as a goroutine for each live child. It forwards bus events
// to per-child subscribers (wrapped in ctrl_event envelopes per §7.1), tracks
// status transitions, handles rename detection, model-label updates, and child exit.
func (c *Controller) monitorChild(childID string, ch *child.Child) {
	busCh, cancel := ch.Bus().Subscribe()
	defer cancel()

	lastStatus := ch.Status()

	// Initialise last-known name from the store so we can detect renames.
	// Initialise last-known model so we can detect model changes (set_model/cycle_model).
	lastKnownName := ""
	lastKnownModel := ""
	lastKnownSessionID := ""
	lastKnownSessionFile := ""
	slashSynced := false
	if snap, ok := c.st.Get(childID); ok {
		lastKnownName = snap.Name
		lastKnownModel = joinModel(snap.Provider, snap.Model)
		lastKnownSessionID = snap.SessionID
		lastKnownSessionFile = snap.SessionFile
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
			wrapped := wrapCtrlEvent(childID, frame)
			c.cm.DeliverToChild(childID, wrapped)
			// Deliver to label-filtered subscribers using the child's current labels.
			if snap, ok := c.st.Get(childID); ok {
				c.cm.DeliverToMatching(childID, snap.Labels, wrapped)
			}

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
			md := ch.Metadata()
			if md.SessionName != "" && md.SessionName != lastKnownName {
				c.handleChildRenamed(childID, md.SessionName, lastKnownName)
				lastKnownName = md.SessionName
			}

			// Detect model changes from set_model / cycle_model responses.
			// Update the store, persist, and emit ctrl_child_labeled.
			if md.Model != "" && md.Model != lastKnownModel {
				c.handleModelChange(childID, md.Model)
				lastKnownModel = md.Model
			}

			// Sync session id / file once they appear. For ReadyOnSpawn children
			// (claude) the process is silent until prompted, so these are unknown
			// at spawn and only surface on the first turn's init; without this the
			// store would keep the empty session id captured at activate time and
			// resume could not re-attach.
			if (md.SessionID != "" && md.SessionID != lastKnownSessionID) ||
				(md.SessionFile != "" && md.SessionFile != lastKnownSessionFile) {
				c.handleSessionMetaChange(childID, md.SessionID, md.SessionFile)
				lastKnownSessionID = md.SessionID
				lastKnownSessionFile = md.SessionFile
			}

			// Capture claude's advertised slash commands once they appear
			// (claude emits them in the init frame; static for the session).
			if len(md.SlashCommands) > 0 && !slashSynced {
				sc := md.SlashCommands
				_ = c.st.Update(childID, func(s *store.Session) { s.SlashCommands = sc })
				slashSynced = true
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
						wrapped := wrapCtrlEvent(childID, frame)
						c.cm.DeliverToChild(childID, wrapped)
						if snap, ok := c.st.Get(childID); ok {
							c.cm.DeliverToMatching(childID, snap.Labels, wrapped)
						}
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
// filter by inner event type.
func wrapCtrlEvent(childID string, raw []byte) []byte {
	env := protocol.CtrlEvent{
		Type:    protocol.TypeCtrlEvent,
		ChildID: childID,
		Event:   json.RawMessage(raw),
	}
	b, _ := json.Marshal(env)
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
		// Deliver to global, per-child, and label-filtered subscribers (spec §7.4).
		c.cm.DeliverToGlobal(b)
		c.cm.DeliverToChild(childID, b)
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
	}
}

// handleModelChange updates the store's Provider/Model fields and the
// pic/model + pic/provider auto-labels when the sniffer detects a model change
// via set_model or cycle_model responses. Emits ctrl_child_labeled.
func (c *Controller) handleModelChange(childID, modelStr string) {
	provider, model := splitModel(modelStr)
	_ = c.st.Update(childID, func(s *store.Session) {
		s.Provider = provider
		s.Model = model
		if s.Labels == nil {
			s.Labels = make(map[string]string)
		}
		if provider != "" {
			s.Labels["pic/provider"] = provider
		} else {
			delete(s.Labels, "pic/provider")
		}
		if model != "" {
			s.Labels["pic/model"] = model
		} else {
			delete(s.Labels, "pic/model")
		}
	})
	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after model change", "childId", childID, "error", err)
	}
	c.emitChildLabeled(childID, snap.Labels)
}

// handleSessionMetaChange syncs the child's sniffed session id / file into the
// store and persists the record. For ReadyOnSpawn children (claude) these are
// unknown at spawn — the process is silent until prompted — and only appear on
// the first turn's init; without this sync the store keeps the empty session id
// captured at activate time and resume cannot re-attach.
func (c *Controller) handleSessionMetaChange(childID, sessionID, sessionFile string) {
	_ = c.st.Update(childID, func(s *store.Session) {
		if sessionID != "" {
			s.SessionID = sessionID
		}
		if sessionFile != "" {
			s.SessionFile = sessionFile
		}
	})
	if err := c.writeRecord(childID); err != nil {
		slog.Warn("write state record after session meta change", "childId", childID, "error", err)
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
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
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
	// RenderRecent returns the render-ring events WITH real timestamps (nil for
	// pi children that have no render-ring), so Since-filtering on the in-memory
	// ExitedRenderRing stays consistent with the live render path.
	renderEvents := ch.RenderRecent(ring.Query{})

	// MarkExited sets Status, ExitedAt, ExitCode, ExitSignal, and ExitedRing
	// atomically under one sess.mu hold so a concurrent Snapshot() cannot
	// observe Status=Exited with ExitedRing still nil.
	c.st.MarkExited(childID, now, res.ExitCode, res.Signal, ringSnapshot, renderEvents)

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
		renderBytes := make([][]byte, len(renderEvents))
		for i, e := range renderEvents {
			renderBytes[i] = e.Bytes
		}
		if err := c.dumper.Dump(childID, ch.InSnapshot(), ch.RingSnapshot(), renderBytes, ch.StderrSnapshot(), meta, exitInfo); err != nil {
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
		// Deliver to per-child and global subscribers BEFORE Remove so the
		// subscriber list is still reachable (spec §7.3).
		c.cm.DeliverToChild(childID, b)
		c.cm.DeliverToGlobal(b)
		// Deliver to label-filtered subscribers; read snap (already marked exited).
		if snap, ok := c.st.Get(childID); ok {
			c.cm.DeliverToMatching(childID, snap.Labels, b)
		}
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

// resolveClaudeBinary resolves the Claude Code CLI binary path. Precedence:
// explicit override → CLAUDE_BINARY env → ~/.local/bin/claude (the path the
// user's claudew/claudep wrappers exec) → PATH lookup of "claude".
func resolveClaudeBinary(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("CLAUDE_BINARY"); env != "" {
		return env, nil
	}
	if u, err := user.Current(); err == nil {
		cand := filepath.Join(u.HomeDir, ".local", "bin", "claude")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return exec.LookPath("claude")
}

// buildClaudeArgv converts a SpawnRequest into the claude CLI argument list
// (excluding the binary itself) for stream-json bidirectional driving.
func buildClaudeArgv(req protocol.SpawnRequest) []string {
	argv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		// AskUserQuestion has no interactive renderer in headless -p mode: claude
		// self-resolves it with an error ("Answer questions?") in the same turn,
		// then the agent falls back to asking in prose — which round-trips fine
		// over the prompt/steer channel. Disallow the dead tool so it never wastes
		// a turn attempting it. Variadic flag stops at the next --option below.
		"--disallowedTools", "AskUserQuestion",
	}
	if req.Model != "" {
		argv = append(argv, "--model", req.Model)
	}
	if req.ResumeSession != "" {
		argv = append(argv, "--resume", req.ResumeSession)
	}
	if req.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", req.AppendSystemPrompt)
	}
	argv = append(argv, req.ExtraArgs...)
	return argv
}

// resolveSpawnPlan picks the binary, argv, and ProtocolProvider for a spawn
// request based on its Kind. Empty Kind defaults to "pi". Shared by Spawn and
// Resume so the two paths can never diverge on protocol selection.
func resolveSpawnPlan(req protocol.SpawnRequest) (bin string, argv []string, prov child.ProtocolProvider, err error) {
	kind := req.Kind
	if kind == "" {
		kind = "pi"
	}
	switch kind {
	case "claude":
		bin, err = resolveClaudeBinary(req.PiBinary)
		return bin, buildClaudeArgv(req), child.ClaudeProvider{}, err
	case "pi":
		bin, err = resolvePiBinary(req.PiBinary)
		return bin, buildArgv(req), child.PiProvider{}, err
	default:
		return "", nil, nil, fmt.Errorf("unknown kind: %s", kind)
	}
}

// spawnKindLabel normalizes a SpawnRequest/snapshot Kind into the value used for
// the pic/kind auto-label. Empty defaults to "pi" (the implicit default kind),
// matching resolveSpawnPlan's kind handling.
func spawnKindLabel(kind string) string {
	if kind == "" {
		return "pi"
	}
	return kind
}

// claudeEnv returns the extra env entries a claude child needs. Currently just
// CLAUDE_CONFIG_DIR; returns nil when configDir is empty so the child inherits
// the controller's default config dir.
func claudeEnv(configDir string) []string {
	if configDir == "" {
		return nil
	}
	return []string{"CLAUDE_CONFIG_DIR=" + configDir}
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
	if !matchesLabelFilter(snap.Labels, f.Labels, f.HasLabel) {
		return false
	}
	return true
}

// matchesLabelFilter returns true when labels satisfies both the AND-match
// required map and the key-presence hasLabels list.
func matchesLabelFilter(labels, required map[string]string, hasLabels []string) bool {
	for k, v := range required {
		if labels[k] != v {
			return false
		}
	}
	for _, k := range hasLabels {
		if _, ok := labels[k]; !ok {
			return false
		}
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
		Kind:               rec.Kind,
		ConfigDir:          rec.ConfigDir,
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
		ExitedAt:           time.UnixMilli(rec.ExitedAt),
		ExitCode:           rec.ExitCode,
		ExitSignal:         rec.ExitSignal,
		Labels:             rec.Labels,
	}
}

// recordFromSnapshot builds a persist.Record from a store Snapshot.
func recordFromSnapshot(snap store.Snapshot) persist.Record {
	return persist.Record{
		ChildID:            snap.ChildID,
		PID:                snap.PID,
		Name:               snap.Name,
		Cwd:                snap.Cwd,
		Kind:               snap.Kind,
		ConfigDir:          snap.ConfigDir,
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
		ExitedAt:           snap.ExitedAt.UnixMilli(),
		ExitCode:           snap.ExitCode,
		ExitSignal:         snap.ExitSignal,
		Labels:             snap.Labels,
	}
}
