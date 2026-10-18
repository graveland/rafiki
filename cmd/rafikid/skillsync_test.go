// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

func TestBuildNamespacesGroupsAndSortsDeterministically(t *testing.T) {
	c := assert.NewCollecting(t)
	got := buildNamespaces([]skills.Record{
		{Namespace: "rafiki", Name: "zebra", Description: "z", Body: "bz"},
		{Namespace: "pg", Name: "alpha", Description: "a", Body: "ba"},
		{Namespace: "rafiki", Name: "apple", Description: "ap", Body: "bap"},
	}, nil, "1.2.3")

	c.Require().Len(got, 2, "got %d namespaces, want 2", len(got))
	// Sorted: the payload is compared for equality on the executor to decide
	// whether to rewrite, so an unstable order would rewrite every sync.
	if got[0].GetName() != "pg" || got[1].GetName() != "rafiki" {
		t.Fatalf("namespaces not sorted: %s, %s", got[0].GetName(), got[1].GetName())
	}
	rafiki := got[1]
	c.Require().Len(rafiki.GetSkills(), 2, "got %d skills in rafiki, want 2", len(rafiki.GetSkills()))
	c.Eq("apple", rafiki.GetSkills()[0].GetName(), "skills not sorted: first is")
	c.Eq("1.2.3", rafiki.GetVersion(), "version")
	// Names travel BARE: Claude Code derives "rafiki:apple" from the plugin
	// directory, so a prefix here would be applied twice.
	for _, s := range rafiki.GetSkills() {
		if len(s.GetName()) > 0 && s.GetName()[0] == 'r' && len(s.GetName()) > 7 && s.GetName()[:7] == "rafiki:" {
			t.Errorf("skill name %q carries a namespace prefix", s.GetName())
		}
	}
}

// An empty corpus is a full removal on the executor. That is a legitimate
// instruction and a catastrophic accident, so the pusher refuses to send one:
// a database blip that returned zero rows must not wipe every machine.
func TestPushRefusesToSendAnEmptyCorpus(t *testing.T) {
	c := assert.NewCollecting(t)
	c.False(shouldPush(nil), "pusher would send an empty corpus; a zero-row read must not wipe executors")
	c.True(shouldPush([]skills.Record{{Namespace: "rafiki", Name: "a", Body: "b"}}), "pusher refused a non-empty corpus")
}

func TestEligibleRequiresSkillsSyncAndClaude(t *testing.T) {
	c := assert.NewCollecting(t)
	eligible := func(d *executorpb.DescribeResponse) bool {
		return (&skillPusher{}).eligible(execpool.LiveExecutor{Executor: executors.Executor{ID: "e1"}, Describe: d})
	}
	c.True(eligible(&executorpb.DescribeResponse{SkillsSync: true, LaunchKinds: []string{"claude"}}), "an executor that accepts syncs and launches claude children must be eligible")
	c.False(eligible(&executorpb.DescribeResponse{SkillsSync: false, LaunchKinds: []string{"claude"}}), "an executor that did not opt into syncs must not be eligible")
	c.False(eligible(&executorpb.DescribeResponse{SkillsSync: true, LaunchKinds: []string{"fundi"}}), "an executor that launches no claude children must not be eligible")
	// A nil Describe must be declined, not dereferenced — the connect hook can
	// fire against a pool entry before its Describe has landed.
	c.False(eligible(nil), "a nil Describe must not be eligible (and must not panic)")
}

// pushStore is a store holding one deletable row, enough for every verb to
// resolve. The source is `manual` because a core or repo row is refused by
// DeleteSkill's own rules — the push contract is what these tests are about.
func pushStore() *fakeSkillStore {
	return &fakeSkillStore{rows: []skills.Record{
		{ID: "id-1", Namespace: "rafiki", Name: "a", Source: "manual"},
	}}
}

// The ticker is gone: skillPusher has no Run, and the corpus is refreshed on
// write instead (connectSkills.push below) and on executor connect (main.go's
// SetOnConnect). These pin the adapter side of that contract — push fires after
// a successful write, never after a failed one, and a nil push is tolerated
// since existing wiring constructs connectSkills without one.
func TestConnectSkillsPushesOnDelete(t *testing.T) {
	ck := assert.NewAborting(t)
	called := false
	c := connectSkills{
		st:   pushStore(),
		push: func(context.Context) { called = true },
	}
	ck.NoError(c.DeleteSkill(context.Background(), "rafiki", "a"), "delete")
	ck.True(called, "DeleteSkill did not push after a successful write")
}

