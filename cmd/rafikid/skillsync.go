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
	"go.graveland.dev/rafiki/pkg/skills"
)

// skillSyncInterval is the slow tick. The push is idempotent and the executor
// rewrites nothing when content is unchanged, so this only needs to be often
// enough that an edit reaches a long-lived executor without a reconnect.
const skillSyncInterval = 10 * time.Minute

// skillPusher delivers the daemon's skill corpus to every executor that both
// launches claude children and accepts a sync.
//
// Per-executor rather than per-launch: several claude children share one
// executor's skills directory, daraja is per-child, and ClaudeParams' fields
// are launch-only. This is sc's model with the database standing in for its
// bundle endpoint and rafiki's executors standing in for "every machine".
type skillPusher struct {
	pool    *execpool.Pool
	store   skills.Store
	version string
}

// shouldPush refuses an empty corpus. An empty namespace list means "remove
// every managed tree" on the executor, so a store read that legitimately
// returns zero rows — a fresh install, or a blip that surfaced as an empty
// result — would otherwise wipe the corpus off every machine at once. sc
// encodes the same rule for the same reason: an empty bundle is content loss,
// never a prune instruction.
func shouldPush(recs []skills.Record) bool { return len(recs) > 0 }

// buildNamespaces groups rows into per-namespace payloads, sorted at both
// levels. The ordering is load-bearing: the executor compares the rendered tree
// against what is on disk to decide whether to rewrite, so an unstable order
// would rewrite every namespace on every sync and keep Claude Code's file
// watching permanently busy.
func buildNamespaces(recs []skills.Record, version string) []*executorpb.SkillNamespace {
	byNS := map[string][]*executorpb.SyncSkill{}
	for _, r := range recs {
		byNS[r.Namespace] = append(byNS[r.Namespace], &executorpb.SyncSkill{
			// Bare name: Claude Code derives "<namespace>:<name>" from the
			// plugin directory itself.
			Name:        r.Name,
			Description: r.Description,
			Body:        r.Body,
		})
	}
	names := make([]string, 0, len(byNS))
	for ns := range byNS {
		names = append(names, ns)
	}
	slices.Sort(names)

	out := make([]*executorpb.SkillNamespace, 0, len(names))
	for _, ns := range names {
		sk := byNS[ns]
		slices.SortFunc(sk, func(a, b *executorpb.SyncSkill) int {
			switch {
			case a.GetName() < b.GetName():
				return -1
			case a.GetName() > b.GetName():
				return 1
			default:
				return 0
			}
		})
		out = append(out, &executorpb.SkillNamespace{
			Name: ns, Version: version, Skills: sk,
		})
	}
	return out
}

// Run pushes on a slow tick until ctx is done. Connect-triggered pushes arrive
// separately through the pool's on-connect hook.
func (sp *skillPusher) Run(ctx context.Context) {
	t := time.NewTicker(skillSyncInterval)
	defer t.Stop()
	sp.pushAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sp.pushAll(ctx)
		}
	}
}

func (sp *skillPusher) pushAll(ctx context.Context) {
	for _, le := range sp.pool.Live() {
		if !sp.eligible(le) {
			continue
		}
		if err := sp.pushTo(ctx, le.Executor.ID); err != nil {
			// Never fatal, and never retried harder: last-good-wins, so the
			// executor keeps whatever corpus it already has until the next tick.
			slog.Warn("skill sync failed", "executor", le.Executor.ID, "error", err)
		}
	}
}

// eligible reports whether an executor both launches claude children and
// accepts a sync. An executor serving only workspace tools has no claude child
// to feed and is skipped.
func (sp *skillPusher) eligible(le execpool.LiveExecutor) bool {
	if le.Describe == nil || !le.Describe.GetSkillsSync() {
		return false
	}
	return slices.Contains(le.Describe.GetLaunchKinds(), "claude")
}

// pushIfEligible pushes to one executor only after confirming it would pass
// the tick path's own filter. The on-connect hook fires for EVERY executor
// that joins the pool — including session executors (rafiki create/attach),
// which build their Options without SkillsSync, host no claude children and
// never opted into syncs — so calling pushTo from it directly sent every such
// executor an RPC it answered permission_denied. Filtering here keeps the two
// paths in agreement: pushAll scans Live() through eligible() on its tick, and
// the connect path is that same check applied to one id. A miss is fine — the
// next tick covers it — so this never blocks or retries; it just declines.
func (sp *skillPusher) pushIfEligible(ctx context.Context, executorID string) error {
	for _, le := range sp.pool.Live() {
		if le.Executor.ID != executorID {
			continue
		}
		if !sp.eligible(le) {
			return nil
		}
		return sp.pushTo(ctx, executorID)
	}
	return nil
}

func (sp *skillPusher) pushTo(ctx context.Context, executorID string) error {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	recs, err := sp.store.List(rctx, true)
	if err != nil {
		return err
	}
	if !shouldPush(recs) {
		slog.Warn("skill corpus is empty; not syncing",
			"executor", executorID,
			"reason", "an empty payload would remove every managed tree on that machine")
		return nil
	}

	client, err := sp.pool.ConnectClientFor(executorID)
	if err != nil {
		return err
	}
	resp, err := client.SyncSkills(rctx, connect.NewRequest(&executorpb.SyncSkillsRequest{
		Namespaces: buildNamespaces(recs, sp.version),
	}))
	if err != nil {
		return err
	}
	slog.Info("skills synced",
		"executor", executorID,
		"written", resp.Msg.GetWritten(),
		"pruned", resp.Msg.GetPruned(),
		"dir", resp.Msg.GetSkillsDir())
	return nil
}
