// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// A provider with no via_executor dials directly: nil transport, no error.
func TestRelayTransportNilWithoutViaExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	p := providers.Provider{Name: "anthropic", Kind: providers.KindAnthropic}
	rt, err := relayTransport(p, nil)
	c.NoError(err, "relayTransport")
	c.Nil(rt, "rt")
}

// A via_executor provider whose selector matches no live executor advertising
// the proxy must FAIL THE TURN, never fall through to a direct dial: that
// would make a misconfigured relay look like it worked while the daemon
// quietly dialed base_url from the SERVER, which either refuses or reaches
// something unrelated.
func TestRelayTransportNoMatchingExecutorErrors(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := execpool.New(relayFakeStore{})
	p := providers.Provider{
		Name:    "vmlx",
		Kind:    providers.KindAnthropic,
		BaseURL: "http://localhost:8005",
		ViaExecutor: &providers.ViaExecutor{
			Selector: "role=workstation",
			Proxy:    "vmlx",
		},
	}

	rt, err := relayTransport(p, pool)
	c.Require().Error(err, "relayTransport succeeded against an empty pool")
	c.Require().Nil(rt, "rt")
	c.StrContains(err.Error(), "vmlx", "error")
	c.StrContains(err.Error(), "role=workstation", "error")
}

// A live executor matching the selector but advertising NO proxies must still
// refuse — the fix here is a --proxy flag, not a label, and the error should
// say so.
func TestRelayTransportErrorNamesTheProxy(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := joinLiveRelayExecutor(t, executors.Executor{
		ID: "exec-1", Enabled: true,
		Labels: map[string]string{"role": "workstation"},
	}, nil /* no proxies advertised */)

	p := providers.Provider{
		Name: "vmlx",
		Kind: providers.KindAnthropic,
		ViaExecutor: &providers.ViaExecutor{
			Selector: "role=workstation",
			Proxy:    "vmlx",
		},
	}

	rt, err := relayTransport(p, pool)
	c.Require().Error(err, "relayTransport succeeded against an executor advertising no proxies")
	c.Require().Nil(rt, "rt")
	c.StrContains(err.Error(), "vmlx", "error")
}

// A live executor matching the selector AND advertising the proxy: relayTransport
// succeeds with a non-nil transport. NewProxyTransport is lazy — it opens no
// stream until the first RoundTrip — so this assertion never touches the
// network.
func TestRelayTransportSucceedsWhenExecutorAdvertisesProxy(t *testing.T) {
	c := assert.NewAborting(t)
	pool := joinLiveRelayExecutor(t, executors.Executor{
		ID: "exec-1", Enabled: true,
		Labels: map[string]string{"role": "workstation"},
	}, []string{"vmlx"})

	p := providers.Provider{
		Name: "vmlx",
		Kind: providers.KindAnthropic,
		ViaExecutor: &providers.ViaExecutor{
			Selector: "role=workstation",
			Proxy:    "vmlx",
		},
	}

	rt, err := relayTransport(p, pool)
	c.NoError(err, "relayTransport")
	c.NotNil(rt, "rt = nil, want a relay transport")
}

// providerSenders must resolve only the providers this spawn's MODEL can
// actually reach — its own provider plus that provider's configured Fallback
// chain — not every via_executor provider in the whole registry. A provider
// outside that reachable set with no live executor must not fail a spawn for
// an unrelated model.
func TestProviderSendersSkipsUnreachableViaExecutorProvider(t *testing.T) {
	c := assert.NewCollecting(t)
	set := &providers.Set{
		DefaultProvider: "openrouter",
		Providers: map[string]providers.Provider{
			"openrouter": {Name: "openrouter", Kind: providers.KindAnthropicOpenRouter},
			"vmlx": {
				Name: "vmlx",
				Kind: providers.KindAnthropic,
				ViaExecutor: &providers.ViaExecutor{
					Selector: "role=laptop",
					Proxy:    "vmlx",
				},
			},
		},
	}
	pool := execpool.New(relayFakeStore{}) // empty: vmlx advertises no live executor

	senders, err := providerSenders(set, pool, "openrouter/deepseek-v4-pro", "", nil)
	c.Require().NoError(err, "providerSenders")
	_, ok := senders["vmlx"]
	c.False(ok, `senders contains "vmlx", want it omitted (no live executor, and unreachable by this model)`)
}

