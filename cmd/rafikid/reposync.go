// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/repos"
	"go.graveland.dev/rafiki/pkg/skills"
)

// repoInventory is the latest refresh's reported snapshot for one
// (owner, name): what the executor discovered in the checkout, and whether
// the repo's one shared venv built. Scripts and packages stay as the
// executor-protocol messages the daemon received them as — the daemon never
// touches git and never re-reads the checkout, it only relays and caches
// what executors report.
type repoInventory struct {
	Scripts   []*executorpb.RepoScript
	Packages  []*executorpb.RepoPackage
	VenvReady bool
	VenvError string
}

// RefreshResult is one refresh's whole answer: what commit it fanned out,
// whether that commit was already the stored one, the discovered inventory and
// venv state, and what happened to the skills half.
//
// SkillErrors is the skills half's failure, never the refresh's: a bad skill
// is reported per skill and the scripts still refresh. The one exception is a
// NAMESPACE COLLISION during registration, which the caller detects through
// NamespaceCollision and treats as a failed registration.
type RefreshResult struct {
	Commit      string
	Unchanged   bool // Commit equalled the stored one
	Scripts     []*executorpb.RepoScript
	Packages    []*executorpb.RepoPackage
	VenvReady   bool
	VenvError   string
	Skills      int
	SkillErrors []string

	// namespaceHeld records that one of SkillErrors came from a
	// *skills.NamespaceHeldError, i.e. another source already holds one of the
	// repo's namespaces in this layer. The texts are the caller-facing answer;
	// this is how the daemon's own AddRepo knows which kind of skill error must
	// fail the registration instead of merely being reported.
	namespaceHeld bool
}

// NamespaceCollision reports whether the skills half was refused because
// another source already holds one of this repo's namespaces in this layer.
func (r RefreshResult) NamespaceCollision() bool { return r.namespaceHeld }

// errRepoRemoved reports that a refresh's registration disappeared while the
// refresh was in flight: a `repo remove` (or a concurrent add that rolled back)
// landed mid-clone, so this refresh has nothing to record against. It wraps
// repos.ErrNotFound so the Connect face answers NotFound rather than Internal.
var errRepoRemoved = fmt.Errorf("the repo was removed while the refresh was in flight; nothing was stored: %w", repos.ErrNotFound)

// repoSource is the skill-set source marker a repo owns. Ownership is the
// marker: a repo writes and deletes only its own repo:<name> rows for its
// owner, so two repos can never overwrite each other's skills.
func repoSource(name string) string { return "repo:" + name }

// repoCall is one in-flight refresh, shared with every caller that arrived
// while it ran: they wait on done and take the same result, so one (owner,
// name) is resolved and fanned out once no matter how many callers ask.
type repoCall struct {
	done   chan struct{}
	result RefreshResult
	err    error
	// leaderCtx is the context the LEADER ran under. A waiter that sees it
	// cancelled knows the outcome says nothing about its own live request, and
	// takes over rather than inheriting a cancellation it never asked for.
	leaderCtx context.Context
	waiters   int // callers blocked on done, for the coalescing tests to observe
}

