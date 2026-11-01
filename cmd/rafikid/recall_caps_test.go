// SPDX-License-Identifier: Apache-2.0

package main

// The database-capability degradation: an embedded install has neither
// pg_textsearch nor vector, so the daemon must build a runtime that refuses
// search instead of one that queries missing bm25 indexes. buildRecallRuntime
// takes the probe's answer as recallCaps; startRecall probes it and decides what
// to start. These pin both, plus the refusals and the memory path that must keep
// working when search is off.

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// embeddingsConfigured is a providers set that ASKS for embeddings — the
// config a degradation must overrule when the database cannot store them.
func embeddingsConfigured() *providers.Set {
	return &providers.Set{
		Embeddings: &providers.EmbeddingsConfig{
			URL: "http://127.0.0.1:1/v1/embeddings", Model: "emb-1", Dimensions: 8,
		},
	}
}

// TestRecallCapsNoTextsearch pins the embedded shape: no pg_textsearch and no
// vector turns search off and builds NO embedder, even though providers.toml
// configures one. recalldb.New(nil) is deliberate — buildRecallRuntime queries
// nothing, and the nil pool proves the caps, not the database, decided.
func TestRecallCapsNoTextsearch(t *testing.T) {
	c := assert.NewAborting(t)
	rt := buildRecallRuntime(nil, embeddingsConfigured(), nil, discardLogger(), recallCaps{textsearch: false, vector: false})
	c.True(rt.searchOff, "searchOff with no pg_textsearch")
	c.Nil(rt.emb, "embedder built without the vector extension")
	c.NotNil(rt.st, "store must still be built: memory tools work without search")
}

// TestRecallCapsNoVector pins the half-degraded shape: pg_textsearch present,
// vector absent. Search runs BM25-only, so searchOff stays false while the
// embedder — which would write into bytea placeholder columns — is not built.
func TestRecallCapsNoVector(t *testing.T) {
	c := assert.NewAborting(t)
	rt := buildRecallRuntime(nil, embeddingsConfigured(), nil, discardLogger(), recallCaps{textsearch: true, vector: false})
	c.False(rt.searchOff, "searchOff with pg_textsearch installed")
	c.Nil(rt.emb, "embedder built without the vector extension")
}

// TestRecallSearchOffBindingRefuses pins the tool-side refusal: a binding on a
// search-less runtime answers recall.ErrSearchUnavailable. The nil store is
// the proof it never reached one — a search attempt would panic here.
func TestRecallSearchOffBindingRefuses(t *testing.T) {
	c := assert.NewAborting(t)
	b := &recallBinding{searchOff: true}
	out, err := b.Recall(t.Context(), tools.RecallQuery{Query: "anything"})
	c.Eq("", out, "output")
	c.True(errors.Is(err, recall.ErrSearchUnavailable), "err = %v, want recall.ErrSearchUnavailable", err)
}

// TestRecallSearchOffBackfillRefuses pins the Connect-side backfill refusal. A
// search-less database runs no indexer, so an armed backfill could never turn
// into work: the verb must answer recall.ErrSearchUnavailable (CodeUnavailable
// on the wire) instead of reporting success for state nobody consumes.
//
// Two runtimes, one per claim. The nil-store runtime proves the refusal happens
// before the first SetState — reaching the store there would nil-panic — and
// the fake-store runtime proves the three state keys are untouched, which is
// the cheap form of "nothing was armed".
func TestRecallSearchOffBackfillRefuses(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u-admin", Username: "brent", Via: server.ProvenanceUser, IsAdmin: true,
	})
	since := time.Unix(1700000000, 0).UTC()

	// A nil store and a nil indexer: the guard must answer before either is
	// touched, so neither a panic nor a nil dereference is reachable here.
	m := connectRecall{c: &Controller{recall: &recallRuntime{searchOff: true}}}
	err := m.Backfill(ctx, since, 2.5)
	c.True(errors.Is(err, recall.ErrSearchUnavailable), "Backfill err = %v, want recall.ErrSearchUnavailable", err)

	// The same refusal through the handler, which is what maps the sentinel to
	// CodeUnavailable (recallError) — and what proves Backfill routes it there.
	s := &connectapi.Server{}
	s.SetRecallManager(m)
	_, err = s.RecallBackfill(ctx, connect.NewRequest(&rafikiv1.RecallBackfillRequest{
		Since: timestamppb.New(since), MaxCostUsd: 2.5,
	}))
	c.Require().Error(err, "RecallBackfill on a search-less database")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "got code")
	c.True(errors.Is(err, recall.ErrSearchUnavailable), "error does not wrap recall.ErrSearchUnavailable: %v", err)

	// Nothing armed: the three keys the success path writes are all absent.
	fs := &fakeConnectRecallStore{}
	fm := connectRecall{c: &Controller{recall: &recallRuntime{st: fs, searchOff: true}}}
	err = fm.Backfill(ctx, since, 2.5)
	c.True(errors.Is(err, recall.ErrSearchUnavailable), "fake-store Backfill err = %v, want recall.ErrSearchUnavailable", err)
	c.Len(fs.states, 0, "backfill state written on a search-less database: %v", fs.states)
}