// A via_executor provider that IS in the reachable set (the model's own
// provider) must still fail the spawn when it has no live executor — the
// scoping change must not weaken that case.
func TestProviderSendersFailsWhenModelsOwnProviderUnavailable(t *testing.T) {
	set := &providers.Set{
		DefaultProvider: "vmlx",
		Providers: map[string]providers.Provider{
			"vmlx": {
				Name: "vmlx",
				Kind: providers.KindAnthropic,
				ViaExecutor: &providers.ViaExecutor{
					Selector: "role=laptop",
					Proxy:    "vmlx",
				},
			},
		},
	}
	pool := execpool.New(relayFakeStore{})

	_, err := providerSenders(set, pool, "vmlx/qwen", "", nil)
	assert.NewAborting(t).Error(err, "providerSenders succeeded for a model whose own provider has no live executor")
}

// ─── ChatGPT-login senders ──────────────────────────────────────────────────

// fakeSenderCreds is a providerSenderCreds that records the owner of every
// sender it is asked to build for.
//
// It exists because a built sender is a pkg/llm value whose token source cannot
// be read back out, so the property that matters (owner A's sender spends A's
// bearer, never B's) is only observable at this seam: which owner each build
// asked for, and that the source it was handed answers for that owner alone.
type fakeSenderCreds struct {
	mu    sync.Mutex
	avail bool
	why   string
	built []credKey
}

func (f *fakeSenderCreds) Available() (bool, string) { return f.avail, f.why }

func (f *fakeSenderCreds) TokenSourceFor(owner, provider string) llm.TokenSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.built = append(f.built, credKey{owner: owner, provider: provider})
	return ownerBearer{owner: owner, provider: provider}
}

func (f *fakeSenderCreds) ModelFactsFor(_, _ string) llm.ModelFacts { return nil }

// builtFor is every (owner, provider) a sender was built for, in order.
func (f *fakeSenderCreds) builtFor() []credKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]credKey(nil), f.built...)
}

// ownerBearer is one owner's token source. Token answers with a bearer naming
// the owner it was built for, so an owner leak is visible as a bearer.
type ownerBearer struct{ owner, provider string }

func (s ownerBearer) Token(context.Context, bool) (string, http.Header, error) {
	return "bearer-" + s.owner, nil, nil
}

// Two owners spawning on the same ChatGPT provider must get two senders, each
// bound to its own owner's credential. A shared sender would let one user's
// turns spend another user's subscription, and would share the sender's cookie
// jar with it.
func TestProviderSendersOwnerIsolation(t *testing.T) {
	ck := assert.NewCollecting(t)
	set := chatGPTProviderSet()
	creds := &fakeSenderCreds{avail: true}
	const model = chatGPTTestProvider + "/gpt-6.1-sol"

	sendersA, err := providerSenders(set, nil, model, ownerA, creds)
	ck.Require().NoError(err, "providerSenders(owner A)")
	sendersB, err := providerSenders(set, nil, model, ownerB, creds)
	ck.Require().NoError(err, "providerSenders(owner B)")

	senderA, okA := sendersA[chatGPTTestProvider]
	senderB, okB := sendersB[chatGPTTestProvider]
	ck.Require().True(okA && okB, "a ChatGPT-login provider with an owner and a usable credential got no sender")
	ck.False(senderA == senderB, "both owners were handed the same sender, so one's credential (and cookie jar) serves the other")

	// Each build asked for its OWN owner's token source, once.
	ck.EqDeep([]credKey{
		{owner: ownerA, provider: chatGPTTestProvider},
		{owner: ownerB, provider: chatGPTTestProvider},
	}, creds.builtFor(), "TokenSourceFor calls, in order")

	// And the sources it handed out answer for their own owner only.
	ctx := context.Background()
	bearerA, _, err := ownerBearer{owner: ownerA, provider: chatGPTTestProvider}.Token(ctx, false)
	ck.Require().NoError(err, "owner A's token")
	bearerB, _, err := ownerBearer{owner: ownerB, provider: chatGPTTestProvider}.Token(ctx, false)
	ck.Require().NoError(err, "owner B's token")
	ck.Eq("bearer-"+ownerA, bearerA, "owner A's bearer")
	ck.Eq("bearer-"+ownerB, bearerB, "owner B's bearer")
	ck.False(bearerB == "bearer-"+ownerA, "owner B's token source answered with owner A's bearer")
}