// repoPusher fans a repo's refresh out to every one of that
// owner's own eligible executors and caches the latest reported inventory.
// It is the per-source counterpart to pymodulePusher's whole-corpus pushAll:
// unlike blob syncs there is no version to stamp and no prune to compute —
// a repo's own history is already its versioning mechanism — so the
// pusher's job is exactly fan-out, caching and disagreement reporting.
//
// Owner scoping: an executor's Labels["owner"] gates what it may be
// refreshed for at all, and the label's USERNAME is resolved through
// resolveOwnerID (Controller.resolveUsernameToUserID). A label that names an
// active user must match the refresh's owner, exactly as pymodulePusher
// scopes its blob pushes. A label that names NO user makes the executor
// UNATTRIBUTED (owner "") ONLY when that label IS the daemon's OS user (osUser
// — the same fallback sessionOwner and attestOwner stamp): that is exactly
// the anonymous-enrolled set, and it is what an anonymous registration syncs
// to. Any OTHER unresolvable label — a since-removed user's executor, whose
// session was never evicted — is skipped with a warn: handing that machine
// anonymous sources (which may embed credentials) would widen the rule to a
// principal the operator deliberately removed. A resolver ERROR is also not a
// miss: the executor is skipped and logged, because a store outage must never
// be read as unattributed and hand anonymous sources to a user's executor.
type repoPusher struct {
	pool           pymodulePool
	store          repos.Store
	resolveOwnerID func(ctx context.Context, username string) (id string, found bool, err error)
	// osUser is the daemon's own OS username — the only unresolvable label
	// that maps to the unattributed owner. Empty when osUser() failed, which
	// disables the unattributed rule entirely (no label equals "").
	osUser string

	// skillStore holds the skills a repo ships, under the repo's own
	// repo:<name> source marker. Nil means skills are discovered and reported
	// but not persisted (a daemon with no skill store).
	skillStore skills.Store
	// skillPush delivers the corpus to the executors after a write that
	// CHANGED something — the existing on-write push path, so a refresh that
	// re-derives an identical set stays free. Nil disables the push.
	skillPush func(ctx context.Context)

	mu    sync.Mutex
	cache map[string]repoInventory // keyed by repoKey(ownerUserID, name)
	// inflight holds the refresh currently running per REGISTRATION: a second
	// caller for the same (owner, name, url, ref) waits for that one's result
	// rather than resolving the ref and cloning again. The url and ref are part
	// of the key because a repointed source is a different registration: after
	// `repo set --ref new`, a refresh of the NEW ref must not join an in-flight
	// refresh of the old one and inherit its commit.
	inflight map[repoRefreshKey]*repoCall
}

// repoKey is the cache key encoding: NUL-separated, never appearing in
// either a validated pymodule name or a user id, so no (owner, name) pair
// can collide with another.
func repoKey(ownerUserID, name string) string {
	return ownerUserID + "\x00" + name
}

// repoRefreshKey identifies one in-flight refresh: the whole registration, not
// just its name. A struct rather than a joined string, so no value of any field
// can forge a different key by embedding the separator.
type repoRefreshKey struct {
	ownerUserID, name, url, ref string
}

func newRepoPusher(pool pymodulePool, store repos.Store, resolveOwnerID func(ctx context.Context, username string) (id string, found bool, err error), osUser string) *repoPusher {
	return &repoPusher{
		pool:           pool,
		store:          store,
		resolveOwnerID: resolveOwnerID,
		osUser:         osUser,
		cache:          make(map[string]repoInventory),
		inflight:       make(map[repoRefreshKey]*repoCall),
	}
}

func (gp *repoPusher) eligible(le execpool.LiveExecutor) bool {
	if le.Describe == nil || !le.Describe.GetRepoSync() {
		return false
	}
	return le.Executor.Labels["owner"] != ""
}

