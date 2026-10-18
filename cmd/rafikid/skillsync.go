// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/skills"
)

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

// shouldPush refuses to act on an EMPTY STORE READ. An empty namespace list
// means "remove every managed tree" on the executor, so a store read that
// legitimately returns zero rows — a fresh install, or a blip that surfaced as
// an empty result — would otherwise wipe the corpus off every machine at once.
// sc encodes the same rule for the same reason: an empty bundle is content
// loss, never a prune instruction.
//
// The test is on the rows as READ, never on the resolved set. Resolving to
// nothing is a legitimate INSTRUCTION — every skill this owner can see is
// disabled — and skipping it would leave the executor's copy of those skills
// enabled forever, so disabling a skill would never reach the machine it
// matters on. An all-disabled owner's executor gets the empty corpus and prunes.
func shouldPush(rows []skills.Record) bool { return len(rows) > 0 }

// skillFileProtos renders a skill's stored files as the wire type. Order is the
// store's (sorted by path), which is what the executor's tree comparison
// depends on.
func skillFileProtos(files []skills.File) []*executorpb.SkillFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]*executorpb.SkillFile, 0, len(files))
	for _, f := range files {
		out = append(out, &executorpb.SkillFile{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	return out
}

// buildNamespaces groups rows into per-namespace payloads, sorted at both
// levels. The ordering is load-bearing: the executor compares the rendered tree
// against what is on disk to decide whether to rewrite, so an unstable order
// would rewrite every namespace on every sync and keep Claude Code's file
// watching permanently busy.
//
// files carries the supporting files of the rows that have any, keyed by row id
// — the same shape Store.Files answers with, so a row with no entry simply
// carries none.
func buildNamespaces(recs []skills.Record, files map[string][]skills.File, version string) []*executorpb.SkillNamespace {
	byNS := map[string][]*executorpb.SyncSkill{}
	for _, r := range recs {
		byNS[r.Namespace] = append(byNS[r.Namespace], &executorpb.SyncSkill{
			// Bare name: Claude Code derives "<namespace>:<name>" from the
			// plugin directory itself.
			Name:        r.Name,
			Description: r.Description,
			Body:        r.Body,
			Files:       skillFileProtos(files[r.ID]),
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

func (sp *skillPusher) pushAll(ctx context.Context) {
	for _, le := range sp.pool.Live() {
		if !sp.eligible(le) {
			continue
		}
		if err := sp.pushTo(ctx, le.Executor.ID); err != nil {
			// Never fatal, and never retried harder: last-good-wins, so the
			// executor keeps whatever corpus it already has until the next
			// write or reconnect.
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

// ownerOf returns the owner of the live executor with this id. The second
// result is false when no such executor is live — there is then no owner to
// resolve a corpus for, and pushing one would be pushing a guess.
func (sp *skillPusher) ownerOf(executorID string) (owner string, live bool) {
	for _, le := range sp.pool.Live() {
		if le.Executor.ID == executorID {
			return le.Executor.OwnerUserID, true
		}
	}
	return "", false
}

// filesFor loads the supporting files of every resolved row that has any, in
// one query. Rows without files are not asked about: the inventory already
// said they have none, and a skill directory is not worth a round trip.
func (sp *skillPusher) filesFor(ctx context.Context, recs []skills.Record) (map[string][]skills.File, error) {
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		if r.HasFiles {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return sp.store.Files(ctx, ids)
}

// pushIfEligible pushes to one executor only after confirming it would pass
// the pushAll path's own filter. The on-connect hook fires for EVERY executor
// that joins the pool — including session executors (rafiki create/attach),
// which build their Options without SkillsSync, host no claude children and
// never opted into syncs — so calling pushTo from it directly sent every such
// executor an RPC it answered permission_denied. Filtering here keeps the two
// paths in agreement: pushAll scans Live() through eligible(), and the connect
// path is that same check applied to one id. A miss is fine — the next write
// or reconnect covers it — so this never blocks or retries; it just declines.
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

	// The corpus is per-OWNER, not per-daemon: two people's claude children
	// share one executor's skills directory, so what lands there is what the
	// executor's own owner may see. An unowned executor gets the shared layers
	// only, which is Resolve's answer for an empty owner.
	owner, live := sp.ownerOf(executorID)
	if !live {
		return fmt.Errorf("executor %s is no longer live", executorID)
	}

	rows, err := sp.store.List(rctx, false)
	if err != nil {
		return err
	}
	// Guard the READ, not the resolved set: a store that answered with rows has
	// said something, even when every one of them resolves away (all disabled),
	// and that answer is "prune".
	if !shouldPush(rows) {
		slog.Warn("skill corpus is empty; not syncing",
			"executor", executorID,
			"owner", owner,
			"reason", "an empty payload would remove every managed tree on that machine")
		return nil
	}
	recs := skills.Resolve(rows, owner)

	files, err := sp.filesFor(rctx, recs)
	if err != nil {
		return err
	}

	client, err := sp.pool.ConnectClientFor(executorID)
	if err != nil {
		return err
	}
	resp, err := client.SyncSkills(rctx, connect.NewRequest(&executorpb.SyncSkillsRequest{
		Namespaces: buildNamespaces(recs, files, sp.version),
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