// Without an owner (and without usable credential machinery) a ChatGPT-login
// provider contributes NO sender. Nothing is built from a daemon-wide
// credential, because the kind has none: the first turn fails loudly on the
// missing sender rather than billing an account nobody chose.
func TestProviderSendersNoOwnerNoChatGPTSender(t *testing.T) {
	ck := assert.NewCollecting(t)
	set := chatGPTProviderSet()
	const model = chatGPTTestProvider + "/gpt-6.1-sol"

	for _, tc := range []struct {
		name  string
		owner string
		creds providerSenderCreds
	}{
		{"no owner", "", &fakeSenderCreds{avail: true}},
		{"no credential machinery", ownerA, nil},
		{"unavailable", ownerA, &fakeSenderCreds{avail: false, why: "set RAFIKI_SECRETS_KEY"}},
	} {
		senders, err := providerSenders(set, nil, model, tc.owner, tc.creds)
		ck.Require().NoError(err, "providerSenders(%s)", tc.name)
		ck.NotHasKey(senders, chatGPTTestProvider, "providerSenders(%s) built a ChatGPT sender anyway", tc.name)
	}

	// The owner+credential case is the control: the same call DOES build one.
	senders, err := providerSenders(set, nil, model, ownerA, &fakeSenderCreds{avail: true})
	ck.Require().NoError(err, "providerSenders(control)")
	ck.HasKey(senders, chatGPTTestProvider, "providerSenders built nothing for an owner with usable credentials")
}

// ─── helpers ────────────────────────────────────────────────────────────────

// relayFakeStore is a minimal executors.Store. Nothing in these tests enrolls
// through it directly (see enrollingStore below); an empty-pool test just
// needs SOME store to construct a Pool.
type relayFakeStore struct{}

func (relayFakeStore) MintToken(context.Context, executors.NewToken) (string, error) { return "", nil }
func (relayFakeStore) Enroll(context.Context, string, map[string]string) (executors.Executor, string, error) {
	return executors.Executor{}, "", nil
}
func (relayFakeStore) Create(context.Context, executors.NewToken) (executors.Executor, string, error) {
	return executors.Executor{}, "", nil
}
func (relayFakeStore) Authenticate(context.Context, string) (executors.Executor, error) {
	return executors.Executor{}, nil
}
func (relayFakeStore) Get(context.Context, string) (executors.Executor, error) {
	return executors.Executor{}, nil
}
func (relayFakeStore) List(context.Context) ([]executors.Executor, error) { return nil, nil }
func (relayFakeStore) SetLabels(context.Context, string, map[string]string, []string) (executors.Executor, error) {
	return executors.Executor{}, nil
}
func (relayFakeStore) SetEnabled(context.Context, string, bool) error { return nil }
func (relayFakeStore) Delete(context.Context, string) error           { return nil }
func (relayFakeStore) Annotate(context.Context, string, map[string]string, []string) error {
	return nil
}
func (relayFakeStore) TouchSeen(context.Context, string) error { return nil }

var _ executors.Store = relayFakeStore{}

// enrollingStore hands back a fixed Executor row on Enroll regardless of the
// token presented — enough to admit one fake executor into a Pool, with the
// labels a test wants to select on, and no database.
type enrollingStore struct {
	relayFakeStore
	exec executors.Executor
}

func (s enrollingStore) Enroll(context.Context, string, map[string]string) (executors.Executor, string, error) {
	return s.exec, "cred", nil
}

// relayDescribeHandler answers Describe with a fixed proxy list and nothing
// else — just enough for Pool.handleConn's admission Describe call and the
// occasional Health poll during a short-lived test.
type relayDescribeHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	executorID string
	proxies    []string
}

func (h *relayDescribeHandler) Describe(
	context.Context, *connect.Request[executorpb.DescribeRequest],
) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId: h.executorID,
		Proxies:    h.proxies,
	}), nil
}

func (h *relayDescribeHandler) Health(
	context.Context, *connect.Request[executorpb.HealthRequest],
) (*connect.Response[executorpb.HealthResponse], error) {
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

// joinLiveRelayExecutor admits one fake executor into a fresh Pool over a real
// unix-socket reverse-dial — execpool.Connect on one end, Pool.Serve on the
// other — exactly the production path a local executor uses, so nothing about
// admission needs re-testing here. exec.Labels is what a via_executor selector
// matches against; proxies is what the fake executor self-reports in Describe.
func joinLiveRelayExecutor(t *testing.T, exec executors.Executor, proxies []string) *execpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)

	// macOS TempDir paths often exceed the unix-socket path length limit;
	// /tmp directly with a short name avoids it.
	dir, err := os.MkdirTemp("/tmp", "relay-")
	c.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	c.NoError(err)
	t.Cleanup(func() { _ = ln.Close() })

	pool := execpool.New(enrollingStore{exec: exec})
	go func() { _ = pool.Serve(ln) }()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, h := executorpbconnect.NewExecutorServiceHandler(&relayDescribeHandler{
		executorID: exec.ID,
		proxies:    proxies,
	})
	go func() {
		_ = execpool.Connect(ctx, execpool.ConnectOptions{
			SocketPath:  sock,
			EnrollToken: "t_test",
			Handler:     h,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pool.Live()) == 1 {
			return pool
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fake executor never joined the pool")
	return nil
}
