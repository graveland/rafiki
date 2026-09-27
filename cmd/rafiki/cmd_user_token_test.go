package main

// The stale-token dead end and the central advice that names the way out.
// A profile token that no longer resolves refuses every verb that presents
// it, so main()'s error print (withTokenAdvice) appends the recovery. The
// trigger is the Connect code — CodeUnauthenticated, what the daemon's
// optionalIdentityInterceptor and UserTokenAuth both answer an unknown
// credential with — never a string match.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// TestWithTokenAdvice pins the central advice text: it names the profile's
// token file and the recovery, fires on the Unauthenticated refusal only, and
// passes an unrelated failure through untouched.
func TestWithTokenAdvice(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	dir := t.TempDir()
	writeTokenedProfile(t, filepath.Join(dir, "unused.sock"), "rfk_tok")
	_, err := resolveProfile(&cobra.Command{})
	c.NoError(err, "resolve profile")

	authErr := connect.NewError(connect.CodeUnauthenticated, errors.New("invalid auth token"))
	got := withTokenAdvice(authErr)

	msg := got.Error()
	for _, want := range []string{
		profile.TokenFile("it"),
		"delete it",
		"recover on the daemon host with: rafikid user create <name>",
		"invalid auth token", // the original reason stays visible
	} {
		c.StrContains(msg, want, "advice message")
	}
	c.ErrorIs(got, authErr, "the original error is no longer wrapped")

	// An unrelated failure passes through untouched — including the Connect
	// codes that merely look like auth (a store outage is Unavailable, a dead
	// daemon is Unavailable, permission is Denied).
	plain := errors.New("boom")
	c.Eq("boom", withTokenAdvice(plain).Error(), "unrelated error was rewritten: %v", withTokenAdvice(plain))
	for _, code := range []connect.Code{connect.CodeUnavailable, connect.CodePermissionDenied, connect.CodeInternal} {
		if got := withTokenAdvice(connect.NewError(code, errors.New("x"))); !strings.HasSuffix(got.Error(), "x") {
			t.Fatalf("code %v was rewritten: %v", code, got)
		}
	}

	// No resolved profile (e.g. the failure happened before mustProfile) —
	// formatting must not bootstrap one, and the error passes through.
	resetProfileCache()
	if got := withTokenAdvice(authErr); got.Error() != authErr.Error() {
		t.Fatalf("advice appeared without a resolved profile: %v", got)
	}
}
