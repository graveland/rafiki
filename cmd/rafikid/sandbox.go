package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/users"
)

// The four persisted sandbox states, matching the CHECK constraint in migration
// 0047. They are literals on the wire and in the database, so a rename here is
// a schema change, not a refactor.
const (
	sandboxStateCreating = "creating"
	sandboxStateReady    = "ready"
	sandboxStateLost     = "lost"
	sandboxStateRemoving = "removing"
)

// sandboxOwnerVolumeKeyLen is how much of the owner's id prefixes a named
// volume, matching the brief. CreateBody refuses an owner key that is not
// purely alphanumeric, so the derivation strips separators first.
const sandboxOwnerVolumeKeyLen = 12

// sandboxStopGraceSeconds is what StopContainer waits before SIGKILL. Docker's
// own default is 10; a sandbox runs `rafiki executor serve`, which shuts down
// promptly, so the default is right.
const sandboxStopGraceSeconds = 10

// sandboxConnectPollInterval and sandboxConnectBudget bound the wait for a
// freshly created container's executor to join the pool. The budget is generous
// because the image may be cold-starting a whole runtime; the poll is frequent
// enough that a warm image connects in well under a second.
const (
	sandboxConnectPollInterval = 250 * time.Millisecond
	sandboxConnectBudget       = 60 * time.Second
)

// sandboxMinReapAge is how long a spawn-block row must have existed before the
// reaper may remove it on the strength of its owning child's row being absent.
// sandboxCreateForSpawn inserts the row BEFORE the spawn writes the child's own
// DB row, so an absent child row means "not written yet" as often as "closed";
// a young row is never reaped on that test alone (2× the connect budget means a
// still-connecting sandbox is never reaped).
const sandboxMinReapAge = 2 * sandboxConnectBudget

// sandboxOwnerVolumeKey derives the short, stable owner key named volumes are
// prefixed with. owner.UserID is a uuidv7 string; the FIRST 12 characters of a
// uuidv7 are its 48-bit millisecond timestamp, so two users minted in the same
// millisecond would share a key and their named volumes would collide across
// owners — a cross-owner leak, because the volume NAME is caller-supplied. The
// key therefore comes from the TAIL of the id (uuidv7's random bits), and only
// alphanumerics survive, since CreateBody rejects a key containing '-'.
//
// An empty owner (the anonymous local socket) keys "local", matching "or
// 'local' when empty".
func sandboxOwnerVolumeKey(userID string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return -1
		}
	}, userID)
	if clean == "" {
		return "local"
	}
	if len(clean) > sandboxOwnerVolumeKeyLen {
		clean = clean[len(clean)-sandboxOwnerVolumeKeyLen:]
	}
	return clean
}

// requireSandboxStore fails closed when the daemon has no sandbox table. There
// is no DB-less sandbox path — the row is what records every access-gating
// fact, so without it nothing may run.
func (c *Controller) requireSandboxStore() error {
	if c.sandboxStore == nil {
		return &connectapi.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "sandboxes require a database",
		}
	}
	return nil
}

// requireSandboxRuntime additionally requires the executor store and pool the
// provisioning flow mints an executor into and reaches the launcher through.
func (c *Controller) requireSandboxRuntime() error {
	if err := c.requireSandboxStore(); err != nil {
		return err
	}
	if c.execStore == nil || c.execPool == nil {
		return &connectapi.ControllerError{
			Code:    protocol.ErrInternal,
			Message: "sandboxes require an executor store and pool (requires RAFIKI_DB; also requires RAFIKI_EXECUTORS_ENABLED=1 when RAFIKI_CONTROL_LISTEN is set)",
		}
	}
	return nil
}

