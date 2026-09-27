// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
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
// a REAL UserTokenAuth wired for all three child credentials, and the real
// child-subtree source wired with a childstore that knows c_child as a
// childless tree (so every target the matrix names is a non-descendant). It
// returns a Connect client pointed at the mounted stack.
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
	// one composer both mounts share, plus the subtree source the childScoped
	// handlers consult. Kill is childScoped: its per-child refusal must come
	// from the REAL source (c_victim is not a descendant of c_child), or the
	// assertion would be proving that an unwired handler answers Unavailable.
	// HistoryLoader tolerates a nil pool because no admitted request below
	// reaches GetHistory.
	ctrl := &Controller{st: childstore.New(), cm: newChildManager()}
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		StartedAt: time.Now(),
	})
	srv := connectapi.NewServer(store.NewMessages(nil))
	srv.SetChildScopeSource(ctrl.childScopeFor)
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
// with operator authority — lift a budget, rewrite what every future agent
// loads or runs — so a child credential must be refused on every one of them.
// Kill is childScoped and appears here anyway: with the subtree source wired,
// its per-child refusal comes from the handler layer (c_victim is not a
// descendant), with the identical code the gate gives the other two child
// credentials — so the test pins the CODE across all three shapes, and the
// per-verb matrix (TestConnectChildScopedVerbs) pins where each refusal
// originates.
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
				assert.NewAborting(t).Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s with a %s credential = %v, want", verb.name, cred.name, err)
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
			assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "%s with a user credential = %v, want %v (the handler ran)", verb.name, err, connect.CodeUnavailable)
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

	// The child-scoped reads admit the per-child secret too: the conversation
	// and pymodule managers are unwired here, so CodeUnavailable again means
	// the gate let it through to the handler.
	getPymodule := connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: "m"})
	child(getPymodule.Header())
	_, err := client.GetPymodule(ctx, getPymodule)
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "GetPymodule with a per-child credential = %v, want", err)
	export := connect.NewRequest(&rafikiv1.ConversationExportRequest{ConversationId: "01a0e4a1-4a46-7866-b2a3-5dc7501689c8"})
	child(export.Header())
	_, err = client.ConversationExport(ctx, export)
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "ConversationExport with a per-child credential = %v, want", err)

	// No credential at all never reaches the Control service on this face:
	// the proxy middleware answers 401 before Connect runs, which the client
	// surfaces as Unauthenticated.
	_, err = client.ListModels(ctx, connect.NewRequest(&rafikiv1.ListModelsRequest{}))
	assert.NewAborting(t).Eq(connect.CodeUnauthenticated, connect.CodeOf(err), "ListModels with no credential = %v, want", err)
}

// TestProxyFaceConnectSpawnRefusesOperatorOnlyFieldToAChildToken and
// TestProxyFaceConnectSpawnAdmitsAUserTokenWithEveryOperatorOnlyFieldSet drive
// connectapi.Server.Spawn's operator-only-field guard (verbs.go's
// firstOperatorOnlySet) through the REAL identity chain this face serves: a
// bearer token resolves through UserTokenAuth to a server.Identity, the
// policy gate (childScoped) admits it, childScopeFor resolves a per-child
// token to a non-nil subtree scope, and only then does the handler run the
// field guard. Each link is pinned separately elsewhere (TestAuthorizeControlProcedure,
// connect_childscope_test.go, verbs_test.go's per-field matrix); these two
// are the proof the chain agrees end to end (W2A MINOR 3).
func TestProxyFaceConnectSpawnRefusesOperatorOnlyFieldToAChildToken(t *testing.T) {
	c := assert.NewCollecting(t)
	client := proxyFaceConnectRoute(t)
	req := connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd: "/tmp",
		Env: map[string]string{"K": "V"},
	})
	req.Header().Set("Authorization", "Bearer "+proxyChildToken)

	_, err := client.Spawn(context.Background(), req)
	c.Require().Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Spawn with env set (per-child token) = %v, want", err)
	c.StrContains(err.Error(), "env", "refusal")
}

