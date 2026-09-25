// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
)

// Test fixtures for the Connect provenance gate: every credential the proxy
// face accepts, and the constants the tests address them by.
const (
	proxyUserToken  = "rfk_user_token_u1"
	proxyBootToken  = "rfk_per_boot_child_secret"
	proxyChildToken = "rfk_per_child_secret_c_child"
)

// proxyFaceConnectRoute mounts the Control service the way proxy.go mounts it
// on the proxy face — connectControlRoute behind tokenAuth.Middleware — with
// a REAL UserTokenAuth wired for all three child credentials. It returns a
// Connect client pointed at the mounted stack.
//
// The route composition here goes through connectControlRoute, the same
// composer proxy.go and connect_uds.go mount through, so this test cannot
// drift from the stack the daemon actually serves.
func proxyFaceConnectRoute(t *testing.T) rafikiv1connect.ControlClient {
	t.Helper()

	auth := server.NewUserTokenAuth(
		fakeUserStore{token: proxyUserToken, id: users.Identity{UserID: "u1", Username: "brent"}},
		proxyBootToken,
		server.DefaultAuthCacheTTL,
	)
	auth.SetChildTokenLookup(func(token string) (childID, ownerUserID string, ok bool) {
		if token == proxyChildToken {
			return "c_child", "u1", true
		}
		return "", "", false
	})
	auth.SetChildOwnerLookup(func(childID string) (string, bool) {
		if childID == "c_child" {
			return "u1", true
		}
		return "", false
	})

	// The same construction proxy.go uses — through connectControlRoute, the
	// one composer both mounts share. HistoryLoader tolerates a nil pool
	// because no admitted request below reaches GetHistory.
	srv := connectapi.NewServer(store.NewMessages(nil))
	h := &server.Handler{}
	h.ControlPath, h.Control = connectControlRoute(srv)

	mux := http.NewServeMux()
	h.Mount(mux, auth.Middleware)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return rafikiv1connect.NewControlClient(ts.Client(), ts.URL)
}

// childCredentials enumerates the three child-shaped credentials the proxy
// face resolves, each of which must fail every userOnly verb:
//
//   - per-child secret: resolves to ProvenanceChildToken, the credential a
//     script child will hold (wave 1 grants it scoped verbs).
//   - per-boot + session: ProvenanceChildAttributed — the shared boot secret
//     plus X-Rafiki-Session, what every claude child's ANTHROPIC_AUTH_TOKEN
//     resolves to when its session header is present.
//   - per-boot bare: the empty Identity{} — the shared boot secret with no
//     session header, which is a child credential, never a user.
func childCredentials() []struct {
	name string
	set  func(h http.Header)
} {
	return []struct {
		name string
		set  func(h http.Header)
	}{
		{"per-child secret", func(h http.Header) {
			h.Set("Authorization", "Bearer "+proxyChildToken)
		}},
		{"per-boot+session", func(h http.Header) {
			h.Set("Authorization", "Bearer "+proxyBootToken)
			h.Set("X-Rafiki-Session", "c_child")
		}},
		{"per-boot bare", func(h http.Header) {
			h.Set("Authorization", "Bearer "+proxyBootToken)
		}},
	}
}

// childUsableVerbs are the verbs the 0.1 brief names: each acts (or answers)
// with operator authority — kill another tree, lift a budget, rewrite what
// every future agent loads or runs — so a child credential must be refused
// on every one of them.
func childUsableVerbs(client rafikiv1connect.ControlClient) []struct {
	name   string
	invoke func(ctx context.Context, set func(h http.Header)) error
} {
	return []struct {
		name   string
		invoke func(ctx context.Context, set func(h http.Header)) error
	}{
		{"Kill", func(ctx context.Context, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_victim"})
			set(req.Header())
			_, err := client.Kill(ctx, req)
			return err
		}},
		{"SetBudget", func(ctx context.Context, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.SetBudgetRequest{ChildId: "c_victim", MaxCost: 100})
			set(req.Header())
			_, err := client.SetBudget(ctx, req)
			return err
		}},
		{"PutPreset", func(ctx context.Context, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.PutPresetRequest{Preset: &rafikiv1.PresetRow{Name: "p"}})
			set(req.Header())
			_, err := client.PutPreset(ctx, req)
			return err
		}},
		{"PutPymodule", func(ctx context.Context, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.PutPymoduleRequest{Name: "m"})
			set(req.Header())
			_, err := client.PutPymodule(ctx, req)
			return err
		}},
		{"UpsertSkill", func(ctx context.Context, set func(h http.Header)) error {
			req := connect.NewRequest(&rafikiv1.UpsertSkillRequest{Namespace: "user", Name: "s", Body: "b"})
			set(req.Header())
			_, err := client.UpsertSkill(ctx, req)
			return err
		}},
	}
}

