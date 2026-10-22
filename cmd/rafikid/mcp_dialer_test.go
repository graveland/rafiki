// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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

	"github.com/multigres/testkit/assert"
)

// writeMCPConfig writes a fixture mcp.json and returns its path.
func writeMCPConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write config")
	return path
}

// A child with no via_executor entries resolves nothing and never touches the
// executor plane — every ordinary mcp.json behaves exactly as before.
func TestResolveMCPDialerNilWithoutViaExecutor(t *testing.T) {
	c := assert.NewAborting(t)
	path := writeMCPConfig(t, `{"mcpServers":{"local":{"command":"hesabu","args":["mcp"]}}}`)
	dial, err := resolveMCPDialer(path, "", nil)
	c.NoError(err, "resolveMCPDialer")
	c.Nil(dial, "dialer for a config with no via_executor server")
}

// A via_executor server with no executor pool fails closed, naming the knobs —
// this is what a daemon without the executor plane (and `rafikid agent`) gets.
func TestResolveMCPDialerRefusesWithoutAPool(t *testing.T) {
	c := assert.NewCollecting(t)
	path := writeMCPConfig(t, `{"mcpServers":{"hesabu":{"via_executor":{"selector":"name=greyshift","command":"hesabu"}}}}`)
	_, err := resolveMCPDialer(path, "", nil)
	c.Require().Error(err, "resolved a via_executor server with no executor plane")
	c.StrContains(err.Error(), `mcp server "hesabu"`, "error names the server")
	c.StrContains(err.Error(), "via_executor requires the executor plane", "error")
}

// No live executor advertising the name says how to declare it: the laptop has
// not been updated, and that is the operator's actual problem.
func TestResolveMCPDialerNoExecutorDeclaresTheName(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newMCPPool(t, fakeMCPExec{
		token:       "t_one",
		exec:        executors.Executor{ID: "exec-1", Enabled: true, Labels: map[string]string{"name": "greyshift"}},
		mcpCommands: nil, // declares no MCP commands
	})
	path := writeMCPConfig(t, `{"mcpServers":{"hesabu":{"via_executor":{"selector":"name=greyshift","command":"hesabu"}}}}`)

	_, err := resolveMCPDialer(path, "", pool)
	c.Require().Error(err, "resolved a server no executor declares")
	c.StrContains(err.Error(), "declares MCP command", "error")
	c.StrContains(err.Error(), "--mcp-command hesabu=", "error names the flag")
}

// An executor DOES declare the name but none matching the selector says THAT:
// it is a selector problem, not a missing flag, and the two must not read the
// same.
func TestResolveMCPDialerNamesAMismatchedSelector(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newMCPPool(t, fakeMCPExec{
		token:       "t_one",
		exec:        executors.Executor{ID: "exec-1", Enabled: true, Labels: map[string]string{"name": "other-machine"}},
		mcpCommands: []string{"hesabu"},
	})
	path := writeMCPConfig(t, `{"mcpServers":{"hesabu":{"via_executor":{"selector":"name=greyshift","command":"hesabu"}}}}`)

	_, err := resolveMCPDialer(path, "", pool)
	c.Require().Error(err, "resolved a server on a non-matching executor")
	c.StrContains(err.Error(), "no executor matching selector", "error")
	c.StrContains(err.Error(), "name=greyshift", "error names the selector")
	c.False(strings.Contains(err.Error(), "--mcp-command"), "error blames the wrong thing: %v", err)
}

// A PROXY server resolves the same way, and its "no executor" message names
// --proxy rather than --mcp-command.
func TestResolveMCPDialerProxyNamesTheProxyFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newMCPPool(t, fakeMCPExec{
		token: "t_one",
		exec:  executors.Executor{ID: "exec-1", Enabled: true, Labels: map[string]string{"name": "greyshift"}},
	})
	path := writeMCPConfig(t, `{"mcpServers":{"codescan":{"via_executor":{"selector":"name=greyshift","proxy":"codescan"}}}}`)

	_, err := resolveMCPDialer(path, "", pool)
	c.Require().Error(err, "resolved a proxy server no executor advertises")
	c.StrContains(err.Error(), "advertises proxy", "error")
	c.StrContains(err.Error(), "--proxy codescan=", "error names the flag")
}

// A malformed selector EXCLUDES rather than admits, and says so by name.
func TestResolveMCPDialerRefusesABadSelector(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := newMCPPool(t, fakeMCPExec{
		token:       "t_one",
		exec:        executors.Executor{ID: "exec-1", Enabled: true, Labels: map[string]string{"name": "greyshift"}},
		mcpCommands: []string{"hesabu"},
	})
	path := writeMCPConfig(t, `{"mcpServers":{"hesabu":{"via_executor":{"selector":"=bad","command":"hesabu"}}}}`)

	_, err := resolveMCPDialer(path, "", pool)
	c.Require().Error(err, "resolved a server through a malformed selector")
	c.StrContains(err.Error(), `mcp server "hesabu"`, "error names the server")
	c.StrContains(err.Error(), "via_executor.selector", "error names the field")
}