// SandboxCreate provisions a NAMED sandbox for owner and returns it once its
// executor has joined the pool. Every failure rolls back the steps before it
// (best-effort, logged) and returns the original error.
//
// callerChild is the child credential's id, or "" for an operator. A child
// caller's identity has already been resolved by the adapter to its owner's
// NON-admin identity, so owner is authoritative here.
func (c *Controller) SandboxCreate(ctx context.Context, owner users.Identity, callerChild string, spec protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	if err := c.requireSandboxRuntime(); err != nil {
		return protocol.SandboxInfo{}, err
	}
	launcher, err := c.resolveSandboxLauncher(callerChild, owner, spec.Launcher)
	if err != nil {
		return protocol.SandboxInfo{}, err
	}
	resolved, err := sandbox.Validate(spec, c.sandboxCfg, launcher.Describe.GetSandboxMountRoots(), sandbox.Caller{Child: callerChild != ""}, true)
	if err != nil {
		return protocol.SandboxInfo{}, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	if err := c.checkSandboxCap(ctx, owner.UserID); err != nil {
		return protocol.SandboxInfo{}, err
	}

	rowID := "sbx_" + ulid.Make().String()
	specJSON, err := json.Marshal(resolved.SandboxSpec)
	if err != nil {
		return protocol.SandboxInfo{}, fmt.Errorf("encoding sandbox spec: %w", err)
	}
	expires := time.Now().Add(resolved.TTL)
	row := sandbox.Row{
		ID:                 rowID,
		OwnerUserID:        owner.UserID,
		Name:               resolved.Name,
		LauncherExecutorID: launcher.Executor.ID,
		Image:              resolved.Image,
		Spec:               specJSON,
		CreatedBy:          callerChild,
		State:              sandboxStateCreating,
		ExpiresAt:          &expires,
	}

	provisioned, executor, err := c.sandboxProvision(ctx, owner, launcher, row, resolved, spec.Name, callerChild, sandboxOwnerVolumeKey(owner.UserID))
	if err != nil {
		return protocol.SandboxInfo{}, err
	}
	var outSpec protocol.SandboxSpec
	_ = json.Unmarshal(provisioned.Spec, &outSpec)
	return sandboxInfoFromRow(provisioned, outSpec, executor.ID != "" && c.executorLive(executor.ID), sandboxStateReady), nil
}

// sandboxCreateForSpawn is SandboxCreate for a spawn block: unnamed, tied to a
// child, with a generated machine name and no TTL. It returns the executor the
// spawn binds. Used by the spawn flow (task 5.1).
func (c *Controller) sandboxCreateForSpawn(ctx context.Context, owner users.Identity, parentChild, newChildID string, spec protocol.SandboxSpec) (executors.Executor, string, error) {
	if err := c.requireSandboxRuntime(); err != nil {
		return executors.Executor{}, "", err
	}
	launcher, err := c.resolveSandboxLauncher(parentChild, owner, spec.Launcher)
	if err != nil {
		return executors.Executor{}, "", err
	}
	resolved, err := sandbox.Validate(spec, c.sandboxCfg, launcher.Describe.GetSandboxMountRoots(), sandbox.Caller{Child: true}, false)
	if err != nil {
		return executors.Executor{}, "", &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	if err := c.checkSandboxCap(ctx, owner.UserID); err != nil {
		return executors.Executor{}, "", err
	}

	rowID := "sbx_" + ulid.Make().String()
	machine := sandboxSpawnMachineName(rowID)
	specJSON, err := json.Marshal(resolved.SandboxSpec)
	if err != nil {
		return executors.Executor{}, "", fmt.Errorf("encoding sandbox spec: %w", err)
	}
	row := sandbox.Row{
		ID:                 rowID,
		OwnerUserID:        owner.UserID,
		LauncherExecutorID: launcher.Executor.ID,
		Image:              resolved.Image,
		Spec:               specJSON,
		CreatedBy:          parentChild,
		OwnerChild:         newChildID,
		Scope:              spec.Scope,
		State:              sandboxStateCreating,
	}
	provisioned, executor, err := c.sandboxProvision(ctx, owner, launcher, row, resolved, machine, newChildID, sandboxOwnerVolumeKey(owner.UserID))
	if err != nil {
		return executors.Executor{}, "", err
	}
	return executor, provisioned.ID, nil
}

// sandboxSpawnMachineName is the generated machine label for a spawn-block
// sandbox: "sbx-" plus the 12 characters of the row id after its "sbx_" prefix,
// lowercased (a ULID, so already lower). It is a valid machine name and unique
// per row.
func sandboxSpawnMachineName(rowID string) string {
	rest := strings.TrimPrefix(rowID, "sbx_")
	if len(rest) > 12 {
		rest = rest[:12]
	}
	return "sbx-" + strings.ToLower(rest)
}

// resolveSandboxLauncher picks the ONE live executor advertising the docker
// proxy that the creator's effective set admits.
//
// creatorChild is the caller's child id ("" for an operator). The candidate set
// is the creator's effective executor set, so a child can never reach a docker
// launcher outside its lineage grant. spec.Launcher, when set, narrows to the
// one launcher whose machine label or id matches.
func (c *Controller) resolveSandboxLauncher(creatorChild string, owner users.Identity, ref string) (execpool.LiveExecutor, error) {
	labels := map[string]string{}
	if creatorChild != "" {
		if snap, ok := c.st.Get(creatorChild); ok {
			labels = snap.Labels
		}
	} else if name, err := sessionOwner(owner); err == nil && name != "" {
		labels = map[string]string{"owner": name}
	}
	set, err := c.effectiveExecutorSetFor(creatorChild, labels, owner.UserID)
	if err != nil {
		return execpool.LiveExecutor{}, err
	}
	byID := map[string]execpool.LiveExecutor{}
	for _, le := range c.execPool.Live() {
		byID[le.Executor.ID] = le
	}
	var docker []execpool.LiveExecutor
	for _, e := range set {
		le, ok := byID[e.ID]
		if !ok {
			continue
		}
		if hasProxy(le.Proxies, sandbox.DockerProxyName) {
			docker = append(docker, le)
		}
	}
	if ref != "" {
		var kept []execpool.LiveExecutor
		for _, le := range docker {
			if le.Executor.Labels["machine"] == ref || le.Executor.ID == ref {
				kept = append(kept, le)
			}
		}
		docker = kept
	}

	switch len(docker) {
	case 1:
	case 0:
		if ref != "" {
			return execpool.LiveExecutor{}, &connectapi.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("no docker launcher named %q is in scope; declare one with --proxy docker=unix:///var/run/docker.sock", ref),
			}
		}
		return execpool.LiveExecutor{}, &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: "no docker launcher is in scope for this sandbox — enroll an executor with " +
				"--proxy docker=unix:///var/run/docker.sock",
		}
	default:
		names := make([]string, 0, len(docker))
		for _, le := range docker {
			names = append(names, sandboxLauncherName(le))
		}
		return execpool.LiveExecutor{}, &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("several docker launchers are in scope (%s); set launcher to one of them",
				strings.Join(names, ", ")),
		}
	}

	le := docker[0]
	if le.Describe == nil || le.Describe.GetSandboxRelayDir() == "" {
		return execpool.LiveExecutor{}, &connectapi.ControllerError{
			Code: protocol.ErrInvalidArgs,
			Message: fmt.Sprintf("launcher %q declares no --relay-dir, so a sandbox cannot connect back to it",
				sandboxLauncherName(le)),
		}
	}
	if c.sandboxEngine == nil || c.sandboxEngine(le.Executor.ID) == nil {
		return execpool.LiveExecutor{}, &connectapi.ControllerError{
			Code:    protocol.ErrInternal,
			Message: fmt.Sprintf("sandbox engine is not wired for launcher %q", sandboxLauncherName(le)),
		}
	}
	return le, nil
}

