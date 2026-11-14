package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/fundi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/skills"
)

// The spawn-time diagnostics this file produces. Each is surfaced to the caller
// in protocol.SpawnResponseData.Warnings (Connect's SpawnResponse.warnings) and
// logged at WARN by Controller.Spawn — which is how a PARENTED spawn's warnings
// become visible, since agent_spawn's tool result deliberately carries none.
const (
	// homeNotConnectedWarning: the child has a home, but its workspace could
	// not be provisioned there, so nothing could read the project tier.
	// Emitted both by the resolver (a RESUMED child whose persisted home is no
	// longer reachable — no home at all, never a different machine) and by the
	// runtime (a home that resolved but would not bind).
	homeNotConnectedWarning = "home executor %s is not connected; project instructions were not loaded"

	// noHomeWarning: no rule named a home and the probe said nothing. The
	// child's workspace tools still work; only its project tier and its
	// "Your machine" block are missing.
	noHomeWarning = "no home executor: project instructions (CLAUDE.md) were not loaded; pass home_executor"

	// unboundWarningPrefix introduces selection's own diagnostic for a spawn
	// that could not reach any executor at all. The text after it is
	// explainNoMatch's per-candidate report, kept verbatim: it is the same
	// message the child's workspace tools will fail with until one connects.
	unboundWarningPrefix = "no executor available yet; the child starts unbound: "
)

// homeProbeTimeout bounds the WHOLE home probe, not each candidate: a spawn
// must not sit for a minute behind a handful of slow executors, and the probe
// exists to place a home, not to prove every machine healthy.
const homeProbeTimeout = 10 * time.Second

// resolveHomeExecutor decides which executor owns the child's cwd, its project
// instructions and its project skills — the HOME — from the reachable set,
// before anything is provisioned. First match wins:
//
//  1. HomeExecutor named by the request. On a resume that value IS the child's
//     persisted rafiki/home-executor label (resumeRequestFromSnapshot), so it
//     reproduces the old home exactly. A miss REFUSES the spawn — for a child
//     caller too, which is what lets SpawnRequest.home_executor be
//     child-allowed — EXCEPT for a persisted home, which yields no home plus
//     the not-connected warning: a child whose machine is briefly offline must
//     not become unresumable, and must never move to another machine silently.
//  2. ExecutorRef, resolved against reach.
//  3. The parent's rafiki/home-executor label, when that executor is reachable:
//     a descendant inherits where its spawner's project tree lives.
//  4. The single reachable executor.
//  5. The executor where the child's cwd exists, probed by provisioning on each
//     in turn. Exactly one hit becomes the home (and its probe workspace is
//     adopted, so nothing provisions twice); several means there is no honest
//     answer and none becomes home; none refuses with the provision diagnostic
//     when every failure was a missing workdir, else reports what could not be
//     checked.
//
// A candidate from rules 2–3 outside reach is skipped, never used: reach is the
// confinement-narrowed live set, and home is a choice WITHIN it. With an empty
// reach the caller's own unbound warning covers the diagnostic, and with a home
// that cannot be bound the runtime warns from spawnHomeInfo — so the no-home
// warning here is only the fallback for a reach set that is neither empty nor
// probed (a multi-executor spawn with no cwd).
func (c *Controller) resolveHomeExecutor(ctx context.Context, req protocol.SpawnRequest, be *boundExecutor, reach []executors.Executor) (homeID string, warnings []string, err error) {
	if req.HomeExecutor != "" {
		// The child's OWN persisted home arriving on a resume/respawn — the
		// request is rebuilt from its record, so this is not a caller
		// declaration and must not be able to strand the child.
		persisted := c.persistedHome(be.childID) == req.HomeExecutor
		if match, ok := matchExecutorRef(req.HomeExecutor, reach); ok {
			return match.ID, nil, nil
		}
		if persisted {
			return "", []string{fmt.Sprintf(homeNotConnectedWarning, req.HomeExecutor)}, nil
		}
		return "", nil, fmt.Errorf("home executor %q is not in this spawn's reachable executors (%s)",
			req.HomeExecutor, reachNames(be.binder, reach))
	}

	// Rules 2 and 3 are CANDIDATES, never authority: an executor outside reach
	// is skipped and resolution carries on, because reach is what confines this
	// child and home can only ever be a member of it.
	if req.ExecutorRef != "" {
		if match, ok := matchExecutorRef(req.ExecutorRef, reach); ok {
			return match.ID, nil, nil
		}
	}
	if req.ParentChildID != "" {
		if parentHome := be.binder.Home(req.ParentChildID); parentHome != "" && reachableID(reach, parentHome) {
			return parentHome, nil, nil
		}
	}

	switch len(reach) {
	case 0:
		// Nothing to choose from. The caller reports why, from selection's own
		// diagnostic; a second, vaguer warning here would only bury it.
		return "", nil, nil
	case 1:
		return reach[0].ID, nil, nil
	}

	if req.Cwd == "" {
		// Nothing to probe: the cwd is the executor's root wherever the child
		// lands, so "where does my cwd exist" has no answer and a probe would
		// only leak one root workspace per machine to learn nothing.
		return "", []string{noHomeWarning}, nil
	}
	return c.probeHomeExecutor(ctx, req, be, reach)
}

