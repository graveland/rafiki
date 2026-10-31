// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
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
	"go.graveland.dev/rafiki/pkg/msg"
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

// fakeSenderCreds is a providerSenderCreds that records, for one build, which
// owner the sender was asked to bind: the token source, the model facts, and
// every Token call a built sender makes.
//
// The Token call is the point. A built sender exposes neither its token source
// nor its facts closure, so the only way to pin the binding AFTER construction
// is to drive the sender and see whose credential it reaches for. Token is
// therefore recorded and then FAILS: pkg/llm resolves the credential before it
// dials, so the failure stops the request inside the process and the test never
// touches the network.
type fakeSenderCreds struct {
	mu       sync.Mutex
	avail    bool
	why      string
	sources  []credKey // TokenSourceFor calls, in order
	factsFor []credKey // ModelFactsFor calls, in order
	tokens   []credKey // Token calls from a built sender, in order
}

func (f *fakeSenderCreds) Available() (bool, string) { return f.avail, f.why }

func (f *fakeSenderCreds) TokenSourceFor(owner, provider string) llm.TokenSource {
	key := credKey{owner: owner, provider: provider}
	f.record(&f.sources, key)
	return ownerBearer{creds: f, key: key}
}

func (f *fakeSenderCreds) ModelFactsFor(owner, provider string) llm.ModelFacts {
	f.record(&f.factsFor, credKey{owner: owner, provider: provider})
	return func(string) (llm.ModelFact, bool) { return llm.ModelFact{}, false }
}

func (f *fakeSenderCreds) record(dst *[]credKey, key credKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*dst = append(*dst, key)
}

func (f *fakeSenderCreds) recorded(dst func(*fakeSenderCreds) *[]credKey) []credKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]credKey(nil), (*dst(f))...)
}

// ownerBearer is one owner's token source. It answers for the owner it was
// built with, never for another, and it refuses so a send stops before dialing.
type ownerBearer struct {
	creds *fakeSenderCreds
	key   credKey
}

func (s ownerBearer) Token(context.Context, bool) (string, http.Header, error) {
	s.creds.record(&s.creds.tokens, s.key)
	return "", nil, fmt.Errorf("fake credential for %s: refusing so the request never leaves the process", s.key.owner)
}

// exerciseSender drives a built sender far enough to resolve its credential. It
// must fail (the fake token source refuses), and the failure is what proves the
// request never reached an endpoint.
func exerciseSender(t *testing.T, sender llm.Sender) {
	t.Helper()
	_, err := sender.Send(context.Background(), &msg.Request{
		Model:     "gpt-6.1-sol",
		MaxTokens: 16,
		Messages:  []msg.Message{{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}}},
	}, msg.EncodeOptions{})
	assert.NewAborting(t).Error(err, "sender.Send succeeded; the sender never resolved a credential, so this test observed nothing")
}

// Two owners spawning on the same ChatGPT provider must get two senders, each
// bound to its own owner's credential. A shared sender would let one user's
// turns spend another user's subscription, and would share the sender's cookie
// jar with it.
//
// The assertion runs through the BUILT senders, not through fresh literals: each
// send resolves a credential, so the recorded Token calls say which owner each
// sender actually reaches for.
func TestProviderSendersOwnerIsolation(t *testing.T) {
	ck := assert.NewCollecting(t)
	set := chatGPTProviderSet()
	creds := &fakeSenderCreds{avail: true}
	const provider = chatGPTTestProvider
	const model = chatGPTTestProvider + "/gpt-6.1-sol"

	sendersA, err := providerSenders(set, nil, model, ownerA, creds)
	ck.Require().NoError(err, "providerSenders(owner A)")
	sendersB, err := providerSenders(set, nil, model, ownerB, creds)
	ck.Require().NoError(err, "providerSenders(owner B)")

	senderA, okA := sendersA[provider]
	senderB, okB := sendersB[provider]
	ck.Require().True(okA && okB, "a ChatGPT-login provider with an owner and a usable credential got no sender")
	ck.False(senderA == senderB, "both owners were handed the same sender, so one's credential (and cookie jar) serves the other")

	// Each build bound its OWN owner's token source and facts, once.
	ck.EqDeep([]credKey{{owner: ownerA, provider: provider}, {owner: ownerB, provider: provider}},
		creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.sources }), "TokenSourceFor calls, in order")
	ck.EqDeep([]credKey{{owner: ownerA, provider: provider}, {owner: ownerB, provider: provider}},
		creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.factsFor }), "ModelFactsFor calls, in order")

	// And the senders themselves reach for their own owner's credential.
	exerciseSender(t, senderA)
	ck.EqDeep([]credKey{{owner: ownerA, provider: provider}},
		creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.tokens }),
		"owner A's sender did not spend owner A's credential")

	exerciseSender(t, senderB)
	ck.EqDeep([]credKey{{owner: ownerA, provider: provider}, {owner: ownerB, provider: provider}},
		creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.tokens }),
		"owner B's sender must spend owner B's credential, and must not re-spend owner A's")
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
		creds, _ := tc.creds.(*fakeSenderCreds)
		senders, err := providerSenders(set, nil, model, tc.owner, tc.creds)
		ck.Require().NoError(err, "providerSenders(%s)", tc.name)
		ck.NotHasKey(senders, chatGPTTestProvider, "providerSenders(%s) built a ChatGPT sender anyway", tc.name)
		if creds != nil {
			ck.Empty(creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.sources }),
				"providerSenders(%s) asked for a token source anyway", tc.name)
			ck.Empty(creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.factsFor }),
				"providerSenders(%s) asked for model facts anyway", tc.name)
		}
	}

	// The owner+credential case is the control: the same call DOES build one.
	creds := &fakeSenderCreds{avail: true}
	senders, err := providerSenders(set, nil, model, ownerA, creds)
	ck.Require().NoError(err, "providerSenders(control)")
	ck.HasKey(senders, chatGPTTestProvider, "providerSenders built nothing for an owner with usable credentials")
	ck.Len(creds.recorded(func(f *fakeSenderCreds) *[]credKey { return &f.sources }), 1, "one token source for one sender")
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
