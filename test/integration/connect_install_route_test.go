// SPDX-License-Identifier: Apache-2.0

package integration_test

// Route-level admission for the two executor-install RPCs, on the MOUNTED
// Connect route.
//
// cmd/rafikid/connect_policy.go classifies InstallExecutor and UninstallExecutor
// policyChildScoped, and that classification is the difference between an agent
// installing an executor through a launcher it can reach and an agent being
// told "requires a user credential". Nothing pinned it: TestInstallFacesAdmitIdentically
// drives the two RPCs at the HANDLER with an identity injected straight into
// the context, so the mounted connectPolicyInterceptor — composed by
// connectControlRoute on both mounts — is not in that path at all. A
// reclassification of the two rows, or a mount that stops composing
// connectControlRoute, therefore left every test green while production
// refused the verb.
//
// The credential has to be a REAL per-child secret: authorizeControlProcedure
// admits a childScoped procedure only for ProvenanceChildToken, so the daemon's
// per-boot secret is refused there whether the row is childScoped or userOnly
// and cannot tell the two apart on its own. bootMCPChildDaemon's fake-claude
// child is where a per-child secret is observable — it arrives in the child's
// own environment (RAFIKI_MCP_TOKEN) beside the per-boot one
// (ANTHROPIC_AUTH_TOKEN), which is what lets this test pin the row from BOTH
// sides:
//
//	credential          ListModels (anyCaller)  InstallExecutor (childScoped)  ListExecutors (userOnly)
//	per-child secret    admitted                admitted                      REFUSED
//	per-boot secret     admitted                REFUSED                       REFUSED
//	user token          admitted                admitted                      admitted
//
// The middle column is the finding: a childScoped row admits the per-child
// secret and refuses the per-boot one. Reclassifying the row userOnly breaks
// the first cell; reclassifying it anyCaller breaks the second; dropping the
// interceptor from the mount changes both and the userOnly column too.
//
// This daemon has NO executor pool (bootMCPChildDaemon turns the executor
// plane off), so the install orchestrator is not wired: an ADMITTED install
// call reaches the handler and comes back Unavailable ("executor installer not
// wired"), while a refused one never reaches it and comes back
// PermissionDenied. That difference IS the policy decision, which is all this
// test claims — what the wire then does is pinned on a daemon that has the
// orchestrator, by TestConnectNewRPCsWired, and no ssh is needed for either.

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestConnectInstallExecutorRouteAdmission(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d, dumps := bootMCPChildDaemon(t)
	userToken := d.createMCPUser(t)

	// The caller: a real claude child holding a real per-child secret.
	child := mcpSpawnClaudeChild(t, mcpConnect(t, d.proxyURL, userToken), "connect-install-route")
	dump := waitClaudeDump(t, d, dumps, child)
	mcpToken := dump.envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", mcpToken, "the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")
	// The per-boot secret the same child also holds: the daemon's own proxy
	// bearer, which every child inherits and which names no child at all.
	bootToken := dump.envValue("ANTHROPIC_AUTH_TOKEN")
	ck.NotEq("", bootToken, "the spawned claude child's environment carries no ANTHROPIC_AUTH_TOKEN")
	ck.NotEq(mcpToken, bootToken, "the per-child secret and the per-boot secret must be different credentials")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := d.connectClient()

	// install / uninstall / listExecutors / listModels run one Control RPC
	// under token over the mounted route and return its error.
	install := func(token string) error {
		req := connect.NewRequest(&rafikiv1.InstallExecutorRequest{Target: "install-route.invalid"})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.InstallExecutor(ctx, req)
		return err
	}
	uninstall := func(token string) error {
		req := connect.NewRequest(&rafikiv1.UninstallExecutorRequest{Executor: "install-route-executor"})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.UninstallExecutor(ctx, req)
		return err
	}
	listModels := func(token string) error {
		req := connect.NewRequest(&rafikiv1.ListModelsRequest{})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.ListModels(ctx, req)
		return err
	}
	listExecutors := func(token string) error {
		req := connect.NewRequest(&rafikiv1.ListExecutorsRequest{})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.ListExecutors(ctx, req)
		return err
	}

	// admitted asserts the gate let the call through: it reached the handler,
	// which on this pool-less daemon can only answer its own "not yet wired"
	// Unavailable. The gate's refusal for an unadmitted child credential is
	// PermissionDenied and nothing else, so any other code is a fixture change
	// this test should be revisited for rather than passed over.
	admitted := func(where string, err error) {
		t.Helper()
		switch connect.CodeOf(err) {
		case connect.CodeUnavailable:
			return
		case connect.CodePermissionDenied:
			t.Errorf("%s refused a credential the policy table admits: %v", where, err)
		default:
			t.Errorf("%s = %v (code %v), want %v from the unwired handler on an ADMITTED call",
				where, err, connect.CodeOf(err), connect.CodeUnavailable)
		}
	}
	// refused asserts the gate itself said no, naming the credential it saw.
	refused := func(where string, err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s = %v (code %v), want %v", where, err, connect.CodeOf(err), connect.CodePermissionDenied)
		}
	}

	// ── the row under test: childScoped admits the per-child secret ──────────
	admitted("InstallExecutor as child", install(mcpToken))
	admitted("UninstallExecutor as child", uninstall(mcpToken))

	// ── the same row refuses a credential that names no child ────────────────
	// The per-boot secret is a real credential (the line above it proves it
	// resolves rather than 401ing) and the table does not admit it here: a
	// childScoped verb is for a child with subtree authority, and this one has
	// none. This is also what separates childScoped from anyCaller — an
	// anyCaller row would admit it.
	if err := listModels(bootToken); err != nil {
		t.Fatalf("ListModels (anyCaller) as the per-boot secret = %v, want it admitted: the credential did not resolve, so the refusal below would prove nothing", err)
	}
	refused("InstallExecutor as the per-boot secret", install(bootToken))
	refused("UninstallExecutor as the per-boot secret", uninstall(bootToken))

	// ── a user credential passes every row ──────────────────────────────────
	admitted("InstallExecutor as user", install(userToken))
	admitted("UninstallExecutor as user", uninstall(userToken))

	// ── and the child credential is refused by a userOnly row ───────────────
	// The other side of the classification: the SAME per-child secret is
	// refused on an operator verb, so a row reclassified userOnly would refuse
	// the two calls above, and a mount that lost the interceptor entirely would
	// admit this one. Neither can pass silently.
	refused("ListExecutors (userOnly) as child", listExecutors(mcpToken))
}
