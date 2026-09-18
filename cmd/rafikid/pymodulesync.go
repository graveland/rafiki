// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"slices"
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

func (pp *pymodulePusher) pushAll(ctx context.Context) {
	for _, le := range pp.pool.Live() {
		if !pp.eligible(le) {
			continue
		}
		if err := pp.pushTo(ctx, le.Executor.ID); err != nil {
			slog.Warn("pymodule sync failed", "executor", le.Executor.ID, "error", err)
		}
	}
}

func (pp *pymodulePusher) pushIfEligible(ctx context.Context, executorID string) error {
	for _, le := range pp.pool.Live() {
		if le.Executor.ID != executorID {
			continue
		}
		if !pp.eligible(le) {
			return nil
		}
		return pp.pushTo(ctx, executorID)
	}
	return nil
}

func (pp *pymodulePusher) pushTo(ctx context.Context, executorID string) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
		return nil
	}
	ownerID, ok := pp.resolveOwnerID(rctx, target.Executor.Labels["owner"])
	if !ok {
		return nil
	}

	recs, err := pp.store.List(rctx, ownerID)
	if err != nil {
		return err
	}

	client, err := pp.pool.ConnectClientFor(executorID)
	if err != nil {
		return err
	}
	resp, err := client.SyncPyModules(rctx, connect.NewRequest(buildPyModules(recs, pp.version)))
	if err != nil {
		return err
	}
	slog.Info("pymodules synced",
		"executor", executorID, "written", resp.Msg.GetWritten(), "pruned", resp.Msg.GetPruned())
	return nil
}