// refresh refreshes (ownerUserID, name) on every one of that owner's eligible
// LIVE executors, coalescing concurrent callers for the same registration.
//
// One refresh per (owner, name, url, ref) is in flight at a time: a second
// caller that arrives while one is running waits for that one's result instead
// of resolving the ref and fanning out again. Two callers — an operator's
// `repo refresh` and the scheduler's timer, say — therefore cost one clone, and
// the store is written once. The waiter honours its OWN context: a caller that
// gives up returns ctx.Err() without disturbing the refresh it was waiting on.
//
// A waiter does not inherit a cancellation it never asked for: when the leader
// failed because ITS caller went away (context.Canceled) while this caller's
// context is still live, the waiter becomes the leader and runs the refresh
// itself. Inheriting that error would report a cancellation to a caller who
// never cancelled.
//
// The error is the refresh's error, never a skill's: a bad skill rides
// RefreshResult.SkillErrors so the scripts half still succeeds.
func (gp *repoPusher) refresh(ctx context.Context, ownerUserID, name, url, ref string) (RefreshResult, error) {
	key := repoRefreshKey{ownerUserID: ownerUserID, name: name, url: url, ref: ref}
	for {
		gp.mu.Lock()
		if gp.inflight == nil {
			gp.inflight = make(map[repoRefreshKey]*repoCall)
		}
		call, running := gp.inflight[key]
		if !running {
			call = &repoCall{done: make(chan struct{}), leaderCtx: ctx}
			gp.inflight[key] = call
			gp.mu.Unlock()
			return gp.lead(ctx, key, call, ownerUserID, name, url, ref)
		}
		call.waiters++
		gp.mu.Unlock()

		select {
		case <-call.done:
			if call.err != nil && call.leaderCtx.Err() != nil && ctx.Err() == nil {
				// The leader failed while its own caller had already given up
				// (cancelled or timed out), so that failure is not an answer
				// for anyone else: this caller is still waiting for one and
				// takes over. A leader that SUCCEEDED is an answer whatever its
				// caller did afterwards.
				continue
			}
			return call.result, call.err
		case <-ctx.Done():
			return RefreshResult{}, ctx.Err()
		}
	}
}

// lead runs one refresh and publishes its outcome to every waiter, then frees
// the slot. The cleanup is DEFERRED so it happens on every exit — a panic in
// refreshOnce would otherwise leave the registration's slot occupied forever,
// and every later refresh of it would block on a channel nobody will close.
func (gp *repoPusher) lead(ctx context.Context, key repoRefreshKey, call *repoCall, ownerUserID, name, url, ref string) (result RefreshResult, err error) {
	defer func() {
		gp.mu.Lock()
		delete(gp.inflight, key)
		call.result, call.err = result, err
		gp.mu.Unlock()
		close(call.done)
	}()
	return gp.refreshOnce(ctx, ownerUserID, name, url, ref)
}