// sandboxLauncherName is the machine label if present, else the id — what an
// operator would type as launcher.
func sandboxLauncherName(le execpool.LiveExecutor) string {
	if m := le.Executor.Labels["machine"]; m != "" {
		return m
	}
	return le.Executor.ID
}

// checkSandboxCap refuses when the owner already holds MaxPerOwner live
// sandboxes, naming what they hold.
func (c *Controller) checkSandboxCap(ctx context.Context, ownerUserID string) error {
	if c.sandboxCfg.MaxPerOwner <= 0 {
		return nil
	}
	rows, err := c.sandboxStore.ListLive(ctx, ownerUserID)
	if err != nil {
		return err
	}
	if len(rows) < c.sandboxCfg.MaxPerOwner {
		return nil
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != "" {
			names = append(names, r.Name)
		} else {
			names = append(names, r.ID)
		}
	}
	return &connectapi.ControllerError{
		Code: protocol.ErrInvalidArgs,
		Message: fmt.Sprintf("sandbox limit reached (%d); remove one first: %s",
			c.sandboxCfg.MaxPerOwner, strings.Join(names, ", ")),
	}
}

// sandboxProvision runs the create → executor → container → wait flow for one
// prepared row, rolling back on any failure. owner is the sandbox's owner
// (whose name stamps the executor's owner label); name is the executor machine
// name (the sandbox name for a named sandbox, a generated one for a spawn
// block); ownerChild is "" for a named sandbox; ownerKey scopes named volumes.
func (c *Controller) sandboxProvision(
	ctx context.Context,
	owner users.Identity,
	launcher execpool.LiveExecutor,
	row sandbox.Row,
	resolved sandbox.Resolved,
	name, ownerChild, ownerKey string,
) (sandbox.Row, executors.Executor, error) {
	rowID := row.ID
	engine := c.sandboxEngine(launcher.Executor.ID)

	var (
		executorID  string
		containerID string
		succeeded   bool
	)
	rollback := func() {
		rctx := context.WithoutCancel(ctx)
		if containerID != "" {
			if err := engine.RemoveContainer(rctx, containerID, true, true); err != nil {
				slog.Warn("sandbox create rollback: remove container failed", "sandboxId", rowID, "containerId", containerID, "error", err)
			}
		}
		if executorID != "" {
			c.execPool.Evict(executorID)
			if err := c.execStore.Delete(rctx, executorID); err != nil && !errors.Is(err, executors.ErrNotFound) {
				slog.Warn("sandbox create rollback: delete executor failed", "sandboxId", rowID, "executorId", executorID, "error", err)
			}
		}
		if err := c.sandboxStore.MarkRemoved(rctx, rowID, time.Now()); err != nil {
			slog.Warn("sandbox create rollback: mark removed failed", "sandboxId", rowID, "error", err)
		}
	}
	defer func() {
		if !succeeded {
			rollback()
		}
	}()

	// 6. Insert the row first, state=creating: the container is created only
	// after its row exists, so a `creating` row never owns a container the
	// reaper might misjudge.
	if err := c.sandboxStore.Insert(ctx, row); err != nil {
		if errors.Is(err, sandbox.ErrNameTaken) {
			return sandbox.Row{}, executors.Executor{}, &connectapi.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("a sandbox named %q already exists", resolved.Name),
			}
		}
		return sandbox.Row{}, executors.Executor{}, err
	}

	// 7. Mint the sandbox's own executor row. The credential is returned once
	// and never stored or logged.
	labels, err := executorTrustLabels(owner, name, resolved.Labels)
	if err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[sandbox.RowLabelSandbox] = "1"
	labels[sandbox.RowLabelID] = rowID
	if row.CreatedBy != "" {
		labels[sandbox.RowLabelCreatedBy] = row.CreatedBy
	}
	if ownerChild != "" {
		labels[sandbox.RowLabelOwnerChild] = ownerChild
		labels[sandbox.RowLabelScope] = string(row.Scope)
	}
	roots := sandboxRoots(resolved)
	executor, credential, err := c.execStore.Create(ctx, executors.NewToken{
		Labels:        labels,
		Roots:         roots,
		Isolation:     "container",
		WorkspaceMode: "pinned",
		Admits:        "",
		OwnerUserID:   row.OwnerUserID,
	})
	if err != nil {
		return sandbox.Row{}, executors.Executor{}, translateExecutorErr(fmt.Errorf("create sandbox executor: %w", err))
	}
	executorID = executor.ID
	row.ExecutorID = executor.ID

	// 8. Engine over the launcher's docker proxy.
	exists, err := engine.ImageExists(ctx, resolved.Image)
	if err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}
	if !exists {
		if err := engine.PullImage(ctx, resolved.Image); err != nil {
			return sandbox.Row{}, executors.Executor{}, err
		}
	}
	body, err := sandbox.CreateBody(resolved, sandbox.CreateInputs{
		SandboxID:      rowID,
		OwnerChild:     ownerChild,
		Credential:     credential,
		RelayHostDir:   launcher.Describe.GetSandboxRelayDir(),
		OwnerVolumeKey: ownerKey,
	})
	if err != nil {
		return sandbox.Row{}, executors.Executor{}, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: err.Error()}
	}
	containerName := sandbox.ContainerNamePrefix + rowID
	containerID, err = engine.CreateContainer(ctx, containerName, body)
	if err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}
	if err := c.sandboxStore.SetContainer(ctx, rowID, containerID); err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}
	row.ContainerID = containerID
	if err := engine.StartContainer(ctx, containerID); err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}

	// 9. Wait for the sandbox's executor to join the pool.
	if err := c.waitForSandboxExecutor(ctx, executor.ID); err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}
	if err := c.sandboxStore.SetState(ctx, rowID, sandboxStateReady); err != nil {
		return sandbox.Row{}, executors.Executor{}, err
	}
	row.State = sandboxStateReady
	succeeded = true
	return row, executor, nil
}

