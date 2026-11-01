// SPDX-License-Identifier: Apache-2.0

package main

// The database-capability degradation: an embedded install has neither
// pg_textsearch nor vector, so the daemon must build a runtime that refuses
// search instead of one that queries missing bm25 indexes. buildRecallRuntime
// takes the probe's answer as recallCaps; these pin what each answer builds.

import (
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/recall"

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