// refreshOnce is refresh's body, run once per in-flight call.
//
// It resolves the ref to a commit ONCE — through the first eligible executor,
// in ID order, that answers — and fans that SHA out to all of them. Git runs
// only on executors, with their owner's own credentials, so the daemon asks
// rather than resolving; resolving once is what keeps two executors from
// landing different commits for one refresh.
//
// The SyncRepo calls then run in parallel (an owner with several executors
// shouldn't pay for them serially — each executor fetches, discovers and
// venv-builds independently), and the LAST one to respond becomes the cached
// inventory snapshot for (owner, name). Executors that disagree on the
// discovered scripts/packages after refreshing the same commit are logged, not
// reconciled — no consensus logic, per design §3.
//
// An executor that fails (dial error, checkout failure, timeout) contributes
// nothing and leaves any prior snapshot in place; the returned error is nil
// whenever at least one executor responded, because the cache is populated
// by definition in that case.
//
// When nothing was eligible and nothing failed, the error names what the
// pool looked like — every live executor with its owner label and repo-sync
// flag — so an operator can see why their executor was passed over without
// grepping the daemon log.
//
// After the fan-out the FIRST successful response in executor-ID order decides
// the skills half: its discovered skills are stored as the repo's
// repo:<name> set (unless it reported skill errors, in which case nothing is
// written and the errors are returned), and the resolved commit is recorded.
// ID order rather than arrival order makes which executor's discovery wins
// deterministic.
func (gp *repoPusher) refreshOnce(ctx context.Context, ownerUserID, name, url, ref string) (RefreshResult, error) {
	// The stored commit is what makes "unchanged" mean "the ref did not move
	// since the last refresh", and whether a row existed at all is what tells
	// the end-of-refresh recheck below whether a missing row means "removed
	// while this ran" (report it) or "never registered" (write nothing, no
	// error — a refresh of a name nobody wrote is not a failure).
	prior, priorErr := gp.recordFor(ctx, ownerUserID, name)
	if priorErr != nil && !errors.Is(priorErr, repos.ErrNotFound) {
		return RefreshResult{}, priorErr
	}
	registered := priorErr == nil

	live := gp.pool.Live()
	summary := make([]string, 0, len(live)) // built on this goroutine only; read after wg.Wait
	targets := make([]string, 0, len(live))
	for _, le := range live {
		summary = append(summary, repoExecutorSummary(le))
		if !gp.eligible(le) {
			continue
		}
		ownerID, found, err := gp.resolveOwnerID(ctx, le.Executor.Labels["owner"])
		if err != nil {
			// A resolver error is a store outage, not a miss: skip the
			// executor rather than read it as unattributed — an outage must
			// never hand anonymous sources to a user's executor.
			slog.Warn("repo refresh: owner lookup failed; skipping executor",
				"executor", le.Executor.ID, "owner", le.Executor.Labels["owner"], "error", err)
			continue
		}
		if !found {
			if label := le.Executor.Labels["owner"]; label == gp.osUser {
				// The label IS the daemon's OS user but resolves to no row: an
				// anonymous-enrolled executor, the unattributed owner.
				ownerID = ""
			} else {
				slog.Warn("repo refresh: owner label does not resolve to an active user and is not the daemon's OS user; skipping executor",
					"executor", le.Executor.ID, "owner", label)
				continue
			}
		}
		if ownerID != ownerUserID {
			continue
		}
		targets = append(targets, le.Executor.ID)
	}
	if len(targets) == 0 {
		return RefreshResult{}, fmt.Errorf("no eligible executor for owner %q's repos: %d live executor(s): %s",
			repoOwnerDisplay(ownerUserID), len(live), strings.Join(summary, "; "))
	}
	// ID order, so which executor resolves is deterministic rather than
	// whatever the pool happened to enumerate first.
	slices.Sort(targets)

	commit, err := gp.resolveCommit(ctx, targets, name, url, ref)
	if err != nil {
		return RefreshResult{}, err
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		first     repoInventory
		firstID   string
		firstSet  bool
		last      repoInventory
		failed    int
		responses = make(map[string]*executorpb.SyncRepoResponse, len(targets))
	)
	for _, executorID := range targets {
		wg.Add(1)
		go func(executorID string) {
			defer wg.Done()
			resp, inv, err := gp.pushTo(ctx, executorID, name, url, ref, commit)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				slog.Warn("repo refresh failed", "executor", executorID, "source", name, "commit", commit, "error", err)
				return
			}
			if firstSet && repoInventoryDiffers(first, inv) {
				slog.Warn("repo inventory disagrees between executors",
					"source", name, "executorA", firstID, "executorB", executorID,
					"executorA_scripts", len(first.Scripts), "executorB_scripts", len(inv.Scripts),
					"executorA_packages", len(first.Packages), "executorB_packages", len(inv.Packages))
			}
			if !firstSet {
				first, firstID, firstSet = inv, executorID, true
			}
			last = inv
			responses[executorID] = resp
			gp.mu.Lock()
			if gp.cache == nil {
				gp.cache = make(map[string]repoInventory)
			}
			gp.cache[repoKey(ownerUserID, name)] = inv
			gp.mu.Unlock()
		}(executorID)
	}
	wg.Wait()
	mu.Lock()
	// Everything the collection needs, copied out before the lock is released:
	// the skills write below must not run under it.
	if !firstSet {
		mu.Unlock()
		if failed > 0 {
			return RefreshResult{}, fmt.Errorf("refresh %q: every eligible executor failed (%d)", name, failed)
		}
		return RefreshResult{}, fmt.Errorf("no eligible executor for owner %q's repos: %d live executor(s): %s",
			repoOwnerDisplay(ownerUserID), len(live), strings.Join(summary, "; "))
	}
	mu.Unlock()

	result := RefreshResult{
		Commit:    commit,
		Unchanged: prior.Commit == commit,
		Scripts:   last.Scripts,
		Packages:  last.Packages,
		VenvReady: last.VenvReady,
		VenvError: last.VenvError,
	}

	// Re-read the registration before writing ANYTHING. A refresh clones for
	// minutes, and the store is not locked while it does: a `repo remove`, a
	// repoint or a concurrent add that rolled back can all land in that window.
	// Writing without re-checking would undo them — the removed source's
	// repo:<name> skills would reappear, and its commit would be recorded
	// against a row that no longer exists.
	//
	// Two refreshes of DIFFERENT registrations can also run at once (the
	// in-flight key carries url and ref), so the older one finishing last must
	// not overwrite the newer one's skills or commit.
	current, rerr := gp.recordFor(ctx, ownerUserID, name)
	switch {
	case errors.Is(rerr, repos.ErrNotFound):
		if !registered {
			// Never registered at all (the unit fixtures' shape): there is no
			// row to record against and none to lose. Report what was found and
			// write nothing.
			return result, nil
		}
		// Registered when the refresh started, gone now: a removal raced it.
		// Drop the cached inventory the fan-out just repopulated and report it,
		// so the caller can never read this as "stored".
		gp.evict(ownerUserID, name)
		return RefreshResult{}, fmt.Errorf("refresh %q: %w", name, errRepoRemoved)
	case rerr != nil:
		return RefreshResult{}, rerr
	case current.URL != url || current.Ref != ref:
		// Repointed (or re-added at a different url/ref) mid-refresh: this
		// commit and these skills belong to the registration that was replaced.
		// The newer registration's own refresh owns the write.
		slog.Info("repo refresh: the registration changed while the refresh ran; nothing was stored",
			"source", name, "owner", repoOwnerDisplay(ownerUserID), "refreshed_ref", ref, "current_ref", current.Ref)
		return result, nil
	}

	// The skills half: one executor's discovery is the repo's set. Executors
	// that agree on a commit are reading the same tree, so any successful one
	// names the same skills; taking the lowest-ID responder keeps the choice
	// deterministic when they disagree.
	var source *executorpb.SyncRepoResponse
	for _, executorID := range targets {
		if resp, ok := responses[executorID]; ok {
			source = resp
			break
		}
	}
	gp.storeSkills(ctx, ownerUserID, name, source, &result)

	// Record the resolution even when the commit did not move: the timestamp is
	// the scheduler's anchor for the next fire, so an unchanged refresh still
	// resets the clock.
	if err := gp.store.SetResolved(ctx, ownerUserID, name, commit, time.Now()); err != nil {
		return RefreshResult{}, fmt.Errorf("record resolved commit for %q: %w", name, err)
	}
	return result, nil
}