func TestConnectSkillsPushesOnSetEnabled(t *testing.T) {
	ck := assert.NewAborting(t)
	called := false
	c := connectSkills{
		st:   pushStore(),
		push: func(context.Context) { called = true },
	}
	ck.NoError(c.SetSkillEnabled(context.Background(), "rafiki", "a", false), "set enabled")
	ck.True(called, "SetSkillEnabled did not push after a successful write")
}

// A write that did not happen pushes nothing: the corpora the executors already
// hold still match the store.
func TestConnectSkillsPushesNothingOnAFailedWrite(t *testing.T) {
	boom := errors.New("db is gone")
	for _, tc := range []struct {
		name   string
		store  *fakeSkillStore
		invoke func(connectSkills) error
	}{
		{"delete failed", &fakeSkillStore{
			rows: pushStore().rows, deleteErr: boom,
		}, func(c connectSkills) error { return c.DeleteSkill(context.Background(), "rafiki", "a") }},
		{"disable failed", &fakeSkillStore{
			rows: pushStore().rows, enableErr: boom,
		}, func(c connectSkills) error { return c.SetSkillEnabled(context.Background(), "rafiki", "a", false) }},
		{"nothing to write", &fakeSkillStore{}, func(c connectSkills) error {
			return c.DeleteSkill(context.Background(), "rafiki", "absent")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			c := connectSkills{st: tc.store, push: func(context.Context) { called = true }}
			assert.NewAborting(t).Error(tc.invoke(c), "write unexpectedly succeeded")
			assert.NewAborting(t).False(called, "push fired on a failed write")
		})
	}
}

func TestConnectSkillsToleratesNilPush(t *testing.T) {
	c := connectSkills{st: pushStore()}
	ck := assert.NewAborting(t)
	ck.NoError(c.DeleteSkill(context.Background(), "rafiki", "a"), "delete")
	ck.NoError(c.SetSkillEnabled(context.Background(), "rafiki", "a", true), "set enabled")
}

// pushFakeExecutor is one fake executor in a test pool. It answers Describe as
// a claude-launching, sync-accepting executor and records every corpus it is
// sent, which is what the push tests assert on.
type pushFakeExecutor struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	id string

	mu   sync.Mutex
	sent []*executorpb.SyncSkillsRequest
}

func (f *pushFakeExecutor) Describe(
	context.Context, *connect.Request[executorpb.DescribeRequest],
) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId:  f.id,
		SkillsSync:  true,
		LaunchKinds: []string{"claude"},
	}), nil
}

func (f *pushFakeExecutor) Health(
	context.Context, *connect.Request[executorpb.HealthRequest],
) (*connect.Response[executorpb.HealthResponse], error) {
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

func (f *pushFakeExecutor) SyncSkills(
	_ context.Context, req *connect.Request[executorpb.SyncSkillsRequest],
) (*connect.Response[executorpb.SyncSkillsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, req.Msg)
	return connect.NewResponse(&executorpb.SyncSkillsResponse{
		Written: int32(len(req.Msg.GetNamespaces())),
	}), nil
}

// lastSync returns the namespaces of the most recent sync, and whether one
// arrived at all.
func (f *pushFakeExecutor) lastSync() ([]*executorpb.SkillNamespace, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil, false
	}
	return f.sent[len(f.sent)-1].GetNamespaces(), true
}

// pushEnrollingStore admits an executor row per enroll token, so a pool can
// hold several fake executors with different owners.
type pushEnrollingStore struct {
	relayFakeStore
	byToken map[string]executors.Executor
}

func (s pushEnrollingStore) Enroll(_ context.Context, token string, _ map[string]string) (executors.Executor, string, error) {
	e, ok := s.byToken[token]
	if !ok {
		return executors.Executor{}, "", fmt.Errorf("unknown enroll token %q", token)
	}
	return e, "cred-" + e.ID, nil
}