// D10: when several executors match, the LOWEST ID wins — a child's server must
// land on the same machine twice in a row.
func TestResolveMCPDialerPicksTheLowestExecutorID(t *testing.T) {
	c := assert.NewAborting(t)
	pool := newMCPPool(t,
		fakeMCPExec{
			token:       "t_b",
			exec:        executors.Executor{ID: "exec-b", Enabled: true, Labels: map[string]string{"name": "greyshift"}},
			mcpCommands: []string{"hesabu"},
		},
		fakeMCPExec{
			token:       "t_a",
			exec:        executors.Executor{ID: "exec-a", Enabled: true, Labels: map[string]string{"name": "greyshift"}},
			mcpCommands: []string{"hesabu"},
		},
	)
	path := writeMCPConfig(t, `{"mcpServers":{"hesabu":{"via_executor":{"selector":"name=greyshift","command":"hesabu"}}}}`)

	resolved, err := resolveMCPEntries(path, "", pool)
	c.NoError(err, "resolveMCPEntries")
	c.Require().True(len(resolved) == 1, "resolved %d entries, want 1", len(resolved))
	c.Eq(resolved["hesabu"].executorID, "exec-a", "resolved executor")
}

// An entry the child's allowlist excludes never refuses its spawn: it is not
// this child's server.
func TestResolveMCPDialerSkipsEntriesOutsideTheAllowlist(t *testing.T) {
	c := assert.NewAborting(t)
	pool := execpool.New(relayFakeStore{}) // no executor at all
	path := writeMCPConfig(t, `{"mcpServers":{
		"hesabu":{"via_executor":{"selector":"name=greyshift","command":"hesabu"}},
		"codescan":{"via_executor":{"selector":"name=greyshift","proxy":"codescan"}}
	}}`)

	dial, err := resolveMCPDialer(path, "localonly", pool)
	c.NoError(err, "a server outside the allowlist refused the spawn")
	c.Nil(dial, "dialer for a config whose via_executor servers are all excluded")
}

// ─── helpers ────────────────────────────────────────────────────────────────

// fakeMCPExec is one fake executor to admit into a test pool.
type fakeMCPExec struct {
	token       string
	exec        executors.Executor
	mcpCommands []string
	proxies     []string
}

// mcpTokenStore enrolls by TOKEN, so one pool can hold several executors with
// different rows.
type mcpTokenStore struct {
	relayFakeStore
	byToken map[string]executors.Executor
}

func (s mcpTokenStore) Enroll(_ context.Context, token string, _ map[string]string) (executors.Executor, string, error) {
	exec, ok := s.byToken[token]
	if !ok {
		return executors.Executor{}, "", errors.New("unknown enrollment token")
	}
	return exec, "cred", nil
}

// mcpDescribeHandler answers Describe with the self-reported facts a
// via_executor entry is resolved against.
type mcpDescribeHandler struct {
	executorpbconnect.UnimplementedExecutorServiceHandler
	executorID  string
	mcpCommands []string
	proxies     []string
}

func (h *mcpDescribeHandler) Describe(
	context.Context, *connect.Request[executorpb.DescribeRequest],
) (*connect.Response[executorpb.DescribeResponse], error) {
	return connect.NewResponse(&executorpb.DescribeResponse{
		ExecutorId:  h.executorID,
		McpCommands: h.mcpCommands,
		Proxies:     h.proxies,
	}), nil
}

func (h *mcpDescribeHandler) Health(
	context.Context, *connect.Request[executorpb.HealthRequest],
) (*connect.Response[executorpb.HealthResponse], error) {
	return connect.NewResponse(&executorpb.HealthResponse{}), nil
}

// newMCPPool admits every given fake executor over a real unix-socket
// reverse-dial — the same path a local executor uses — and waits until they are
// all live.
func newMCPPool(t *testing.T, execs ...fakeMCPExec) *execpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)

	byToken := make(map[string]executors.Executor, len(execs))
	for _, e := range execs {
		byToken[e.token] = e.exec
	}
	pool := execpool.New(mcpTokenStore{byToken: byToken})

	for _, e := range execs {
		// macOS TempDir paths often exceed the unix-socket path length limit;
		// /tmp directly with a short name avoids it.
		dir, err := os.MkdirTemp("/tmp", "mcppool-")
		c.NoError(err, "mktemp")
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "s")
		ln, err := net.Listen("unix", sock)
		c.NoError(err, "listen")
		t.Cleanup(func() { _ = ln.Close() })
		go func() { _ = pool.Serve(ln) }()

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		_, h := executorpbconnect.NewExecutorServiceHandler(&mcpDescribeHandler{
			executorID:  e.exec.ID,
			mcpCommands: e.mcpCommands,
			proxies:     e.proxies,
		})
		go func() {
			_ = execpool.Connect(ctx, execpool.ConnectOptions{
				SocketPath:  sock,
				EnrollToken: e.token,
				Handler:     h,
			})
		}()
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(pool.Live()) == len(execs) {
			return pool
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fake executors never joined the pool: %d of %d live", len(pool.Live()), len(execs))
	return nil
}