// storeSkills writes the skills half of a refresh and folds what happened into
// result: the count stored, and the errors — per-skill errors the executor
// reported, or the one namespace collision that must fail a registration.
//
// Nothing is written when the executor reported skill errors: a partial
// discovery is not a set to replace the stored one with. A store failure is
// also a skill error rather than the refresh's, so the scripts half survives
// it — and its text is carried out to the caller rather than logged and
// dropped, because a refresh that reported success while its skills silently
// went nowhere is worse than one that says so.
func (gp *repoPusher) storeSkills(ctx context.Context, ownerUserID, name string, source *executorpb.SyncRepoResponse, result *RefreshResult) {
	if source == nil {
		return
	}
	if errs := source.GetSkillErrors(); len(errs) > 0 {
		result.SkillErrors = errs
		return
	}
	records := repoSkillRecords(ownerUserID, name, source.GetSkills())
	if gp.skillStore == nil {
		// No skill store (a daemon without a database): discovery is still
		// reported, it just has nowhere to live.
		result.Skills = len(records)
		return
	}
	changed, err := gp.skillStore.PutSet(ctx, skills.SetKey{OwnerUserID: ownerUserID, Source: repoSource(name)}, records)
	if err != nil {
		var held *skills.NamespaceHeldError
		if errors.As(err, &held) {
			result.namespaceHeld = true
		}
		result.SkillErrors = append(result.SkillErrors, err.Error())
		return
	}
	result.Skills = len(records)
	if changed && gp.skillPush != nil {
		gp.skillPush(ctx)
	}
}

