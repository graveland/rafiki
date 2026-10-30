// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/routing"
)

// unsetConvCacheBytes clears the variable for one test and restores the
// previous state afterwards. t.Setenv registers the restore; the Unsetenv is
// what makes the state genuinely unset rather than empty.
func unsetConvCacheBytes(t *testing.T) {
	t.Helper()
	t.Setenv(convCacheBytesEnv, "")
	if err := os.Unsetenv(convCacheBytesEnv); err != nil {
		t.Fatalf("unset %s: %v", convCacheBytesEnv, err)
	}
}

func TestConvCacheBytesDefault(t *testing.T) {
	c := assert.NewAborting(t)
	unsetConvCacheBytes(t)
	n, err := convCacheBytes()
	c.NoError(err, "unset is the documented default, not an error")
	c.Eq(int64(268435456), n, "the default is 256 MiB")
}

func TestConvCacheBytesRefusesNonPositive(t *testing.T) {
	c := assert.NewAborting(t)
	for _, v := range []string{"0", "-1", "abc"} {
		t.Setenv(convCacheBytesEnv, v)
		_, err := convCacheBytes()
		c.ErrorContains(err,
			fmt.Sprintf("RAFIKI_CONV_CACHE_BYTES must be a positive integer, got %q", v),
			"value %q", v)
	}
}

func TestConvCacheBytesParses(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv(convCacheBytesEnv, "1048576")
	n, err := convCacheBytes()
	c.NoError(err, "a positive integer parses")
	c.Eq(int64(1048576), n, "the value is taken as given")
}

// TestConvStoreSharedByControllerAndProxyFace pins the daemon's one-cache rule
// at the two capture paths that can break it: the Controller's capture store
// and the proxy face's must be backed by the same *conv.Store the daemon built,
// never by a private cache of their own over the same rows.
//
// It also pins clearMarker moving with captureStore — both point at the same
// store, and a /clear written through a stale one would land where no reader
// looks.
func TestConvStoreSharedByControllerAndProxyFace(t *testing.T) {
	t.Setenv("RAFIKI_PROXY_LISTEN", "127.0.0.1:0")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	unsetConvCacheBytes(t)
	c := assert.NewAborting(t)

	// A parsed-but-never-connected pool: every store below is built, none is
	// queried.
	pool := testPool(t)
	convs, err := newConvStore(pool)
	c.NoError(err, "newConvStore")

	dir := t.TempDir()
	ctrl := NewController(childstore.New(), filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, pool, nil, false, t.Context(), nil, nil, nil, nil)
	t.Cleanup(ctrl.Stop)
	ctrl.setConvStore(convs)
	c.Eq(convs, ctrl.convs, "the Controller carries the daemon's store")
	c.NotNil(ctrl.captureStore, "a pool builds the Controller's capture store")
	c.Eq(convs, ctrl.captureStore.ConvStore(), "the Controller's capture store shares it")
	marker, ok := ctrl.clearMarker.(*capture.CaptureStore)
	c.True(ok, "clearMarker is the capture store")
	c.True(marker == ctrl.captureStore, "clearMarker moved with captureStore")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	face, err := startProxyFace(ctx, faceOptions{
		Pool:      pool,
		Logger:    slog.New(slog.DiscardHandler),
		ConvStore: convs,
		// A guard of its own: the nil branch would rehydrate from the
		// unreachable pool, which this test has no reason to exercise.
		Guard: routing.NewProviderGuard(routing.DefaultEjectTTL, slog.New(slog.DiscardHandler)),
	})
	c.NoError(err, "startProxyFace")
	defer face.Close(ctx)
	c.NotNil(face.captureStore, "a pool builds the proxy face's capture store")
	c.Eq(convs, face.captureStore.ConvStore(), "the proxy face's capture store shares it")
	c.True(ctrl.captureStore.ConvStore() == face.captureStore.ConvStore(),
		"the Controller and the proxy face capture through one store, not two")
}
