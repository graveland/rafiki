// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
)

// gitPymoduleInventory is the latest refresh's reported snapshot for one
// (owner, name): what the executor discovered in the checkout, and whether
// the repo's one shared venv built. Scripts and packages stay as the
// executor-protocol messages the daemon received them as — the daemon never
// touches git and never re-reads the checkout, it only relays and caches
// what executors report.
type gitPymoduleInventory struct {
	Scripts   []*executorpb.GitSourceScript
	Packages  []*executorpb.GitSourcePackage
	VenvReady bool
	VenvError string
}

// gitPymodulePusher fans a git source's refresh out to every one of that
// owner's own eligible executors and caches the latest reported inventory.
// It is the per-source counterpart to pymodulePusher's whole-corpus pushAll:
// unlike blob syncs there is no version to stamp and no prune to compute —
// a git source's own history is already its versioning mechanism — so the
// pusher's job is exactly fan-out, caching and disagreement reporting.
//
// Owner scoping is the same as pymodulePusher's: an executor's
// Labels["owner"] gates what it may be refreshed for at all, and the label's
// USERNAME is resolved through resolveOwnerID (the same lookup
// resolveUsernameToUserID performs) and must match the refresh's owner.
type gitPymodulePusher struct {
	pool           pymodulePool
	store          gitpymodules.Store
	resolveOwnerID func(ctx context.Context, username string) (string, bool)

	mu    sync.Mutex
	cache map[string]gitPymoduleInventory // keyed by gitSourceKey(ownerUserID, name)
}

// gitSourceKey is the cache key encoding: NUL-separated, never appearing in
// either a validated pymodule name or a user id, so no (owner, name) pair
// can collide with another.
func gitSourceKey(ownerUserID, name string) string {
	return ownerUserID + "\x00" + name
}

func newGitPymodulePusher(pool pymodulePool, store gitpymodules.Store, resolveOwnerID func(ctx context.Context, username string) (string, bool)) *gitPymodulePusher {
	return &gitPymodulePusher{
		pool:           pool,
		store:          store,
		resolveOwnerID: resolveOwnerID,
		cache:          make(map[string]gitPymoduleInventory),
	}
}

func (gp *gitPymodulePusher) eligible(le execpool.LiveExecutor) bool {
	if le.Describe == nil || !le.Describe.GetPymoduleGitSync() {
		return false
	}
	return le.Executor.Labels["owner"] != ""
}

// refresh broadcasts a SyncPyModuleGitSource call for (ownerUserID, name) to
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
func (gp *gitPymodulePusher) refresh(ctx context.Context, ownerUserID, name, url, ref string) (gitPymoduleInventory, error) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		first    gitPymoduleInventory
		firstID  string
		firstSet bool
		last     gitPymoduleInventory
		failed   int
	)
	for _, le := range gp.pool.Live() {
		if !gp.eligible(le) {
			continue
		}
		ownerID, ok := gp.resolveOwnerID(ctx, le.Executor.Labels["owner"])
		if !ok || ownerID != ownerUserID {
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
				slog.Warn("git pymodule source refresh failed", "executor", executorID, "source", name, "error", err)
				return
			}
			if firstSet && gitInventoryDiffers(first, inv) {
				slog.Warn("git pymodule source inventory disagrees between executors",
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
				gp.cache = make(map[string]gitPymoduleInventory)
			}
			gp.cache[gitSourceKey(ownerUserID, name)] = inv
			gp.mu.Unlock()
		}(le.Executor.ID)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !firstSet {
		if failed > 0 {
			return gitPymoduleInventory{}, fmt.Errorf("refresh %q: every eligible executor failed (%d)", name, failed)
		}
		return gitPymoduleInventory{}, errors.New("no eligible executor for this owner's git sources")
	}
	return last, nil
}

// gitInventoryDiffers reports whether two reported inventories name
// different scripts or packages. Only the NAMES matter — descriptions ride
// the same files, so they cannot disagree while the names agree.
func gitInventoryDiffers(a, b gitPymoduleInventory) bool {
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
func (gp *gitPymodulePusher) pushTo(ctx context.Context, executorID, name, url, ref string) (gitPymoduleInventory, error) {
	rctx, cancel := context.WithTimeout(ctx, pymoduleSyncTimeout())
	defer cancel()

	client, err := gp.pool.ConnectClientFor(executorID)
	if err != nil {
		return gitPymoduleInventory{}, err
	}
	resp, err := client.SyncPyModuleGitSource(rctx, connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{
		Name: name, Url: url, Ref: ref,
	}))
	if err != nil {
		return gitPymoduleInventory{}, err
	}
	slog.Info("git pymodule source refreshed",
		"executor", executorID, "source", name,
		"scripts", len(resp.Msg.GetScripts()), "packages", len(resp.Msg.GetPackages()),
		"venv_ready", resp.Msg.GetVenvReady())
	return gitPymoduleInventory{
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
func (gp *gitPymodulePusher) evict(ownerUserID, name string) {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	delete(gp.cache, gitSourceKey(ownerUserID, name))
}

func (gp *gitPymodulePusher) inventoryFor(ownerUserID, name string) (gitPymoduleInventory, bool) {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	inv, ok := gp.cache[gitSourceKey(ownerUserID, name)]
	return inv, ok
}

// allInventory returns every cached (name, inventory) pair for ownerUserID,
// for the "list everything" discovery path (the dynamic python-modules skill
// body and MCP's pymodule_list). Only that owner's entries — the cache is
// per-owner, so no other owner's names can leak through the prefix filter.
func (gp *gitPymodulePusher) allInventory(ownerUserID string) map[string]gitPymoduleInventory {
	gp.mu.Lock()
	defer gp.mu.Unlock()
	out := make(map[string]gitPymoduleInventory)
	prefix := ownerUserID + "\x00"
	for key, inv := range gp.cache {
		if name, ok := strings.CutPrefix(key, prefix); ok {
			out[name] = inv
		}
	}
	return out
}

// recordFor resolves one registered source's stored url/ref for the owner —
// the RefreshGitSource path, which carries only the name and must re-send
// the source's CURRENT registration, not whatever the caller remembers.
// (The store has no Get; List is the only read it exposes.)
func (gp *gitPymodulePusher) recordFor(ctx context.Context, ownerUserID, name string) (gitpymodules.GitSourceRecord, error) {
	recs, err := gp.store.List(ctx, ownerUserID)
	if err != nil {
		return gitpymodules.GitSourceRecord{}, err
	}
	for _, r := range recs {
		if r.Name == name {
			return r, nil
		}
	}
	return gitpymodules.GitSourceRecord{}, gitpymodules.ErrNotFound
}