// repoSkillRecords lifts the executor protocol's discovered skills into the
// store's records: the repo's source marker and owner on every row, so the
// write can only ever replace its own set.
func repoSkillRecords(ownerUserID, name string, found []*executorpb.RepoSkill) []skills.Record {
	out := make([]skills.Record, 0, len(found))
	for _, sk := range found {
		rec := skills.Record{
			Namespace:   sk.GetNamespace(),
			Name:        sk.GetName(),
			Description: sk.GetDescription(),
			Body:        sk.GetBody(),
			Source:      repoSource(name),
			OwnerUserID: ownerUserID,
		}
		for _, f := range sk.GetFiles() {
			rec.Files = append(rec.Files, skills.File{Path: f.GetPath(), Mode: f.GetMode(), Content: f.GetContent()})
		}
		out = append(out, rec)
	}
	return out
}

// resolveCommit asks the eligible executors, in ID order, to resolve ref to a
// commit and returns the first answer. An executor that cannot answer — an
// RPC failure, or git's own refusal in the response's error field — is tried
// past; only when every one of them fails does the refresh fail, naming the
// last error so the operator sees the most recent reason.
func (gp *repoPusher) resolveCommit(ctx context.Context, executorIDs []string, name, url, ref string) (string, error) {
	var lastErr error
	for _, executorID := range executorIDs {
		commit, err := gp.resolveOn(ctx, executorID, name, url, ref)
		if err != nil {
			lastErr = err
			slog.Warn("repo refresh: resolve failed; trying the next executor",
				"executor", executorID, "source", name, "error", err)
			continue
		}
		return commit, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no eligible executor")
	}
	return "", fmt.Errorf("resolve %s: %w", name, lastErr)
}

// resolveOn asks ONE executor to resolve (url, ref) to a commit.
func (gp *repoPusher) resolveOn(ctx context.Context, executorID, name, url, ref string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, pymoduleSyncTimeout())
	defer cancel()

	client, err := gp.pool.ConnectClientFor(executorID)
	if err != nil {
		return "", err
	}
	resp, err := client.ResolveRepoRef(rctx, connect.NewRequest(&executorpb.ResolveRepoRefRequest{
		Name: name, Url: url, Ref: ref,
	}))
	if err != nil {
		return "", err
	}
	if msg := resp.Msg.GetError(); msg != "" {
		return "", errors.New(msg)
	}
	commit := resp.Msg.GetCommit()
	if commit == "" {
		return "", errors.New("executor resolved no commit")
	}
	return commit, nil
}

// repoOwnerDisplay renders an owner id for the no-eligible-executor error: the
// unattributed bucket has no id to print, so it gets a name instead.
func repoOwnerDisplay(ownerUserID string) string {
	if ownerUserID == "" {
		return "(unattributed)"
	}
	return ownerUserID
}

// repoExecutorSummary renders one live executor for the no-eligible-executor
// error: its id prefix, the owner label it advertises (a USERNAME, possibly
// one that resolves to no active user — the unattributed case) and whether it
// can sync repos at all.
func repoExecutorSummary(le execpool.LiveExecutor) string {
	return fmt.Sprintf("%s owner=%q repo-sync=%t",
		shortID(le.Executor.ID), le.Executor.Labels["owner"], le.Describe.GetRepoSync())
}

// repoInventoryDiffers reports whether two reported inventories name
// different scripts or packages. Only the NAMES matter — descriptions ride
// the same files, so they cannot disagree while the names agree.
func repoInventoryDiffers(a, b repoInventory) bool {
	return !slicesEqualByName(a.Scripts, b.Scripts) || !slicesEqualByName(a.Packages, b.Packages)
}