// pushTestPool admits one fake executor per row into a fresh Pool over a real
// unix-socket reverse-dial — the same path a local executor takes — so pushTo
// is exercised against a real pool and a real Connect client rather than a
// hand-rolled fake.
func pushTestPool(t *testing.T, rows ...executors.Executor) (*execpool.Pool, map[string]*pushFakeExecutor) {
	t.Helper()
	c := assert.NewAborting(t)

	// /tmp directly with a short name: a TempDir path can exceed the unix
	// socket length limit.
	dir, err := os.MkdirTemp("/tmp", "skillsync-")
	c.NoError(err, "temp dir")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	c.NoError(err, "listen")
	t.Cleanup(func() { _ = ln.Close() })

	byToken := make(map[string]executors.Executor, len(rows))
	fakes := make(map[string]*pushFakeExecutor, len(rows))
	for _, row := range rows {
		byToken[row.ID] = row
		fakes[row.ID] = &pushFakeExecutor{id: row.ID}
	}

	pool := execpool.New(pushEnrollingStore{byToken: byToken})
	go func() { _ = pool.Serve(ln) }()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, row := range rows {
		_, h := executorpbconnect.NewExecutorServiceHandler(fakes[row.ID])
		go func(id string, h http.Handler) {
			_ = execpool.Connect(ctx, execpool.ConnectOptions{
				SocketPath:  sock,
				EnrollToken: id,
				Handler:     h,
			})
		}(row.ID, h)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pool.Live()) == len(rows) {
			return pool, fakes
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fake executors never joined the pool: %d of %d live", len(pool.Live()), len(rows))
	return nil, nil
}

// pushedNames lists the skill names in one namespace of a pushed corpus.
func pushedNames(ns *executorpb.SkillNamespace) []string {
	out := make([]string, 0, len(ns.GetSkills()))
	for _, s := range ns.GetSkills() {
		out = append(out, s.GetName())
	}
	return out
}

// namespaceNamed finds one namespace in a pushed corpus.
func namespaceNamed(nss []*executorpb.SkillNamespace, name string) *executorpb.SkillNamespace {
	for _, ns := range nss {
		if ns.GetName() == name {
			return ns
		}
	}
	return nil
}

// TestSkillPushPerExecutorOwner pins the corpus as PER-OWNER: two people's
// claude children share one executor's skills directory, so each executor is
// sent what its own owner may see — their own user layer, the shared layers,
// and nothing of the other's.
func TestSkillPushPerExecutorOwner(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &fakeSkillStore{rows: []skills.Record{
		{ID: "core", Namespace: "rafiki", Name: "core-skill", Body: "core", Source: skills.CoreSource, Enabled: true},
		{ID: "u1row", Namespace: "mine", Name: "only-u1", Body: "b", Source: "repo:u1", OwnerUserID: "u1", Enabled: true},
		{ID: "u2row", Namespace: "mine", Name: "only-u2", Body: "b", Source: "repo:u2", OwnerUserID: "u2", Enabled: true},
	}}
	pool, fakes := pushTestPool(t,
		executors.Executor{ID: "e1", Enabled: true, OwnerUserID: "u1"},
		executors.Executor{ID: "e2", Enabled: true, OwnerUserID: "u2"},
	)
	sp := &skillPusher{pool: pool, store: store, version: "v-test"}
	ctx := context.Background()

	ck.Require().NoError(sp.pushTo(ctx, "e1"), "push to e1")
	nss, ok := fakes["e1"].lastSync()
	ck.Require().True(ok, "e1 received no sync")
	ck.EqDeep([]string{"core-skill"}, pushedNames(namespaceNamed(nss, "rafiki")), "e1's shared layer")
	ck.EqDeep([]string{"only-u1"}, pushedNames(namespaceNamed(nss, "mine")), "e1's own user layer")
	for _, ns := range nss {
		for _, name := range pushedNames(ns) {
			ck.False(name == "only-u2", "e1 was sent %q, another owner's skill", name)
		}
	}

	ck.Require().NoError(sp.pushTo(ctx, "e2"), "push to e2")
	nss, ok = fakes["e2"].lastSync()
	ck.Require().True(ok, "e2 received no sync")
	ck.EqDeep([]string{"core-skill"}, pushedNames(namespaceNamed(nss, "rafiki")), "e2's shared layer")
	ck.EqDeep([]string{"only-u2"}, pushedNames(namespaceNamed(nss, "mine")), "e2's own user layer")
	for _, ns := range nss {
		for _, name := range pushedNames(ns) {
			ck.False(name == "only-u1", "e2 was sent %q, another owner's skill", name)
		}
	}
}

// TestSkillPushIncludesFiles pins that a skill's supporting files travel with
// it: a claude child reads them from the plugin directory, so a corpus without
// them is a skill whose body points at nothing.
func TestSkillPushIncludesFiles(t *testing.T) {
	ck := assert.NewCollecting(t)
	want := []skills.File{
		{Path: "docs/notes.md", Mode: 0o644, Content: []byte("notes\n")},
		{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
	}
	store := &fakeSkillStore{
		rows: []skills.Record{
			{ID: "f1", Namespace: "rafiki", Name: "with-files", Body: "b", Source: skills.CoreSource, Enabled: true, HasFiles: true},
			{ID: "f2", Namespace: "rafiki", Name: "no-files", Body: "b", Source: skills.CoreSource, Enabled: true},
		},
		files: map[string][]skills.File{"f1": want},
	}
	pool, fakes := pushTestPool(t, executors.Executor{ID: "e1", Enabled: true})

	sp := &skillPusher{pool: pool, store: store, version: "v-test"}
	ck.Require().NoError(sp.pushTo(context.Background(), "e1"), "push")

	nss, ok := fakes["e1"].lastSync()
	ck.Require().True(ok, "executor received no sync")
	rafiki := namespaceNamed(nss, "rafiki")
	ck.Require().NotNil(rafiki, "no rafiki namespace in the pushed corpus")
	ck.Require().Len(rafiki.GetSkills(), 2, "got %d skills, want 2", len(rafiki.GetSkills()))

	byName := map[string]*executorpb.SyncSkill{}
	for _, s := range rafiki.GetSkills() {
		byName[s.GetName()] = s
	}
	got := byName["with-files"].GetFiles()
	ck.Require().Len(got, len(want), "got %d file(s), want %d", len(got), len(want))
	for i, f := range want {
		ck.Eq(f.Path, got[i].GetPath(), "file %d path", i)
		ck.Eq(f.Mode, got[i].GetMode(), "file %d mode", i)
		ck.Eq(string(f.Content), string(got[i].GetContent()), "file %d content", i)
	}
	ck.Len(byName["no-files"].GetFiles(), 0, "a skill with no files carried some: got")
}

// TestSkillPushOwnerlessGetsSharedOnly: an unowned executor (a lone
// developer's machine, serving nobody in particular) gets the shared layers —
// core plus global — and no user's rows, since Resolve has no owner to resolve
// against.
func TestSkillPushOwnerlessGetsSharedOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	store := &fakeSkillStore{rows: []skills.Record{
		{ID: "core", Namespace: "rafiki", Name: "core-skill", Body: "core", Source: skills.CoreSource, Enabled: true},
		{ID: "global", Namespace: "shared", Name: "shared-skill", Body: "g", Source: "manual", Enabled: true},
		{ID: "u1row", Namespace: "mine", Name: "only-u1", Body: "b", Source: "repo:u1", OwnerUserID: "u1", Enabled: true},
	}}
	pool, fakes := pushTestPool(t, executors.Executor{ID: "e1", Enabled: true})

	sp := &skillPusher{pool: pool, store: store, version: "v-test"}
	ck.Require().NoError(sp.pushTo(context.Background(), "e1"), "push")

	nss, ok := fakes["e1"].lastSync()
	ck.Require().True(ok, "executor received no sync")
	ck.EqDeep([]string{"core-skill"}, pushedNames(namespaceNamed(nss, "rafiki")), "shared core layer")
	ck.EqDeep([]string{"shared-skill"}, pushedNames(namespaceNamed(nss, "shared")), "shared global layer")
	ck.Nil(namespaceNamed(nss, "mine"), "an ownerless executor was sent a user layer")
}
