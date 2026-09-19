// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// pymodulePool is the slice of *execpool.Pool the pusher needs. An interface,
// the same shape as executorPool in executor_select.go, so the pusher is
// testable against a fake instead of a listener, a database and a dialling
// executor — nothing exported on *execpool.Pool installs a live connection.
// *execpool.Pool satisfies it directly.
type pymodulePool interface {
	Live() []execpool.LiveExecutor
	ConnectClientFor(executorID string) (executorpbconnect.ExecutorServiceClient, error)
}

// pymodulePusher delivers one owner's pymodule corpus to each of that
// owner's own executors. Unlike skillPusher, this is OWNER-SCOPED, not
// whole-corpus: an executor's Labels["owner"] gates what it receives at all,
// because pymodule content is private ("my scripts are mine, not other
// users"), and an executor carrying no owner label is not entitled to
// anyone's.
//
// resolveOwnerID resolves an executor's Labels["owner"] USERNAME to a
// conversations.users id, the same lookup cmd/rafikid's resumeOwnerUserID
// already performs. It returns ("", false) when the executor has no owner
// label or the username does not resolve -- both mean "push nothing to this
// executor."
type pymodulePusher struct {
	pool           pymodulePool
	store          pymodules.Store
	version        string
	resolveOwnerID func(ctx context.Context, username string) (string, bool)
}

func buildPyModules(recs []pymodules.Record, version string) *executorpb.SyncPyModulesRequest {
	mods := make([]*executorpb.SyncPyModule, 0, len(recs))
	for _, r := range recs {
		mods = append(mods, &executorpb.SyncPyModule{Name: r.Name, Code: r.Code})
	}
	slices.SortFunc(mods, func(a, b *executorpb.SyncPyModule) int {
		switch {
		case a.GetName() < b.GetName():
			return -1
		case a.GetName() > b.GetName():
			return 1
		default:
			return 0
		}
	})
	return &executorpb.SyncPyModulesRequest{Version: version, Modules: mods}
}

func (pp *pymodulePusher) eligible(le execpool.LiveExecutor) bool {
	if le.Describe == nil || !le.Describe.GetPymodulesSync() {
		return false
	}
	return le.Executor.Labels["owner"] != ""
}

// pymoduleSyncTimeout is how long one executor's SyncPyModules push may run
// before its context expires. A venv build can legitimately take minutes, so
// the default is generous; RAFIKI_PYMODULE_SYNC_TIMEOUT (a Go duration string,
// e.g. "5m") overrides it when it parses, and anything unset or unparseable
// falls back to the default.
func pymoduleSyncTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("RAFIKI_PYMODULE_SYNC_TIMEOUT")); err == nil {
		return d
	}
	return 5 * time.Minute
}

func (pp *pymodulePusher) pushAll(ctx context.Context) []*executorpb.PyModuleVenvResult {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*executorpb.PyModuleVenvResult
	)
	for _, le := range pp.pool.Live() {
		if !pp.eligible(le) {
			continue
		}
		wg.Add(1)
		go func(executorID string) {
			defer wg.Done()
			res, err := pp.pushTo(ctx, executorID)
			if err != nil {
				slog.Warn("pymodule sync failed", "executor", executorID, "error", err)
				return
			}
			mu.Lock()
			results = append(results, res...)
			mu.Unlock()
		}(le.Executor.ID)
	}
	wg.Wait()
	// Aggregated across executors, not deduplicated: the same module name can
	// fail on two different executors, and the caller renders that.
	return results
}

func (pp *pymodulePusher) pushIfEligible(ctx context.Context, executorID string) ([]*executorpb.PyModuleVenvResult, error) {
	for _, le := range pp.pool.Live() {
		if le.Executor.ID != executorID {
			continue
		}
		if !pp.eligible(le) {
			return nil, nil
		}
		return pp.pushTo(ctx, executorID)
	}
	return nil, nil
}

func (pp *pymodulePusher) pushTo(ctx context.Context, executorID string) ([]*executorpb.PyModuleVenvResult, error) {
	rctx, cancel := context.WithTimeout(ctx, pymoduleSyncTimeout())
	defer cancel()

	var target execpool.LiveExecutor
	found := false
	for _, le := range pp.pool.Live() {
		if le.Executor.ID == executorID {
			target, found = le, true
			break
		}
	}
	if !found {
		return nil, nil
	}
	ownerID, ok := pp.resolveOwnerID(rctx, target.Executor.Labels["owner"])
	if !ok {
		return nil, nil
	}

	recs, err := pp.store.List(rctx, ownerID)
	if err != nil {
		return nil, err
	}

	client, err := pp.pool.ConnectClientFor(executorID)
	if err != nil {
		return nil, err
	}
	resp, err := client.SyncPyModules(rctx, connect.NewRequest(buildPyModules(recs, pp.version)))
	if err != nil {
		return nil, err
	}
	slog.Info("pymodules synced",
		"executor", executorID, "written", resp.Msg.GetWritten(), "pruned", resp.Msg.GetPruned())
	return resp.Msg.GetVenvResults(), nil
}