// probeHomeExecutor is rule 5: let the machines themselves answer "is this
// child's cwd here". Each candidate gets a workspace provisioned AS a home
// workspace (useCwd=true), so the winner needs no second provision.
//
// Exactly one hit becomes home. Several hits mean the cwd exists on more than
// one machine and the daemon has no honest way to prefer one — an operator's
// home_executor is that answer, and guessing would put the child's project
// instructions on an arbitrary machine. Zero hits is either the cwd existing
// nowhere (the refusal an eager bind has always produced) or machines that
// could not answer (a warning, because a flaky probe is not evidence the cwd is
// wrong).
func (c *Controller) probeHomeExecutor(ctx context.Context, req protocol.SpawnRequest, be *boundExecutor, reach []executors.Executor) (string, []string, error) {
	ctx10s, cancel := context.WithTimeout(ctx, homeProbeTimeout)
	defer cancel()

	type hit struct {
		execID, wsID string
		cl           tools.ExecutorClient
	}
	var (
		hits []hit
		// failures keeps each candidate's error so the missing-workdir refusal
		// can carry the executor's own message.
		failures []string
		firstErr error
		firstID  string
		// missingCwd stays true only while EVERY failure is a refused workdir;
		// anything else means the machines could not answer.
		missingCwd = true
	)

	for _, e := range reach {
		if isSandboxRow(e) {
			// A sandbox provisions at ITS OWN workdir (its container root), not
			// the requested cwd, so a probe there can never answer "does this
			// child's cwd exist here" — and it always succeeds, which would turn
			// one clean hit on a native executor into a false ambiguity. It is
			// also never an implicit home choice: `executor` must name it.
			continue
		}
		wsID, _, cl, perr := be.binder.ProvisionOn(ctx10s, e.ID, true)
		if perr == nil && wsID == "" {
			// A provision that reports no workspace did not make one; treating
			// it as a hit would adopt a binding with nothing behind it.
			perr = errors.New("provision returned no workspace")
		}
		if perr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", be.binder.DisplayName(e.ID), perr))
			if firstErr == nil {
				firstErr, firstID = perr, e.ID
			}
			if connect.CodeOf(perr) != connect.CodeInvalidArgument {
				missingCwd = false
			}
			continue
		}
		hits = append(hits, hit{execID: e.ID, wsID: wsID, cl: cl})
	}

	switch len(hits) {
	case 1:
		if aerr := be.adopt(hits[0].execID, hits[0].wsID, hits[0].cl); aerr != nil {
			return "", nil, aerr
		}
		return hits[0].execID, nil, nil
	case 0:
		if firstErr != nil && missingCwd {
			// The same refusal a plain eager bind surfaces: the child's cwd
			// exists on none of the machines it may use.
			return "", nil, provisionRefusal(firstID, firstErr)
		}
		if len(failures) > 0 {
			return "", []string{"could not check cwd on: " + strings.Join(failures, ", ")}, nil
		}
		// Unreachable while len(reach) >= 2, kept as the honest fallback.
		return "", []string{noHomeWarning}, nil
	}

	// Several machines hold the child's cwd. Release what the probe made — none
	// of it is a binding — and ask the operator to choose.
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, be.binder.DisplayName(h.execID))
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		be.binder.ReleaseOn(rctx, h.execID, h.wsID)
		rcancel()
	}
	return "", []string{fmt.Sprintf(
		"cwd %s exists on %s; pass home_executor to choose where project instructions come from",
		req.Cwd, strings.Join(names, ", "))}, nil
}