func TestProxyFaceConnectSpawnAdmitsAUserTokenWithEveryOperatorOnlyFieldSet(t *testing.T) {
	client := proxyFaceConnectRoute(t)
	req := connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:                "/tmp",
		ConfigDir:          "/cfg",
		AppendSystemPrompt: "be nice",
		Thinking:           "high",
		NoSession:          true,
		ResumeSession:      "sess-1",
		ForkSession:        "sess-0",
		Extensions:         []string{"ext"},
		NoExtensions:       true,
		Verbose:            true,
		ExtraArgs:          []string{"--flag"},
		SkillsDirs:         []string{"/skills"},
		McpConfig:          "/mcp.json",
		Env:                map[string]string{"K": "V"},
		RecordRequests:     true,
		PassthroughAuth:    "on",
	})
	req.Header().Set("Authorization", "Bearer "+proxyUserToken)

	// The child lifecycle is unwired in this fixture, so a user credential
	// that clears the guard reaches CodeUnavailable, not success -- that's
	// "the handler ran", the same acceptable outcome
	// TestProxyFaceConnectAdmitsUserAndAnyCaller uses for the other operator
	// verbs. What this test refuses to accept is PermissionDenied.
	_, err := client.Spawn(context.Background(), req)
	assert.NewAborting(t).NotEq(connect.CodePermissionDenied, connect.CodeOf(err), "Spawn with every operator-only field set (user token) = %v, want anything but", err)
}

// TestControlPolicyTableCoversEveryProcedure is the completeness gate: every
// procedure of the generated Control service descriptor must have a table
// entry, and the table must hold no stale names. A new RPC therefore cannot
// land unclassified — the same class of bug as the missing
// UnimplementedControlHandler embed, caught here at test time instead of in
// production as a fail-open default.
func TestControlPolicyTableCoversEveryProcedure(t *testing.T) {
	c := assert.NewCollecting(t)
	svc := rafikiv1.File_rafiki_v1_control_proto.Services().ByName("Control")
	c.Require().NotNil(svc, "no Control service in the generated descriptor")
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
		c.False(!classified[name], "controlPolicyTable names %q, which is not a Control service procedure; drop the stale entry", name)
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
		got := policyFor(procedure)
		assert.NewCollecting(t).Eq(want, got, "policyFor(%q) = %v, want", procedure, got)
	}
}

// TestAuthorizeControlProcedure pins the identity × policy matrix at the
// interceptor level. Admit is exactly nil; refuse is exactly
// CodePermissionDenied. The empty Identity{} — what a bare per-boot token
// resolves to — is a child credential, not a user: it carries no provenance
// and must not inherit operator authority because its UserID is empty rather
// than borrowed.
//
// childScoped admits ONE child credential at the gate: a per-child secret
// names a child with subtree authority, so it passes and the HANDLER layer
// resolves its subtree (connectapi.ChildScopeSource — the per-verb matrix is
// TestConnectChildScopedVerbs). The other two child shapes name no child with
// authority — an attributed identity's ChildID is empty by construction, and
// the bare per-boot secret names nothing at all — so both stay refused here,
// exactly as in wave 0. Gate admission is not operator authority: a handler
// without the wired source must never serve a child caller the operator path,
// which the wiring test and the per-verb matrix both pin.
func TestAuthorizeControlProcedure(t *testing.T) {
	c := assert.NewCollecting(t)
	const (
		userOnly   = controlProcedurePrefix + "PutPreset"
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
		// A nil identity and a real user credential pass every policy. A
		// per-child secret passes only the anyCaller and childScoped verbs —
		// on childScoped it has yet to name a legal target, which the handler
		// layer enforces. The two unattributed child shapes pass only
		// anyCaller.
		trusted := id.id == nil || id.id.IsUserCredential()
		childToken := id.id != nil && id.id.Via == server.ProvenanceChildToken
		for _, tc := range []struct {
			procedure string
			wantAdmit bool
		}{
			{anyCaller, true},
			{userOnly, trusted},
			{childScope, trusted || childToken},
		} {
			err := authorizeControlProcedure(ctx, tc.procedure)
			if tc.wantAdmit {
				c.NoError(err, "%s, %s: %v, want admitted", id.name, tc.procedure, err)
				continue
			}
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s, %s: %v, want", id.name, tc.procedure, err)
			c.StrContains(err.Error(), tc.procedure, "%s, %s: refusal %q does not name the procedure", id.name, tc.procedure, err)
		}
	}
}