func slicesEqualByName[T interface{ GetName() string }](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v.GetName()]++
	}
	for _, v := range b {
		if seen[v.GetName()] == 0 {
			return false
		}
		seen[v.GetName()]--
	}
	return true
}

// pushTo refreshes ONE named source on ONE executor onto the commit the
// daemon resolved, and returns the discovery report. Same generous,
// env-overridable timeout as the blob-sync push: a fetch plus a venv build can
// legitimately take minutes.
//
// A response carrying checkout_error is a FAILURE for this executor: nothing
// changed there, and the daemon must count it rather than cache an inventory
// that describes a checkout the executor never made.
func (gp *repoPusher) pushTo(ctx context.Context, executorID, name, url, ref, commit string) (*executorpb.SyncRepoResponse, repoInventory, error) {
	rctx, cancel := context.WithTimeout(ctx, pymoduleSyncTimeout())
	defer cancel()

	client, err := gp.pool.ConnectClientFor(executorID)
	if err != nil {
		return nil, repoInventory{}, err
	}
	resp, err := client.SyncRepo(rctx, connect.NewRequest(&executorpb.SyncRepoRequest{
		Name: name, Url: url, Ref: ref, Commit: commit,
	}))
	if err != nil {
		return nil, repoInventory{}, err
	}
	if cerr := resp.Msg.GetCheckoutError(); cerr != "" {
		return nil, repoInventory{}, fmt.Errorf("checkout %s: %s", commit, cerr)
	}
	slog.Info("repo refreshed",
		"executor", executorID, "source", name, "commit", resp.Msg.GetCommit(),
		"scripts", len(resp.Msg.GetScripts()), "packages", len(resp.Msg.GetPackages()),
		"skills", len(resp.Msg.GetSkills()),
		"venv_ready", resp.Msg.GetVenvReady())
	return resp.Msg, repoInventory{
		Scripts:   resp.Msg.GetScripts(),
		Packages:  resp.Msg.GetPackages(),
		VenvReady: resp.Msg.GetVenvReady(),
		VenvError: resp.Msg.GetVenvError(),
	}, nil
}

// evict drops the cached inventory for (ownerUserID, name) — the pusher's
// counterpart to a store Delete. Without it the removed source's rows keep
// rendering on every span surface (allInventory for the skill body and MCP
// pymodule_list, inventoryFor for `python list --repo <removed>`) until the
// daemon restarts, because the cache readers never consult the store.
func (gp *repoPusher) evict(ownerUserID, name string) {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	delete(gp.cache, repoKey(ownerUserID, name))
}

func (gp *repoPusher) inventoryFor(ownerUserID, name string) (repoInventory, bool) {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	inv, ok := gp.cache[repoKey(ownerUserID, name)]
	return inv, ok
}

// allInventory returns every cached (name, inventory) pair for ownerUserID,
// for the "list everything" discovery path (the dynamic python-modules skill
// body and MCP's pymodule_list). Only that owner's entries — the cache is
// per-owner, so no other owner's names can leak through the prefix filter.
func (gp *repoPusher) allInventory(ownerUserID string) map[string]repoInventory {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	out := make(map[string]repoInventory)
	prefix := ownerUserID + "\x00"
	for key, inv := range gp.cache {
		if name, ok := strings.CutPrefix(key, prefix); ok {
			out[name] = inv
		}
	}
	return out
}

// recordFor resolves one registered source's stored url/ref for the owner —
// the RefreshRepo path, which carries only the name and must re-send
// the source's CURRENT registration, not whatever the caller remembers.
// (The store has no Get; List is the only read it exposes.)
func (gp *repoPusher) recordFor(ctx context.Context, ownerUserID, name string) (repos.Record, error) {
	recs, err := gp.store.List(ctx, ownerUserID)
	if err != nil {
		return repos.Record{}, err
	}
	for _, r := range recs {
		if r.Name == name {
			return r, nil
		}
	}
	return repos.Record{}, repos.ErrNotFound
}
