// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/embedpg"
)

func TestRequireDSNPrefersExplicitFlag(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://from-env/db")
	c := assert.NewAborting(t)
	got, err := requireDSN("postgres://from-flag/db")
	c.NoError(err, "requireDSN returned error")
	c.Eq("postgres://from-flag/db", got, "want the flag value, got")
}

func TestRequireDSNFallsBackToEnv(t *testing.T) {
	t.Setenv("RAFIKI_DB", "postgres://from-env/db")
	c := assert.NewAborting(t)
	got, err := requireDSN("")
	c.NoError(err, "requireDSN returned error")
	c.Eq("postgres://from-env/db", got, "want the env value, got")
}

// The daemon requires a database (Phase C design 2.1). An absent DSN is a
// startup error, not a degraded mode: without it there is no history, no cost
// accounting, no task ledger, no users, and no executor plane.
func TestRequireDSNEmptyIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_DB", "")
	c := assert.NewAborting(t)
	_, err := requireDSN("")
	c.Error(err, "want an error when no DSN is configured, got nil")
	// The message must tell the operator how to fix it.
	c.StrContains(err.Error(), "RAFIKI_DB", "error must name RAFIKI_DB, got: %v", err)
	c.StrContains(err.Error(), "rafiki setup", "error must name the local-install path, got: %v", err)
	c.StrContains(err.Error(), "RAFIKI_DB=embedded", "error must name the embedded mode, got: %v", err)
}

// Anything that is not the sentinel is somebody else's DSN: it comes back
// byte-identical, and the embedded server is never even constructed.
func TestResolveDSNPassesThroughOtherValues(t *testing.T) {
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		t.Fatal("newEmbeddedServer called for a non-embedded DSN")
		return embedpg.Server{}
	}

	c := assert.NewAborting(t)
	for _, dsn := range []string{"postgres://u@h/db", "host=/tmp dbname=x"} {
		got, err := resolveDSN(context.Background(), dsn)
		c.NoError(err, "resolveDSN(%q) returned error", dsn)
		c.Eq(dsn, got, "resolveDSN(%q) must pass through, got", dsn)
	}
}

// The embedded branch really goes to pkg/embedpg: the fixture download is
// asked for the pinned artifact, and its failure surfaces wrapped as an
// "embedded database:" error rather than as a DSN pgx would try to dial.
func TestResolveDSNEmbeddedUsesEmbedpg(t *testing.T) {
	// NOT t.TempDir(): on macOS its path pushes the unix socket path past the
	// 103-byte sun_path limit, and Ensure refuses the data dir before it ever
	// reaches the download this test is about.
	root, err := os.MkdirTemp("/tmp", "epg")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		return embedpg.Server{
			BinDir:   filepath.Join(root, "bin"),
			DataDir:  filepath.Join(root, "pg"),
			LogPath:  filepath.Join(root, "pg.log"),
			LockPath: filepath.Join(root, "pg.lock"),
			BaseURL:  srv.URL,
			Client:   srv.Client(),
		}
	}

	c := assert.NewAborting(t)
	_, err = resolveDSN(context.Background(), "embedded")
	c.Error(err, "want an error when the binary download fails, got nil")
	c.ErrorContains(err, "embedded database:", "error must be wrapped as an embedded-database failure, got: %v", err)
	c.Eq(int32(1), hits.Load(), "want exactly one artifact request, got")
}