// waitForSandboxExecutor polls the pool for executorID until it appears, the
// budget runs out, or ctx is cancelled.
func (c *Controller) waitForSandboxExecutor(ctx context.Context, executorID string) error {
	deadline := time.Now().Add(sandboxConnectBudget)
	for {
		if c.executorLive(executorID) {
			return nil
		}
		if time.Now().After(deadline) {
			return &connectapi.ControllerError{
				Code:    protocol.ErrInternal,
				Message: "the sandbox did not connect: its image must run `rafiki executor serve` and reach the launcher's relay socket",
			}
		}
		select {
		case <-ctx.Done():
			return &connectapi.ControllerError{
				Code:    protocol.ErrInternal,
				Message: "the sandbox did not connect before the request ended: its image must run `rafiki executor serve` and reach the launcher's relay socket",
			}
		case <-time.After(sandboxConnectPollInterval):
		}
	}
}

// executorLive reports whether executorID currently has a connection in the
// pool.
func (c *Controller) executorLive(executorID string) bool {
	if executorID == "" || c.execPool == nil {
		return false
	}
	for _, le := range c.execPool.Live() {
		if le.Executor.ID == executorID {
			return true
		}
	}
	return false
}

// sandboxRoots is the executor row's roots: every mount target plus the
// workdir, describing the container's view for humans and selectors. It
// enforces nothing — the mounts are the grant.
func sandboxRoots(resolved sandbox.Resolved) []string {
	roots := make([]string, 0, len(resolved.Mounts)+1)
	for _, m := range resolved.Mounts {
		roots = append(roots, m.Target)
	}
	if resolved.Workdir != "" {
		roots = append(roots, resolved.Workdir)
	}
	return roots
}