// provisionRefusal is the error a spawn has always surfaced when a provision is
// refused: boundExecutor.provision's wrap of the executor's own message (see
// executor_binding.go). The home probe reuses that exact shape rather than
// inventing a new one, so a cwd that exists nowhere refuses with the text an
// eager bind produced.
//
// The eager home bind refuses with the SAME text without calling this: its error
// comes out of boundExecutor.provision, which already applies this wrap, so
// bindHome returns it unchanged rather than wrapping it twice.
func provisionRefusal(execID string, err error) error {
	return fmt.Errorf("workspace on executor %s: %w", shortID(execID), err)
}

// persistedHome is the home executor a child's OWN record carries: the
// rafiki/home-executor label, or — for a child that predates that label, or one
// that started unbound and bound lazily — rafiki/executor, which NoteBinding
// writes only for the HOME binding.
//
// It is what tells a RESUMED home from a caller's declaration when a request
// arrives with HomeExecutor set, and it is the durable answer a resume must
// reproduce. The binder's Home is the same first label; this reads the record
// directly because the fallback label is not part of the binder's contract.
func (c *Controller) persistedHome(childID string) string {
	if c == nil || c.st == nil || childID == "" {
		return ""
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return ""
	}
	return persistedHomeLabel(snap.Labels)
}

// persistedHomeLabel is the one reading of a child's own labels for its home:
// rafiki/home-executor, else rafiki/executor. The resume path and the resolver's
// persisted-home check share it, so the value a resume CARRIES cannot disagree
// with the value that tells the resolver it is persisted.
func persistedHomeLabel(labels map[string]string) string {
	if home := labels[homeExecutorLabel]; home != "" {
		return home
	}
	return labels["rafiki/executor"]
}

// bindHome provisions the child's HOME workspace at spawn and decides what a
// failure MEANS. It is the one place the three outcomes are told apart:
//
//   - nil: the home is bound, and the project tier can be read there.
//   - a CodeInvalidArgument refusal (the executor cannot see the child's cwd):
//     returned as refuseErr, and the spawn must be REFUSED with it. This is the
//     one class of bind failure that does not heal, and it is exactly what the
//     multi-executor probe refuses with — a cwd the home cannot see would leave
//     a running child failing every workspace call on a path its prompt names.
//   - anything else (transport, unavailable, the executor departing between
//     resolution and the bind): homeBound=false with the error as cause, so the
//     child starts unbound and the not-connected warning carries the reason.
//
// The refusal text is byte-identical to the probe's: boundExecutor.provision
// already wraps the executor's message with provisionRefusal's exact format, so
// it is returned unchanged rather than wrapped again.
func (c *Controller) bindHome(ctx context.Context, be *boundExecutor, homeID string) (homeBound bool, cause, refuseErr error) {
	_, err := be.clientFor(ctx, homeID)
	switch {
	case err == nil:
		return true, nil, nil
	case connect.CodeOf(err) == connect.CodeInvalidArgument:
		return false, nil, err
	default:
		return false, err, nil
	}
}