// TestProxyFaceConnectRefusesChildCredentials is the wire-level proof of the
// provenance gate: every child credential the proxy face resolves is refused
// with PermissionDenied on every operator verb, before any handler runs.
func TestProxyFaceConnectRefusesChildCredentials(t *testing.T) {
	client := proxyFaceConnectRoute(t)
	ctx := context.Background()

	for _, cred := range childCredentials() {
		for _, verb := range childUsableVerbs(client) {
			t.Run(cred.name+"/"+verb.name, func(t *testing.T) {
				err := verb.invoke(ctx, cred.set)
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("%s with a %s credential = %v, want %v",
						verb.name, cred.name, err, connect.CodePermissionDenied)
				}
			})
		}
	}
}

// The control group: the gate must refuse child credentials specifically,
// not everyone. A real user credential reaches the (here unwired) handlers,
// and an anyCaller verb stays reachable by a child credential.
func TestProxyFaceConnectAdmitsUserAndAnyCaller(t *testing.T) {
	client := proxyFaceConnectRoute(t)
	ctx := context.Background()

	// A user credential passes the gate and reaches the handler layer. Every
	// manager below is unwired in this fixture, so the expected outcome is
	// CodeUnavailable — the handler ran. PermissionDenied here would mean the
	// gate rejects users too.
	user := func(h http.Header) { h.Set("Authorization", "Bearer "+proxyUserToken) }
	for _, verb := range childUsableVerbs(client) {
		t.Run("user/"+verb.name, func(t *testing.T) {
			err := verb.invoke(ctx, user)
			if connect.CodeOf(err) != connect.CodeUnavailable {
				t.Fatalf("%s with a user credential = %v, want %v (the handler ran)",
					verb.name, err, connect.CodeUnavailable)
			}
		})
	}

	// anyCaller verbs stay callable by a child credential: read-only,
	// non-scoped, and the only verbs wave 0 grants a child. ListModels'
	// lister is unwired here, so CodeUnavailable again means "the handler
	// ran".
	child := childCredentials()[0].set
	req := connect.NewRequest(&rafikiv1.ListModelsRequest{})
	child(req.Header())
	if _, err := client.ListModels(ctx, req); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("ListModels with a per-child credential = %v, want %v (anyCaller)", err, connect.CodeUnavailable)
	}

	// No credential at all never reaches the Control service on this face:
	// the proxy middleware answers 401 before Connect runs, which the client
	// surfaces as Unauthenticated.
	if _, err := client.ListModels(ctx, connect.NewRequest(&rafikiv1.ListModelsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListModels with no credential = %v, want %v", err, connect.CodeUnauthenticated)
	}
}

// TestControlPolicyTableCoversEveryProcedure is the completeness gate: every
// procedure of the generated Control service descriptor must have a table
// entry, and the table must hold no stale names. A new RPC therefore cannot
// land unclassified — the same class of bug as the missing
// UnimplementedControlHandler embed, caught here at test time instead of in
// production as a fail-open default.
func TestControlPolicyTableCoversEveryProcedure(t *testing.T) {
	svc := rafikiv1.File_rafiki_v1_control_proto.Services().ByName("Control")
	if svc == nil {
		t.Fatal("no Control service in the generated descriptor")
	}
	methods := svc.Methods()
	classified := make(map[string]bool, methods.Len())
	for i := 0; i < methods.Len(); i++ {
		name := string(methods.Get(i).Name())
		classified[name] = true
		policy, ok := controlPolicyTable[name]
		if !ok {
			t.Errorf("Control.%s has no entry in controlPolicyTable: classify it before the RPC ships, or it defaults (fail-closed) to userOnly unreviewed", name)
			continue
		}
		switch policy {
		case policyUserOnly, policyAnyCaller, policyChildScoped:
		default:
			t.Errorf("Control.%s has an out-of-range policy %d", name, policy)
		}
	}
	for name := range controlPolicyTable {
		if !classified[name] {
			t.Errorf("controlPolicyTable names %q, which is not a Control service procedure; drop the stale entry", name)
		}
	}
}

// TestPolicyForDefaultsClosed pins the fail-closed resolution: a procedure
// outside the service and an unknown Control RPC are both userOnly, so a
// gap in the table can only over-refuse, never admit.
func TestPolicyForDefaultsClosed(t *testing.T) {
	for procedure, want := range map[string]controlPolicy{
		controlProcedurePrefix + "Kill":        policyChildScoped,
		controlProcedurePrefix + "ListModels":  policyAnyCaller,
		controlProcedurePrefix + "PutPymodule": policyUserOnly,
		controlProcedurePrefix + "NoSuchRPC":   policyUserOnly,
		"/other.v1.Service/Do":                 policyUserOnly,
		"not-a-procedure":                      policyUserOnly,
	} {
		if got := policyFor(procedure); got != want {
			t.Errorf("policyFor(%q) = %v, want %v", procedure, got, want)
		}
	}
}