// TestServeConnectUDSRefusesChildCredentialsOnOperatorVerbs pins the OTHER
// mount of the Control service: the local socket composes the same policy
// gate behind its optional identity interceptor, and the same child-subtree
// source behind the gate. Kill is childScoped, so the three child credentials
// are refused from two different layers with one identical code — the
// per-boot shapes by the gate (they name no child with authority), the
// per-child secret by the handler's subtree check (c_victim is not a
// descendant of c_child) — while a credential-LESS caller, the socket's own
// trust model, still reaches the handlers. The two planes must agree: one
// that refuses a child credential and one that serves it would answer the
// same operator differently depending on which route the request took.
func TestServeConnectUDSRefusesChildCredentialsOnOperatorVerbs(t *testing.T) {
	c := assert.NewCollecting(t)
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

	ctrl := &Controller{st: childstore.New(), cm: newChildManager()}
	ctrl.st.Insert(&childstore.Session{
		ChildID: "c_child", Status: protocol.StatusIdle, Kind: protocol.KindFundi,
		StartedAt: time.Now(),
	})
	srv := connectapi.NewServer(nil)
	srv.SetChildScopeSource(ctrl.childScopeFor)
	sock := filepath.Join(shortTempDir(t), "s")
	ln, err := serveConnectUDS(ctx, srv, auth, sock)
	c.Require().NoError(err, "serveConnectUDS")
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
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "Kill over UDS with a %s credential = %v, want", cred.name, err)
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

// TestScopeForNilIsUnreachableOnTheRealProxyFace is the load-bearing half of
// scopeFor's nil-is-local-trust proof (connect_adapters_test.go's
// TestScopeForNilIsUnreachableOffTheUDS is the other half, and stays --
// there is compositional value in pinning the auth wrapper alone). That test
// REBUILDS the stack: it calls h.Mount with its own wrap closure, which
// proves UserTokenAuth.Middleware itself refuses an unauthenticated caller,
// but not that proxy.go still WIRES that middleware onto the Control mount it
// actually serves. server.Handler.Mount treats a nil wrap as pass-through
// (pkg/server/handler.go), so if proxy.go ever dropped or reordered its wrap
// argument, the real proxy face would admit an anonymous Control call --
// scopeFor(nil) would then read that as local trust and grant ScopeAll over
// every user's conversations from the network face, and the rebuilt-stack
// test would still pass, none the wiser.
//
// This test instead calls startProxyFace -- the same constructor
// cmd/rafikid's main() calls to build the face it actually serves -- and
// drives ConversationSearch, a userOnly scopeFor-backed verb, through it with
// no credential at all. It fails the moment that composition stops
// authenticating.
func TestScopeForNilIsUnreachableOnTheRealProxyFace(t *testing.T) {
	t.Setenv("RAFIKI_PROXY_LISTEN", "127.0.0.1:0")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	c := assert.NewAborting(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	face, err := startProxyFace(ctx, faceOptions{
		Logger:   slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry(),
	})
	c.NoError(err, "startProxyFace")
	defer face.Close(ctx)

	client := rafikiv1connect.NewControlClient(http.DefaultClient, face.URL)
	_, err = client.ConversationSearch(context.Background(),
		connect.NewRequest(&rafikiv1.ConversationSearchRequest{}))
	c.Error(err, "ConversationSearch with no credential on the real proxy face succeeded, want a refusal")
	switch code := connect.CodeOf(err); code {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
	default:
		t.Fatalf("ConversationSearch with no credential err = %v (code %v), want Unauthenticated or PermissionDenied", err, code)
	}
}