// SandboxList returns the owner's live sandboxes. Connected reflects the pool;
// a `ready` row whose executor is gone is reported `lost` when its launcher is
// live and confirms the container is missing (and persisted as such).
func (c *Controller) SandboxList(owner users.Identity) ([]protocol.SandboxInfo, error) {
	if err := c.requireSandboxStore(); err != nil {
		return nil, err
	}
	ctx := context.Background()
	rows, err := c.sandboxStore.ListLive(ctx, owner.UserID)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	if c.execPool != nil {
		for _, le := range c.execPool.Live() {
			live[le.Executor.ID] = true
		}
	}
	out := make([]protocol.SandboxInfo, 0, len(rows))
	for _, r := range rows {
		connected := live[r.ExecutorID]
		state := r.State
		if state == sandboxStateReady && !connected {
			state = c.sandboxObservedState(ctx, r, live, state)
		}
		var spec protocol.SandboxSpec
		if len(r.Spec) > 0 {
			_ = json.Unmarshal(r.Spec, &spec)
		}
		out = append(out, sandboxInfoFromRow(r, spec, connected, state))
	}
	return out, nil
}

// sandboxObservedState downgrades a ready row to lost when its launcher is live
// and reports the container missing. It persists the downgrade (idempotent).
// When the launcher is not live the stored state is kept — this daemon cannot
// tell, and guessing lost would flap.
func (c *Controller) sandboxObservedState(ctx context.Context, r sandbox.Row, live map[string]bool, stored string) string {
	if !live[r.LauncherExecutorID] || r.ContainerID == "" || c.sandboxEngine == nil {
		return stored
	}
	engine := c.sandboxEngine(r.LauncherExecutorID)
	if engine == nil {
		return stored
	}
	_, found, err := engine.InspectContainer(ctx, r.ContainerID)
	if err != nil || found {
		return stored
	}
	if err := c.sandboxStore.SetState(ctx, r.ID, sandboxStateLost); err != nil {
		slog.Warn("sandbox: could not persist lost state", "sandboxId", r.ID, "error", err)
	}
	return sandboxStateLost
}

// SandboxRemove removes one of the owner's live sandboxes by name or row id.
//
// Authority: an operator (callerChild == "") may remove any of the owner's
// rows; a child may remove a row it created or one a descendant of it created.
func (c *Controller) SandboxRemove(ctx context.Context, owner users.Identity, callerChild, ref string) error {
	if err := c.requireSandboxStore(); err != nil {
		return err
	}
	row, err := c.resolveSandboxRef(ctx, owner.UserID, ref)
	if err != nil {
		return err
	}
	if callerChild != "" {
		allowed := row.CreatedBy == callerChild || c.st.IsDescendant(callerChild, row.CreatedBy)
		if !allowed {
			return &connectapi.ControllerError{
				Code:    protocol.ErrPermissionDenied,
				Message: fmt.Sprintf("sandbox %q was not created by this child or one of its descendants", ref),
			}
		}
	}
	return c.removeSandboxRow(ctx, row)
}

