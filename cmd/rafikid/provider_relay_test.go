// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/providers"
)

// A provider with no via_executor dials directly: nil transport, no error.
func TestRelayTransportNilWithoutViaExecutor(t *testing.T) {
	p := providers.Provider{Name: "anthropic", Kind: providers.KindAnthropic}
	rt, err := relayTransport(p, nil)
	if err != nil {
		t.Fatalf("relayTransport: %v", err)
	}
	if rt != nil {
		t.Fatalf("rt = %v, want nil (direct dial)", rt)
	}
}

// A via_executor provider whose selector matches no live executor advertising
// the proxy must FAIL THE TURN, never fall through to a direct dial: that
// would make a misconfigured relay look like it worked while the daemon
// quietly dialed base_url from the SERVER, which either refuses or reaches
// something unrelated.
func TestRelayTransportNoMatchingExecutorErrors(t *testing.T) {
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
	if err == nil {
		t.Fatal("relayTransport succeeded against an empty pool")
	}
	if rt != nil {
		t.Fatalf("rt = %v, want nil on error", rt)
	}
	if !strings.Contains(err.Error(), "vmlx") {
		t.Errorf("error = %q, want it to name the provider %q", err.Error(), "vmlx")
	}
	if !strings.Contains(err.Error(), "role=workstation") {
		t.Errorf("error = %q, want it to name the selector %q", err.Error(), "role=workstation")
	}
}

// A live executor matching the selector but advertising NO proxies must still
// refuse — the fix here is a --proxy flag, not a label, and the error should
// say so.
func TestRelayTransportErrorNamesTheProxy(t *testing.T) {
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
	if err == nil {
		t.Fatal("relayTransport succeeded against an executor advertising no proxies")
	}
	if rt != nil {
		t.Fatalf("rt = %v, want nil on error", rt)
	}
	if !strings.Contains(err.Error(), "vmlx") {
		t.Errorf("error = %q, want it to mention the proxy name %q", err.Error(), "vmlx")
	}
}

// A live executor matching the selector AND advertising the proxy: relayTransport
// succeeds with a non-nil transport. NewProxyTransport is lazy — it opens no
// stream until the first RoundTrip — so this assertion never touches the
// network.
func TestRelayTransportSucceedsWhenExecutorAdvertisesProxy(t *testing.T) {
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
	if err != nil {
		t.Fatalf("relayTransport: %v", err)
	}
	if rt == nil {
		t.Fatal("rt = nil, want a relay transport")
	}
}

// providerSenders must resolve only the providers this spawn's MODEL can
// actually reach — its own provider plus that provider's configured Fallback
// chain — not every via_executor provider in the whole registry. A provider
// outside that reachable set with no live executor must not fail a spawn for
// an unrelated model.
func TestProviderSendersSkipsUnreachableViaExecutorProvider(t *testing.T) {
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

	senders, err := providerSenders(set, pool, "openrouter/deepseek-v4-pro")
	if err != nil {
		t.Fatalf("providerSenders: %v, want success (vmlx is unrelated to this model)", err)
	}
	if _, ok := senders["vmlx"]; ok {
		t.Error(`senders contains "vmlx", want it omitted (no live executor, and unreachable by this model)`)
	}
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

	if _, err := providerSenders(set, pool, "vmlx/qwen"); err == nil {
		t.Fatal("providerSenders succeeded for a model whose own provider has no live executor")
	}
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

	// macOS TempDir paths often exceed the unix-socket path length limit;
	// /tmp directly with a short name avoids it.
	dir, err := os.MkdirTemp("/tmp", "relay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
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