// adopt installs a workspace the probe already made as the child's HOME
// binding, and records the home.
//
// The field write goes through publishWorkspace — the ONE guard every
// workspace publication passes — rather than being assigned here: adopt
// publishes a placeholder under b.mu, then hands it to that guard, which
// re-checks b.closed and that the placeholder is still this executor's binding
// and RELEASES the workspace itself when either is false. A second publish path
// is exactly how a workspace lands after the exit sweep with nothing left to
// release it.
//
// The labels are written before the binding becomes usable (ready closes last),
// the same ordering provision uses: teardown finds the home workspace by label,
// so it must never be usable before it is findable — and, through the same
// helper, an exit that lands in that gap removes them again rather than
// leaving them naming a workspace the sweep already released.
//
// Publish and label are one step under the home-transition slot, like every other
// home-label write: the slot is uncontested at spawn (nothing else can reach a
// fresh boundExecutor), but taking it here means no home binding is ever labelled
// outside it, so there is no second ordering to reason about. Like provision, it
// does not WAIT for the slot — it has already published the placeholder another
// caller may be waiting on (homeWaitNone).
func (b *boundExecutor) adopt(execID, wsID string, cl tools.ExecutorClient) error {
	if execID == "" || wsID == "" {
		return errors.New("adopt: a home binding needs an executor id and a workspace id")
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.releaseAsync(context.Background(), execID, wsID)
		return errChildExited
	}
	if cur := b.bindings[execID]; cur != nil {
		// Another caller already owns this executor — the probe ran while
		// something else was binding. Its workspace wins, and the probed one is
		// redundant. (At spawn this is unreachable: a fresh boundExecutor has no
		// bindings and nothing else runs before the child starts.)
		b.mu.Unlock()
		b.releaseAsync(context.Background(), execID, wsID)
		return nil
	}
	bd := &binding{ready: make(chan struct{})}
	b.bindings[execID] = bd
	b.mu.Unlock()

	perr := b.withHomeTransition(context.Background(), homeWaitNone, func(held bool) error {
		if perr := b.publishWorkspace(context.Background(), bd, execID, wsID, cl, true); perr != nil {
			return perr
		}
		b.noteHomeBinding(bd, execID, wsID, held)
		return nil
	})
	if perr != nil {
		b.failFirstProvision(bd, execID, perr)
		return perr
	}

	b.setHome(execID)
	close(bd.ready)
	return nil
}

// noteHomeExecutor records the child's home executor at spawn, where the
// child's record may not exist yet.
//
// It goes through the same store-then-stash path NoteBinding and markUnbound
// use, and it NEVER rewrites a different home: the persisted label is the
// authority a resumed child comes back to, so a resolver that changed its mind
// (or a second resolution racing the first) must not move the child's home on
// disk behind its back.
func (c *Controller) noteHomeExecutor(childID, execID string) {
	if execID == "" {
		return
	}
	if snap, ok := c.st.Get(childID); ok {
		if cur := snap.Labels[homeExecutorLabel]; cur != "" && cur != execID {
			return
		}
	}
	_, err := c.st.SetLabels(childID, map[string]string{homeExecutorLabel: execID}, nil)
	switch {
	case err == nil:
		return
	case !errors.Is(err, childstore.ErrNotFound):
		slog.Warn("could not record the child's home executor",
			"child", childID, "executor", shortID(execID), "error", err)
		return
	}

	// The child record does not exist yet. Stash for Spawn.
	c.wsLabelsMu.Lock()
	defer c.wsLabelsMu.Unlock()
	if c.wsLabels == nil {
		c.wsLabels = make(map[string]workspaceLabels)
	}
	wl := c.wsLabels[childID]
	if wl.homeExecutorID == "" || wl.homeExecutorID == execID {
		wl.homeExecutorID = execID
	}
	c.wsLabels[childID] = wl
}