// resolveSandboxRef finds a live row of ownerUserID by name, then by id.
func (c *Controller) resolveSandboxRef(ctx context.Context, ownerUserID, ref string) (sandbox.Row, error) {
	if ref == "" {
		return sandbox.Row{}, &connectapi.ControllerError{Code: protocol.ErrInvalidArgs, Message: "sandbox name or id required"}
	}
	notFound := &connectapi.ControllerError{
		Code:    protocol.ErrNotFound,
		Message: fmt.Sprintf("no sandbox %q for this owner", ref),
	}
	if row, ok, err := c.sandboxStore.GetByName(ctx, ownerUserID, ref); err != nil {
		return sandbox.Row{}, err
	} else if ok {
		return row, nil
	}
	row, ok, err := c.sandboxStore.Get(ctx, ref)
	if err != nil {
		return sandbox.Row{}, err
	}
	if !ok || row.RemovedAt != nil || row.OwnerUserID != ownerUserID {
		return sandbox.Row{}, notFound
	}
	return row, nil
}

// removeSandboxRow tears a row down: state=removing, then the CONTAINER, then
// the executor row, then the tombstone. The container goes first because a
// deleted executor row makes the sandbox's executor exit terminally and its
// `unless-stopped` policy would restart it in a loop.
//
// It re-reads the row by id first, so a caller acting on a stale snapshot (the
// sweep) never writes to a row that has since been tombstoned or changed: an
// already-removed row is a no-op.
//
// If the launcher is not live the row is left `removing` and an error is
// returned, UNLESS the launcher's executor ROW is also gone — a permanently
// dead launcher — in which case the removal proceeds (the container is left to
// the reaper) so the owner's cap slot is freed.
func (c *Controller) removeSandboxRow(ctx context.Context, row sandbox.Row) error {
	fresh, ok, err := c.sandboxStore.Get(ctx, row.ID)
	if err != nil {
		return err
	}
	if !ok || fresh.RemovedAt != nil {
		// Already tombstoned (or gone): nothing to do. This is the re-Get that
		// makes the sweep tolerant of a row removed under it.
		return nil
	}
	row = fresh

	if row.State != sandboxStateRemoving {
		if err := c.sandboxStore.SetState(ctx, row.ID, sandboxStateRemoving); err != nil {
			return err
		}
	}

	launcherGone := false
	if row.ContainerID != "" && !c.executorLive(row.LauncherExecutorID) {
		// The launcher is offline. Is its executor ROW gone too? If so the
		// launcher is permanently gone and waiting is pointless — free the cap
		// slot and leave the container to the reaper. If the row is still there
		// the launcher may come back, so keep the row `removing` and retry.
		switch _, gerr := c.execStore.Get(ctx, row.LauncherExecutorID); {
		case gerr == nil:
			return &connectapi.ControllerError{
				Code:    protocol.ErrInternal,
				Message: "launcher offline; the sandbox will be removed when it reconnects",
			}
		case errors.Is(gerr, executors.ErrNotFound):
			launcherGone = true
		default:
			return gerr
		}
	}
	if row.ContainerID != "" && !launcherGone {
		if c.sandboxEngine == nil {
			return &connectapi.ControllerError{Code: protocol.ErrInternal, Message: "sandbox engine is not wired"}
		}
		engine := c.sandboxEngine(row.LauncherExecutorID)
		if engine == nil {
			return &connectapi.ControllerError{Code: protocol.ErrInternal, Message: "sandbox engine is not wired"}
		}
		if err := engine.StopContainer(ctx, row.ContainerID, sandboxStopGraceSeconds); err != nil {
			return err
		}
		if err := engine.RemoveContainer(ctx, row.ContainerID, true, true); err != nil {
			return err
		}
	}

	if row.ExecutorID != "" {
		c.execPool.Evict(row.ExecutorID)
		if err := c.execStore.Delete(ctx, row.ExecutorID); err != nil && !errors.Is(err, executors.ErrNotFound) {
			return err
		}
	}
	return c.sandboxStore.MarkRemoved(ctx, row.ID, time.Now())
}

// removeSandboxesOwnedBy tears down every live sandbox a child owns — its own
// spawn block and any descendant's. Called by the spawn flow when the owning
// child closes (task 5.1). Best-effort: each failure is logged and the sweep
// retries.
func (c *Controller) removeSandboxesOwnedBy(ctx context.Context, childID string) {
	if c.sandboxStore == nil || childID == "" {
		return
	}
	rows, err := c.sandboxStore.ListAllLive(ctx)
	if err != nil {
		slog.Warn("sandbox: could not list sandboxes for a closing child", "childId", childID, "error", err)
		return
	}
	for _, r := range rows {
		if r.OwnerChild == "" {
			continue
		}
		if r.OwnerChild != childID && !c.st.IsDescendant(childID, r.OwnerChild) {
			continue
		}
		if err := c.removeSandboxRow(ctx, r); err != nil {
			slog.Warn("sandbox: removal of a closing child's sandbox failed", "sandboxId", r.ID, "childId", childID, "error", err)
		}
	}
}

