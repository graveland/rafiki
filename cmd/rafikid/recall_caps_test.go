// SPDX-License-Identifier: Apache-2.0

package main

// The database-capability degradation: an embedded install has neither
// pg_textsearch nor vector, so the daemon must build a runtime that refuses
// search instead of one that queries missing bm25 indexes. buildRecallRuntime
// takes the probe's answer as recallCaps; these pin what each answer builds.

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