// TestAuthorizeControlProcedure pins the identity × policy matrix at the
// interceptor level. Admit is exactly nil; refuse is exactly
// CodePermissionDenied. The empty Identity{} — what a bare per-boot token
// resolves to — is a child credential, not a user: it carries no provenance
// and must not inherit operator authority because its UserID is empty rather
// than borrowed.
func TestAuthorizeControlProcedure(t *testing.T) {
	const (
		userOnly   = controlProcedurePrefix + "SetBudget"
		anyCaller  = controlProcedurePrefix + "ListModels"
		childScope = controlProcedurePrefix + "Kill"
	)

	identities := []struct {
		name string
		id   *server.Identity
	}{
		{"nil identity (UDS local trust)", nil},
		{"user credential", &server.Identity{UserID: "u1", Username: "brent", Via: server.ProvenanceUser}},
		{"per-child secret", &server.Identity{UserID: "u1", ChildID: "c_child", Via: server.ProvenanceChildToken}},
		{"per-boot + session", &server.Identity{UserID: "u1", Via: server.ProvenanceChildAttributed}},
		{"bare per-boot (empty Identity{})", &server.Identity{}},
	}

	for _, id := range identities {
		ctx := context.Background()
		if id.id != nil {
			ctx = server.WithIdentity(ctx, id.id)
		}
		// A nil identity and a real user credential pass every policy. A child
		// credential passes only the anyCaller verb; userOnly and childScoped
		// (enforced as userOnly in wave 0 — wave 1 changes this expectation for
		// ProvenanceChildToken identities alone) refuse it.
		trusted := id.id == nil || id.id.IsUserCredential()
		for _, tc := range []struct {
			procedure string
			wantAdmit bool
		}{
			{anyCaller, true},
			{userOnly, trusted},
			{childScope, trusted},
		} {
			err := authorizeControlProcedure(ctx, tc.procedure)
			if tc.wantAdmit {
				if err != nil {
					t.Errorf("%s, %s: %v, want admitted", id.name, tc.procedure, err)
				}
				continue
			}
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Errorf("%s, %s: %v, want %v", id.name, tc.procedure, err, connect.CodePermissionDenied)
			}
			if !strings.Contains(err.Error(), tc.procedure) {
				t.Errorf("%s, %s: refusal %q does not name the procedure", id.name, tc.procedure, err)
			}
		}
	}
}

// TestServeConnectUDSRefusesChildCredentialsOnOperatorVerbs pins the OTHER
// mount of the Control service: the local socket composes the same policy
// gate behind its optional identity interceptor, so a child credential
// presented to connect.sock is refused on operator verbs exactly as it is on
// the proxy face — while a credential-LESS caller, the socket's own trust
// model, still reaches the handlers. The two planes must agree: one that
// refuses a child credential and one that serves it would answer the same
// operator differently depending on which route the request took.
func TestServeConnectUDSRefusesChildCredentialsOnOperatorVerbs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	auth := server.NewUserTokenAuth(stubUserStore{token: proxyUserToken, id: users.Identity{UserID: "u1", Username: "brent"}}, proxyBootToken, time.Minute)
	auth.SetChildTokenLookup(func(token string) (childID, ownerUserID string, ok bool) {
		if token == proxyChildToken {
			return "c_child", "u1", true
		}
		return "", "", false
	})
	auth.SetChildOwnerLookup(func(childID string) (string, bool) {
		if childID == "c_child" {
			return "u1", true
		}
		return "", false
	})

	srv := connectapi.NewServer(nil)
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	if err != nil {
		t.Fatalf("serveConnectUDS: %v", err)
	}
	defer ln.Close()

	client := rafikiv1connect.NewControlClient(udsHTTPClient(sock), "http://connect.rafiki.invalid")
	kill := func(set func(h http.Header)) error {
		req := connect.NewRequest(&rafikiv1.KillRequest{ChildId: "c_victim"})
		set(req.Header())
		_, err := client.Kill(ctx, req)
		return err
	}

	// The two child credentials the optional identity interceptor can
	// resolve from headers.
	for _, cred := range childCredentials() {
		err := kill(cred.set)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("Kill over UDS with a %s credential = %v, want %v",
				cred.name, err, connect.CodePermissionDenied)
		}
	}

	// No credential: the socket decided admission, and the call reaches the
	// handler layer (unwired here — Unavailable, never refused).
	if err := kill(func(h http.Header) {}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("Kill over UDS with no credential = %v, want %v (handler reached)", err, connect.CodeUnavailable)
	}

	// A user credential resolves and passes the gate the same way.
	if err := kill(func(h http.Header) { h.Set("Authorization", "Bearer "+proxyUserToken) }); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("Kill over UDS with a user credential = %v, want %v (handler reached)", err, connect.CodeUnavailable)
	}
}
