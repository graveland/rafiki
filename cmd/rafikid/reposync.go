// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/repos"
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

	mu    sync.Mutex
	cache map[string]repoInventory // keyed by repoKey(ownerUserID, name)
}

// repoKey is the cache key encoding: NUL-separated, never appearing in
// either a validated pymodule name or a user id, so no (owner, name) pair
// can collide with another.
func repoKey(ownerUserID, name string) string {
	return ownerUserID + "\x00" + name
}

func newRepoPusher(pool pymodulePool, store repos.Store, resolveOwnerID func(ctx context.Context, username string) (id string, found bool, err error), osUser string) *repoPusher {
	return &repoPusher{
		pool:           pool,
		store:          store,
		resolveOwnerID: resolveOwnerID,
		osUser:         osUser,
		cache:          make(map[string]repoInventory),
	}
}

func (gp *repoPusher) eligible(le execpool.LiveExecutor) bool {
	if le.Describe == nil || !le.Describe.GetRepoSync() {
		return false
	}
	return le.Executor.Labels["owner"] != ""
}

// refresh broadcasts a SyncRepo call for (ownerUserID, name) to
// every one of that owner's eligible LIVE executors, in parallel (an owner
// with several executors shouldn't pay for them serially — each executor
// clones, discovers and venv-builds independently), and caches the LAST one
// to respond as the inventory snapshot for (owner, name). Executors that
// disagree on the discovered scripts/packages after refreshing the same ref
// are logged, not reconciled — no consensus logic, per design §3.
//
// An executor that fails (dial error, clone failure, timeout) contributes
// nothing and leaves any prior snapshot in place; the returned error is nil
// whenever at least one executor responded, because the cache is populated
// by definition in that case.
//
// When nothing was eligible and nothing failed, the error names what the
// pool looked like — every live executor with its owner label and repo-sync
// flag — so an operator can see why their executor was passed over without
// grepping the daemon log.
func (gp *repoPusher) refresh(ctx context.Context, ownerUserID, name, url, ref string) (repoInventory, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		first    repoInventory
		firstID  string
		firstSet bool
		last     repoInventory
		failed   int
		live     = gp.pool.Live()
		summary  = make([]string, 0, len(live)) // built on this goroutine only; read after wg.Wait
	)
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
		wg.Add(1)
		go func(executorID string) {
			defer wg.Done()
			inv, err := gp.pushTo(ctx, executorID, name, url, ref)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				slog.Warn("repo refresh failed", "executor", executorID, "source", name, "error", err)
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
			gp.mu.Lock()
			if gp.cache == nil {
				gp.cache = make(map[string]repoInventory)
			}
			gp.cache[repoKey(ownerUserID, name)] = inv
			gp.mu.Unlock()
		}(le.Executor.ID)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !firstSet {
		if failed > 0 {
			return repoInventory{}, fmt.Errorf("refresh %q: every eligible executor failed (%d)", name, failed)
		}
		return repoInventory{}, fmt.Errorf("no eligible executor for owner %q's repos: %d live executor(s): %s",
			repoOwnerDisplay(ownerUserID), len(live), strings.Join(summary, "; "))
	}
	return last, nil
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

// pushTo refreshes ONE named source on ONE executor and returns the
// discovery report. Same generous, env-overridable timeout as the blob-sync
// push: a clone plus a venv build can legitimately take minutes.
func (gp *repoPusher) pushTo(ctx context.Context, executorID, name, url, ref string) (repoInventory, error) {
	rctx, cancel := context.WithTimeout(ctx, pymoduleSyncTimeout())
	defer cancel()

	client, err := gp.pool.ConnectClientFor(executorID)
	if err != nil {
		return repoInventory{}, err
	}
	resp, err := client.SyncRepo(rctx, connect.NewRequest(&executorpb.SyncRepoRequest{
		Name: name, Url: url, Ref: ref,
	}))
	if err != nil {
		return repoInventory{}, err
	}
	slog.Info("repo refreshed",
		"executor", executorID, "source", name,
		"scripts", len(resp.Msg.GetScripts()), "packages", len(resp.Msg.GetPackages()),
		"venv_ready", resp.Msg.GetVenvReady())
	return repoInventory{
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