// homeInfo is everything the spawn path reads from the child's HOME
// binding: the project tier and project skills (they live in the child's cwd,
// so they exist on exactly one machine), the tools that machine serves, and the
// "Your machine" block.
//
// Nothing here is ever fetched from a non-home executor: a project file read
// from the wrong machine is worse than no project file, because the model then
// believes instructions apply that were written for somebody else's tree.
//
// homeBound says the home's workspace already exists. When it does not (the
// machine left between resolution and the bind, or the bind failed for a reason
// that may heal) the project tier is empty and the caller is told why — never
// silently replaced by another machine's. bindErr, when set, is the bind
// failure's own reason, carried in the warning so the diagnostic is not lost.
func (c *Controller) homeInfo(ctx context.Context, req protocol.SpawnRequest, owner executorOwner, be *boundExecutor, homeID string, homeBound bool, bindErr error, reach []executors.Executor) spawnHomeInfo {
	var info spawnHomeInfo
	switch {
	case homeID != "" && !homeBound:
		w := fmt.Sprintf(homeNotConnectedWarning, be.binder.DisplayName(homeID))
		if bindErr != nil {
			w += fmt.Sprintf(" (bind error: %v)", bindErr)
		}
		info.Warnings = append(info.Warnings, w)
	case homeID != "":
		project, pcErr := fetchProjectContext(ctx, be)
		if pcErr != nil {
			slog.Warn("project context unavailable; agent gets global instructions only",
				"child", be.childID, "executor", shortID(homeID), "error", pcErr)
			info.Warnings = append(info.Warnings, fmt.Sprintf(homeNotConnectedWarning, be.binder.DisplayName(homeID)))
		} else {
			info.Project = project
		}
		remote, psErr := fetchProjectSkills(ctx, be)
		if psErr != nil {
			slog.Warn("project skills unavailable", "child", be.childID, "executor", shortID(homeID), "error", psErr)
		}
		info.ProjectSkills = remote
		info.ExecutorTools = c.executorToolsFor(homeID)
	}

	// The "Your machine" block comes from the executor's ROW, never from the
	// executor's own account of itself.
	//
	// A request that CARRIED a home never falls back to another machine: an
	// unreachable persisted home must leave the child with no block, not a block
	// naming the one executor that happened to still be there — that machine is
	// not where its cwd and CLAUDE.md came from, and the prompt would repeat the
	// lie for the child's whole life.
	//
	// With no carried home, the first reachable candidate is kept only when
	// there is exactly one. A fresh spawn cannot reach that branch (the resolver
	// names a home for every single-reachable set), so it is a guard for a
	// resolver that one day returns no home there.
	var row executors.Executor
	switch {
	case homeID != "":
		row, _ = c.executorRow(homeID)
	case req.HomeExecutor == "" && len(reach) == 1:
		if chosen, cErr := c.chooseExecutorCandidate(req, owner); cErr == nil {
			row = chosen
		}
	}
	if row.ID != "" {
		wsInfo := workspaceInfoFromRow(row)
		wsInfo.ExecutorName = row.Labels["machine"]
		info.Workspace = wsInfo
	}
	return info
}

// spawnHomeInfo is homeInfo's result.
type spawnHomeInfo struct {
	Project       string
	ProjectSkills []skills.SkillMeta
	ExecutorTools []string
	Workspace     *fundi.WorkspaceInfo
	Warnings      []string
}

// unboundWarnings explains a spawn that starts with no bound workspace. With an
// empty reach — or one that could not be read at all — the diagnostic is
// selection's own per-candidate report, which is also what the child's
// workspace tools will fail with until an executor connects. With a non-empty
// reach the resolver has already said why it named no home, and a second
// warning would only repeat it in vaguer words.
func (c *Controller) unboundWarnings(be *boundExecutor, reach []executors.Executor, reachErr error) []string {
	if len(reach) > 0 && reachErr == nil {
		return nil
	}
	diag := ""
	switch {
	case reachErr != nil:
		diag = reachErr.Error()
	default:
		if _, cerr := be.binder.ChooseFor(be.childID); cerr != nil {
			diag = cerr.Error()
		}
	}
	if diag == "" {
		return []string{noHomeWarning}
	}
	return []string{unboundWarningPrefix + diag}
}
