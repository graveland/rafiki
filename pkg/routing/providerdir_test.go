// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// providersJSON is a slice of OpenRouter's /api/v1/providers, chosen for the
// names the lowercase-and-hyphenate guess gets wrong.
const providersJSON = `{"data":[
{"name":"OpenInference","slug":"open-inference"},
{"name":"AtlasCloud","slug":"atlas-cloud"},
{"name":"Mancer 2","slug":"mancer"},
{"name":"Z.AI","slug":"z-ai"},
{"name":"CoreWeave","slug":"coreweave"}]}`

// directoryServer serves body at every path and counts the requests.
func directoryServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testDirectory(t *testing.T) (*ProviderDirectory, *atomic.Int32) {
	t.Helper()
	srv, hits := directoryServer(t, http.StatusOK, providersJSON)
	return NewProviderDirectoryForTest(srv.Client(), srv.URL, slog.New(slog.DiscardHandler)), hits
}

// TestProviderDirectoryResolvesNamesToSlugs proves the directory maps display
// names, spaced spellings and slugs alike to the real slug, and refuses a name
// it does not list.
func TestProviderDirectoryResolvesNamesToSlugs(t *testing.T) {
	ck := assert.NewCollecting(t)
	d, hits := testDirectory(t)
	for in, want := range map[string]string{
		"OpenInference":  "open-inference",
		"Open Inference": "open-inference",
		"openinference":  "open-inference",
		"open-inference": "open-inference",
		"AtlasCloud":     "atlas-cloud",
		"Mancer 2":       "mancer",
		"Z.AI":           "z-ai",
	} {
		got, degraded, err := d.Resolve(context.Background(), in)
		ck.NoError(err, "Resolve(%q)", in)
		ck.False(degraded, "Resolve(%q) degraded", in)
		ck.Eq(want, got, "Resolve(%q)", in)
	}
	_, _, err := d.Resolve(context.Background(), "bogus")
	ck.ErrorIs(err, ErrUnknownProvider, "Resolve(bogus)")
	ck.Eq(int32(1), hits.Load(), "directory fetches")
	ck.Eq("OpenInference", d.Name("open-inference"), "Name")
	ck.Eq("new-host", d.Slug("New Host"), "Slug of an unlisted name falls back to the guess")
}

// TestProviderDirectorySlugIsLazyAndNonBlocking proves Slug answers with the
// guess before the first load, and that it kicks off the load itself.
func TestProviderDirectorySlugIsLazyAndNonBlocking(t *testing.T) {
	d, hits := testDirectory(t)
	ck := assert.NewAborting(t)
	ck.Eq(int32(0), hits.Load(), "a fresh directory must not fetch")
	first := d.Slug("OpenInference")
	ck.True(first == "openinference" || first == "open-inference", "Slug = %q", first)
	deadline := time.Now().Add(5 * time.Second)
	for d.Slug("OpenInference") != "open-inference" {
		if time.Now().After(deadline) {
			t.Fatal("Slug never resolved through the background load")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ck.Eq(int32(1), hits.Load(), "directory fetches")
}

// TestProviderDirectoryUnreachableDegradesToGuess proves an OpenRouter outage
// never blocks an operator: Resolve guesses and says so, and the failure backs
// off rather than refetching on every call.
func TestProviderDirectoryUnreachableDegradesToGuess(t *testing.T) {
	ck := assert.NewCollecting(t)
	srv, hits := directoryServer(t, http.StatusServiceUnavailable, "down")
	d := NewProviderDirectoryForTest(srv.Client(), srv.URL, slog.New(slog.DiscardHandler))
	for range 3 {
		got, degraded, err := d.Resolve(context.Background(), "Open Inference")
		ck.NoError(err, "Resolve")
		ck.True(degraded, "Resolve must report the guess")
		ck.Eq("open-inference", got, "Resolve")
	}
	ck.Eq(int32(1), hits.Load(), "a failed fetch must back off")
}

// TestIgnoredForResolvesEjectedDisplayNames proves a guard ejection, keyed by
// the display name a response reports, reaches provider.ignore as the real
// slug — the guess ("openinference") is a slug OpenRouter silently ignores.
func TestIgnoredForResolvesEjectedDisplayNames(t *testing.T) {
	d, _ := testDirectory(t)
	_, _, err := d.Resolve(context.Background(), "OpenInference") // load
	assert.NewAborting(t).NoError(err)
	g := testGuard()
	g.SetDirectory(d)
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "OpenInference"))
	}
	got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro")
	if len(got) != 1 || got[0] != "open-inference" {
		t.Fatalf("IgnoredFor = %v, want [open-inference]", got)
	}
}

// TestBanValidatesAgainstDirectory proves a ban names a provider OpenRouter
// knows: a display name is stored as its slug, an unknown one is refused.
func TestBanValidatesAgainstDirectory(t *testing.T) {
	ck := assert.NewCollecting(t)
	d, _ := testDirectory(t)
	g := testGuard()
	g.SetDirectory(d)
	rec, err := g.Ban(context.Background(), time.Now(), "AtlasCloud", 0, "")
	ck.NoError(err, "Ban(AtlasCloud)")
	ck.Eq("atlas-cloud", rec.Provider, "stored slug")

	_, err = g.Ban(context.Background(), time.Now(), "bogus", 0, "")
	ck.ErrorIs(err, ErrInvalidBan, "Ban(bogus)")
	ck.ErrorIs(err, ErrUnknownProvider, "Ban(bogus)")
}

// TestLiftPrefersTheStoredKey proves a ban stored under a slug OpenRouter
// never had can be lifted by that exact name without the directory
// redirecting the lift onto the provider's real ban.
func TestLiftPrefersTheStoredKey(t *testing.T) {
	ck := assert.NewAborting(t)
	g := testGuard()
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "openinference", 0, "") // no directory: stored as given
	ck.NoError(err)
	d, _ := testDirectory(t)
	g.SetDirectory(d)
	_, err = g.Ban(context.Background(), now, "OpenInference", 0, "")
	ck.NoError(err)

	ck.NoError(g.Lift(context.Background(), now, "openinference"), "Lift(openinference)")
	var left []string
	for _, r := range g.Ejected(now) {
		left = append(left, r.Provider)
	}
	ck.EqDeep([]string{"open-inference"}, left, "bans after lifting the dead one")
	ck.NoError(g.Lift(context.Background(), now, "OpenInference"), "Lift(OpenInference)")
	ck.Len(g.Ejected(now), 0, "bans after lifting both")
}