// TestRecallSearchOffStartRecallWiresStoreWithoutIndexer drives startRecall itself —
// the one function carrying the "store assigned in every case, indexer not
// started when search is off" requirement. The scratch database is migrated
// (so both extensions exist) and then has pg_textsearch DROPPED, which is the
// embedded shape reached through the real probe rather than through a seam.
//
// Three mutations this fails on, and nothing else does:
//   - moving `c.recall = rt` below the searchOff return → ctrl.recall is nil;
//   - dropping the early return → the indexer goroutine starts, so recallWg
//     never drains and the wait below times out;
//   - hardcoding the caps instead of probing → vector IS installed here while
//     pg_textsearch is not, so the embedder is built and searchOff is true only
//     if the probe's mixed answer drove it.
func TestRecallSearchOffStartRecallWiresStoreWithoutIndexer(t *testing.T) {
	c := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := t.Context()

	// DROP ... CASCADE takes the bm25 indexes with it; nothing below reads them.
	if _, err := pool.Exec(ctx, `DROP EXTENSION pg_textsearch CASCADE`); err != nil {
		t.Fatalf("drop pg_textsearch: %v", err)
	}

	ctrl := &Controller{}
	startRecall(ctx, ctrl, pool, embeddingsConfigured(), nil, discardLogger())

	c.NotNil(ctrl.recall, "recall runtime must be wired even when search is off: memory tools depend on the store")
	c.True(ctrl.recall.searchOff, "searchOff after the probe found no pg_textsearch")
	c.NotNil(ctrl.recall.st, "store")
	c.NotNil(ctrl.recall.emb, "vector is installed here, so the embedder must still be built — a hardcoded caps would miss this")

	// No indexer goroutine: recallWg has no Add, so Wait returns immediately.
	// Had the early return been dropped, the indexer would be running and Wait
	// would block until this test's context is cancelled.
	done := make(chan struct{})
	go func() { ctrl.recallWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recall indexer goroutine started on a database with no pg_textsearch")
	}
}

// TestRecallSearchOffMemoryToolsStillWork pins the other half of the design's
// requirement: with search off, the memory verbs must neither refuse nor
// dereference anything nil — they never read searchOff and use only the store.
// A future `if searchOff` added to a memory method would be a silent regression
// of "memory tools keep working everywhere", so every memory verb on both faces
// is exercised against a recording fake.
func TestRecallSearchOffMemoryToolsStillWork(t *testing.T) {
	c := assert.NewAborting(t)
	fs := &fakeConnectRecallStore{memories: []recall.Memory{{Path: "proj", Name: "note"}}}
	rt := &recallRuntime{st: fs, searchOff: true}
	ctx := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u-bob", Via: server.ProvenanceUser,
	})
	m := connectRecall{c: &Controller{recall: rt}}

	// The Connect face.
	mem, err := m.PutMemory(ctx, recall.Memory{Path: "proj", Name: "note", Body: "b"})
	c.NoError(err, "PutMemory with search off")
	c.Eq("u-bob", fs.putOwner, "PutMemory owner")
	c.Eq("m-id", mem.ID, "PutMemory result")
	got, err := m.GetMemory(ctx, "proj", "note")
	c.NoError(err, "GetMemory with search off")
	c.Eq("note", got.Name, "GetMemory result")
	tree, err := m.MemoryTree(ctx, "proj", 2)
	c.NoError(err, "MemoryTree with search off")
	c.Len(tree, 1, "MemoryTree rows")
	c.NoError(m.DeleteMemory(ctx, "proj", "note"), "DeleteMemory with search off")
	c.Eq("proj/note", fs.deleted, "DeleteMemory reached the store")

	// The in-process tool face (the fundi child's binding), same runtime shape.
	b := bindingWith(fs, users.Identity{UserID: "u-bob"})
	b.searchOff = true
	_, err = b.MemoryPut(ctx, "proj", "note2", "body", nil)
	c.NoError(err, "binding MemoryPut with search off")
	c.Eq("u-bob", fs.putOwner, "binding MemoryPut owner")
	c.NoError(b.MemoryDelete(ctx, "proj", "note2"), "binding MemoryDelete with search off")
}

// TestRecallSearchOffBackfillChecksAuthorityAndBudgetFirst pins the ORDER of
// Backfill's three refusals: authorization, then request validation, then
// capability. Hoisting the searchOff check above either would tell a caller who
// may not run the verb — or who sent an invalid request — what the operator's
// Postgres has, and would pass every other test.
func TestRecallSearchOffBackfillChecksAuthorityAndBudgetFirst(t *testing.T) {
	c := assert.NewAborting(t)
	since := time.Unix(1700000000, 0).UTC()
	m := connectRecall{c: &Controller{recall: &recallRuntime{searchOff: true}}}
	s := &connectapi.Server{}
	s.SetRecallManager(m)

	nonAdmin := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u-bob", Username: "bob", Via: server.ProvenanceUser,
	})
	err := m.Backfill(nonAdmin, since, 2.5)
	c.True(errors.Is(err, recall.ErrInvalidScope), "non-admin on a search-less database: err = %v, want ErrInvalidScope", err)
	c.False(errors.Is(err, recall.ErrSearchUnavailable), "the capability leaked to a non-admin: %v", err)
	_, err = s.RecallBackfill(nonAdmin, connect.NewRequest(&rafikiv1.RecallBackfillRequest{
		Since: timestamppb.New(since), MaxCostUsd: 2.5,
	}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "non-admin code")

	admin := server.WithIdentity(context.Background(), &server.Identity{
		UserID: "u-admin", Username: "brent", Via: server.ProvenanceUser, IsAdmin: true,
	})
	for _, budget := range []float64{0, -1} {
		err := m.Backfill(admin, since, budget)
		c.True(errors.Is(err, connectapi.ErrNoBackfillBudget), "budget %v on a search-less database: err = %v, want ErrNoBackfillBudget", budget, err)
		c.False(errors.Is(err, recall.ErrSearchUnavailable), "budget %v: the capability leaked past request validation: %v", budget, err)
	}
	_, err = s.RecallBackfill(admin, connect.NewRequest(&rafikiv1.RecallBackfillRequest{
		Since: timestamppb.New(since), MaxCostUsd: 0,
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "zero-budget code")
}