// ownedSandbox answers which child-owned sandbox childID must bind to: the one
// live row whose owner_child is childID, or — for a subtree-scoped row — an
// ancestor of childID.
//
// No eligible row is (zero, false, nil). Exactly one is that row. More than one
// is corruption (nesting is not allowed) and fails closed. A store error is
// returned, never read as "not owned". Used by the spawn flow (task 5.1).
func (c *Controller) ownedSandbox(ctx context.Context, childID, ownerUserID string) (sandbox.Row, bool, error) {
	if c.sandboxStore == nil {
		return sandbox.Row{}, false, &connectapi.ControllerError{Code: protocol.ErrInternal, Message: "sandboxes require a database"}
	}
	rows, err := c.sandboxStore.ListLive(ctx, ownerUserID)
	if err != nil {
		return sandbox.Row{}, false, err
	}
	var eligible []sandbox.Row
	for _, r := range rows {
		if r.OwnerChild == "" {
			continue
		}
		if r.OwnerChild == childID {
			eligible = append(eligible, r)
			continue
		}
		if r.Scope == protocol.ScopeSubtree && c.st.IsDescendant(r.OwnerChild, childID) {
			eligible = append(eligible, r)
		}
	}
	switch len(eligible) {
	case 0:
		return sandbox.Row{}, false, nil
	case 1:
		return eligible[0], true, nil
	default:
		return sandbox.Row{}, false, &connectapi.ControllerError{
			Code: protocol.ErrInternal,
			Message: fmt.Sprintf("child %s is owned by %d live sandboxes; sandbox nesting is not allowed",
				childID, len(eligible)),
		}
	}
}

// sandboxInitialSweepDelay is how long after boot the first sweep runs — long
// enough that recovery has begun, short enough that a stranded row is noticed
// promptly.
const sandboxInitialSweepDelay = 5 * time.Second

// startSandboxSweeper runs sweepSandboxes once shortly after boot and then on
// every SandboxCfg.SweepInterval, on the daemon's lifecycle context. It joins
// sweeperWg so Stop waits for it, the same contract as startSweeper.
func (c *Controller) startSandboxSweeper(ctx context.Context) {
	if c.sandboxStore == nil {
		return
	}
	c.sweeperWg.Add(1)
	go func() {
		defer c.sweeperWg.Done()
		timer := time.NewTimer(sandboxInitialSweepDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.sweepSandboxes(ctx)
		}
		ticker := time.NewTicker(c.sandboxCfg.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.sweepSandboxes(ctx)
			}
		}
	}()
}

// sweepSandboxes is one idempotent pass: orphan-container reaping, TTL expiry,
// finishing rows stuck in `removing`, and the spawn-block reaper. Run by the
// loop in main.go every sandboxCfg.SweepInterval and once shortly after boot.
func (c *Controller) sweepSandboxes(ctx context.Context) {
	if c.sandboxStore == nil {
		return
	}
	now := time.Now()

	// Orphan containers FIRST, and each candidate's row is read AFTER its
	// launcher's container listing (see reapOrphanContainers), so a sandbox
	// created during the listing is seen live and never reaped.
	c.reapOrphanContainers(ctx)

	rows, err := c.sandboxStore.ListAllLive(ctx)
	if err != nil {
		slog.Warn("sandbox sweep: list live failed", "error", err)
		return
	}

	// TTL: every live NAMED row past its expiry is removed with system
	// authority. A `creating` row is skipped — the create flow owns it and its
	// rollback, and its TTL cannot have elapsed anyway.
	for _, r := range rows {
		if r.Name == "" || r.ExpiresAt == nil || !r.ExpiresAt.Before(now) {
			continue
		}
		if r.State == sandboxStateCreating {
			continue
		}
		if err := c.removeSandboxRow(ctx, r); err != nil {
			slog.Warn("sandbox sweep: TTL removal failed", "sandboxId", r.ID, "error", err)
		}
	}

	// Finish rows a prior pass left `removing` (a launcher was offline).
	// removeSandboxRow re-reads each row, so a row removed under this snapshot
	// is a no-op.
	for _, r := range rows {
		if r.State != sandboxStateRemoving {
			continue
		}
		if err := c.removeSandboxRow(ctx, r); err != nil {
			slog.Warn("sandbox sweep: finishing removal failed", "sandboxId", r.ID, "error", err)
		}
	}

	c.reapChildlessSandboxes(ctx, rows, now)
}

// reapOrphanContainers removes, per live docker launcher, every container whose
// sandbox row is absent or tombstoned.
//
// Each candidate's row is read with Get AFTER the container listing, so a
// sandbox created in the window between the listing and this decision has its
// row already and is kept. This is what makes the "a container with a `creating`
// row is kept" invariant hold under a concurrent create.
func (c *Controller) reapOrphanContainers(ctx context.Context) {
	if c.execPool == nil || c.sandboxEngine == nil {
		return
	}
	for _, le := range c.execPool.Live() {
		if !hasProxy(le.Proxies, sandbox.DockerProxyName) {
			continue
		}
		engine := c.sandboxEngine(le.Executor.ID)
		if engine == nil {
			continue
		}
		containers, err := engine.ListContainers(ctx, sandbox.DockerLabelSandbox)
		if err != nil {
			// A launcher that is offline is skipped silently and retried next
			// pass.
			continue
		}
		for _, ct := range containers {
			rowID := ct.Labels[sandbox.DockerLabelSandbox]
			if rowID == "" {
				continue
			}
			// Read the row AFTER the listing: a still-live row means the
			// container is not an orphan.
			row, ok, err := c.sandboxStore.Get(ctx, rowID)
			if err != nil {
				// Cannot decide; never remove on an unreadable store. Retried
				// next pass.
				slog.Warn("sandbox reaper: could not read a container's row", "sandboxId", rowID, "error", err)
				continue
			}
			if ok && row.RemovedAt == nil {
				continue
			}
			if err := engine.RemoveContainer(ctx, ct.ID, true, true); err != nil {
				slog.Warn("sandbox reaper: removing an orphan container failed", "containerId", ct.ID, "sandboxId", rowID, "error", err)
			}
		}
	}
}

// reapChildlessSandboxes removes live spawn-block rows whose owning child's
// PERSISTED row is closed or absent.
//
// A `creating` row is skipped (the create flow owns it), and so is a row
// younger than sandboxMinReapAge: sandboxCreateForSpawn inserts the row before
// the spawn writes the child's own row, so an absent child row on a young row
// usually means "not written yet". The decision reads the PERSISTED child rows
// (List returns live rows only) — never the in-memory store, where a live child
// may not yet be loaded while recovery runs.
func (c *Controller) reapChildlessSandboxes(ctx context.Context, rows []sandbox.Row, now time.Time) {
	liveChildren, childKnown := c.liveChildSet(ctx)
	if !childKnown {
		return
	}
	for _, r := range rows {
		if r.Name != "" || r.OwnerChild == "" {
			continue
		}
		if r.State == sandboxStateCreating {
			continue
		}
		if now.Sub(r.CreatedAt) < sandboxMinReapAge {
			continue
		}
		if liveChildren[r.OwnerChild] {
			continue
		}
		if err := c.removeSandboxRow(ctx, r); err != nil {
			slog.Warn("sandbox reaper: removing a closed child's sandbox failed", "sandboxId", r.ID, "childId", r.OwnerChild, "error", err)
		}
	}
}

// liveChildSet returns the set of child ids with a live persisted row. ok is
// false when there is no child store to consult, in which case the reaper
// removes nothing.
func (c *Controller) liveChildSet(ctx context.Context) (map[string]bool, bool) {
	if c.children == nil {
		return nil, false
	}
	recs, err := c.children.List(ctx)
	if err != nil {
		slog.Warn("sandbox reaper: could not list live children", "error", err)
		return nil, false
	}
	set := make(map[string]bool, len(recs))
	for _, rec := range recs {
		set[rec.ChildID] = true
	}
	return set, true
}

// sandboxInfoFromRow renders a row for a caller.
func sandboxInfoFromRow(row sandbox.Row, spec protocol.SandboxSpec, connected bool, state string) protocol.SandboxInfo {
	var expires *time.Time
	if row.ExpiresAt != nil {
		t := *row.ExpiresAt
		expires = &t
	}
	return protocol.SandboxInfo{
		ID:          row.ID,
		Name:        row.Name,
		ExecutorID:  row.ExecutorID,
		Launcher:    row.LauncherExecutorID,
		ContainerID: row.ContainerID,
		Image:       row.Image,
		Network:     spec.Network,
		State:       state,
		Connected:   connected,
		CreatedBy:   row.CreatedBy,
		OwnerChild:  row.OwnerChild,
		Scope:       row.Scope,
		Labels:      spec.Labels,
		CreatedAt:   row.CreatedAt,
		ExpiresAt:   expires,
	}
}
