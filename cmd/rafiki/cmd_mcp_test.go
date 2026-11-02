// SPDX-License-Identifier: Apache-2.0

package main

// `rafiki mcp login` / `rafiki mcp logout` over the real wire: the endpoint the
// profile resolves, the requests the CLI sends, the callback served at the path
// and port the DAEMON names, the multi-round continuation loop, the paste
// fallback, the occupied-port refusal, and the output contract (directions and
// the success line on stderr for a login, the result on stdout for a logout,
// and never a callback code in either).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// fakeMcpLoginRPC is a ControlHandler double for the three MCP OAuth RPCs: it
// records what the CLI sent and answers with canned values, so the tests assert
// on the wire rather than on the flow's internals. Configure it BEFORE the
// server starts serving (that goroutine start is the happens-before edge for
// the fields the handlers read under the mutex).
type fakeMcpLoginRPC struct {
	rafikiv1connect.UnimplementedControlHandler

	mu        sync.Mutex
	beginReqs []*connect.Request[rafikiv1.BeginMcpLoginRequest]
	compReqs  []*connect.Request[rafikiv1.CompleteMcpLoginRequest]
	delReqs   []*connect.Request[rafikiv1.DeleteMcpCredentialRequest]

	auths []string

	beginResp *rafikiv1.BeginMcpLoginResponse
	beginErr  error
	// compResps is consumed in order: round one, round two, … A test that wants
	// one final answer leaves it with a single entry.
	compResps []*rafikiv1.CompleteMcpLoginResponse
	compErr   error
	delErr    error
}

func newFakeMcpLoginRPC() *fakeMcpLoginRPC { return &fakeMcpLoginRPC{} }

func (f *fakeMcpLoginRPC) BeginMcpLogin(
	_ context.Context, req *connect.Request[rafikiv1.BeginMcpLoginRequest],
) (*connect.Response[rafikiv1.BeginMcpLoginResponse], error) {
	f.mu.Lock()
	f.record(req)
	f.beginReqs = append(f.beginReqs, req)
	resp, err := f.beginResp, f.beginErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeMcpLoginRPC) CompleteMcpLogin(
	_ context.Context, req *connect.Request[rafikiv1.CompleteMcpLoginRequest],
) (*connect.Response[rafikiv1.CompleteMcpLoginResponse], error) {
	f.mu.Lock()
	f.record(req)
	f.compReqs = append(f.compReqs, req)
	round := len(f.compReqs) - 1
	err := f.compErr
	var resp *rafikiv1.CompleteMcpLoginResponse
	if err == nil {
		if round < len(f.compResps) {
			resp = f.compResps[round]
		} else {
			resp = &rafikiv1.CompleteMcpLoginResponse{Server: "linear"}
		}
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeMcpLoginRPC) DeleteMcpCredential(
	_ context.Context, req *connect.Request[rafikiv1.DeleteMcpCredentialRequest],
) (*connect.Response[rafikiv1.DeleteMcpCredentialResponse], error) {
	f.mu.Lock()
	f.record(req)
	f.delReqs = append(f.delReqs, req)
	err := f.delErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&rafikiv1.DeleteMcpCredentialResponse{}), nil
}

func (f *fakeMcpLoginRPC) lastBegin(t *testing.T) *rafikiv1.BeginMcpLoginRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.beginReqs) == 0 {
		return nil
	}
	return f.beginReqs[len(f.beginReqs)-1].Msg
}

func (f *fakeMcpLoginRPC) lastComplete(t *testing.T) *rafikiv1.CompleteMcpLoginRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.compReqs) == 0 {
		return nil
	}
	return f.compReqs[len(f.compReqs)-1].Msg
}

// authorizations returns the Authorization header of every request the fake
// saw, in order: the transport-carried credential is the one thing a CLI test
// can assert that the endpoint resolution cannot fake.
func (f *fakeMcpLoginRPC) authorizations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.auths))
	return append(out, f.auths...)
}

func (f *fakeMcpLoginRPC) record(raw connect.AnyRequest) {
	if raw == nil {
		return
	}
	f.auths = append(f.auths, raw.Header().Get("Authorization"))
}

func (f *fakeMcpLoginRPC) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.delReqs)
}

func (f *fakeMcpLoginRPC) completeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.compReqs)
}

func (f *fakeMcpLoginRPC) lastDelete(t *testing.T) *rafikiv1.DeleteMcpCredentialRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delReqs) == 0 {
		return nil
	}
	return f.delReqs[len(f.delReqs)-1].Msg
}

// serveMcpScratch isolates the profile environment and serves the fake Control
// plane on the socket the profile newConnectEndpoint dials.
func serveMcpScratch(t *testing.T, fake *fakeMcpLoginRPC) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	dir, err := os.MkdirTemp("", "rafiki-mcp")
	c.Require().NoError(err, "MkdirTemp")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	path, handler := rafikiv1connect.NewControlHandler(fake)
	serveConnectOnUnixSocket(t, sock, path, handler)
	writeTokenedProfile(t, sock, "")
}

// serveMcpScratchWithToken is serveMcpScratch with a profile credential, so a
// test can see it arrive on the wire.
func serveMcpScratchWithToken(t *testing.T, fake *fakeMcpLoginRPC, token string) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	dir, err := os.MkdirTemp("", "rafiki-mcpt")
	c.Require().NoError(err, "MkdirTemp")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	path, handler := rafikiv1connect.NewControlHandler(fake)
	serveConnectOnUnixSocket(t, sock, path, handler)
	writeTokenedProfile(t, sock, token)
}

// mcpCLIRoot runs the real `rafiki mcp` subtree behind the persistent flags the
// root command carries, with stdin, stdout and stderr kept apart: a login writes
// its prompts AND its success line to stderr, so a test that merged them could
// not tell the output contract from a silent stdout.
func mcpCLIRoot(t *testing.T, ctx context.Context, stdin io.Reader, args ...string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "rafiki", SilenceUsage: true}
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.PersistentFlags().StringP("color", "c", "auto", "")
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.AddCommand(newMCPCmd())

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	root.SetContext(ctx)
	return root, &stdout, &stderr
}

// TestMcpOAuthLoginDrivesTheDaemonsDescriptor is the whole verb over the real wire:
// BeginMcpLogin with the resolved absolute config path and the repeated
// --scope values, a callback served at the PATH the daemon named (rafiki
// login's own path must 404 there), Complete with the query the listener
// received, and the confirmation on stderr. It passes --no-browser, as every
// listener-driving test does.
func TestMcpOAuthLoginDrivesTheDaemonsDescriptor(t *testing.T) {
	c := assert.NewAborting(t)
	port := freeCallbackPort(t)
	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-1",
		AuthorizeUrl:  "https://auth.example.net/authorize?client_id=rafiki",
		RedirectPort:  port,
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.compResps = []*rafikiv1.CompleteMcpLoginResponse{{
		Server:   "linear",
		Resource: "https://mcp.example.net/mcp",
		Tools:    []string{"create_issue", "list_issues"},
	}}
	serveMcpScratch(t, fake)

	configPath := writeClientMCPConfig(t, `{"mcpServers":{"linear":{"url":"https://mcp.example.net/mcp"}}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, stderr := mcpCLIRoot(t, ctx, strings.NewReader(""),
		"mcp", "login", "linear",
		"--mcp-config", configPath,
		"--scope", "tools:read", "--scope", "tools:write",
		"--no-browser")
	exec := make(chan error, 1)
	go func() { exec <- root.Execute() }()

	client := &http.Client{Timeout: 2 * time.Second}
	// Asked first, and at the OTHER flow's path: a 404 here is what proves the
	// served path came from the daemon's callback_path, not from a constant.
	other := getCallbackWhenServing(t, client,
		fmt.Sprintf("http://%s:%d/oidc/callback?code=stale&state=s", loopbackHost, port))
	_ = other.Body.Close()
	c.Eq(http.StatusNotFound, other.StatusCode, "the flow must serve the daemon's callback path")

	resp, err := client.Get(fmt.Sprintf("http://%s:%d/mcp/oauth/callback?code=secret-code&state=s", loopbackHost, port))
	c.Require().NoError(err, "GET the callback")
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode, "callback status")

	c.Require().NoError(<-exec, "mcp login failed:\n%s", stderr.String())

	begin := fake.lastBegin(t)
	c.Require().NotNil(begin, "the daemon never saw BeginMcpLogin")
	c.Eq(configPath, begin.GetConfigPath(), "config_path must be the absolute path the flag named")
	c.Eq("linear", begin.GetServer(), "server")
	c.EqDeep([]string{"tools:read", "tools:write"}, begin.GetScopes(), "repeated --scope values")

	complete := fake.lastComplete(t)
	c.Require().NotNil(complete, "the daemon never saw CompleteMcpLogin")
	c.Eq("ml-1", complete.GetLoginId(), "login id")
	c.Eq("code=secret-code&state=s", complete.GetCallbackQuery(), "the query the listener received")

	out := stderr.String()
	c.StrContains(out, "https://auth.example.net/authorize?client_id=rafiki", "the authorize URL goes to stderr")
	c.StrContains(out, "logged in to linear (resource https://mcp.example.net/mcp, 2 tool(s))", "success line")
	c.Eq("", stdout.String(), "stdout stays silent: the confirmation is a prompt, not data")
	c.False(strings.Contains(out, "secret-code"), "the callback code must never be printed: %s", out)
	c.False(strings.Contains(out, "code="), "no callback query in the output: %s", out)
}

// TestMcpOAuthLoginConsentRounds pins the continuation contract from the
// client's side: a continuation is NOT success, the same login id carries on,
// the listener is re-bound for the next round at the same fixed port, and only
// the continuation-free answer is reported.
func TestMcpOAuthLoginConsentRounds(t *testing.T) {
	c := assert.NewAborting(t)
	port := freeCallbackPort(t)
	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-9",
		AuthorizeUrl:  "https://auth.example.net/authorize?round=1",
		RedirectPort:  port,
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.compResps = []*rafikiv1.CompleteMcpLoginResponse{
		{
			// Round one asks for more access: same login id, a new URL.
			Continuation: &rafikiv1.BeginMcpLoginResponse{
				LoginId:       "ml-9",
				AuthorizeUrl:  "https://auth.example.net/authorize?round=2",
				RedirectPort:  port,
				CallbackPath:  "/mcp/oauth/callback",
				RedirectHosts: []string{loopbackHost},
			},
		},
		{
			Server:   "linear",
			Resource: "https://mcp.example.net/mcp",
			Tools:    []string{"list_issues"},
		},
	}
	serveMcpScratch(t, fake)

	configPath := writeClientMCPConfig(t, `{"mcpServers":{"linear":{"url":"https://mcp.example.net/mcp"}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, stderr := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "login", "linear",
		"--mcp-config", configPath, "--no-browser")
	exec := make(chan error, 1)
	go func() { exec <- root.Execute() }()

	client := &http.Client{Timeout: 2 * time.Second}
	for round := 1; round <= 2; round++ {
		url := fmt.Sprintf("http://%s:%d/mcp/oauth/callback?code=round-%d&state=s", loopbackHost, port, round)
		// The listener is bound only after Begin returns (and again for round
		// two), so the browser's GET can arrive first; retrying is what a
		// browser tab reload does.
		resp := getCallbackWhenServing(t, client, url)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		c.Eq(http.StatusOK, resp.StatusCode, "round %d status", round)
	}

	c.Require().NoError(<-exec, "mcp login failed:\n%s", stderr.String())
	c.Eq(2, fake.completeCount(), "each consent round is completed once")
	c.Eq("ml-9", fake.lastComplete(t).GetLoginId(), "the same login id continues across rounds")

	out := stderr.String()
	c.StrContains(out, "round=1", "the first authorize URL")
	c.StrContains(out, "round=2", "the continuation's authorize URL")
	c.StrContains(out, "asked for more access", "the continuation is announced")
	c.StrContains(out, "logged in to linear", "the final round is the success")
	c.Eq("", stdout.String(), "stdout stays silent")
}

// TestMcpOAuthLoginPasteFallback drives the browser-on-another-machine path: a
// pasted redirect URL supplies the query, and the flow still binds the port the
// daemon named.
func TestMcpOAuthLoginPasteFallback(t *testing.T) {
	c := assert.NewAborting(t)
	port := freeCallbackPort(t)
	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-paste",
		AuthorizeUrl:  "https://auth.example.net/authorize",
		RedirectPort:  port,
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.compResps = []*rafikiv1.CompleteMcpLoginResponse{{Server: "linear", Resource: "https://mcp.example.net/mcp"}}
	serveMcpScratch(t, fake)

	stdin := strings.NewReader("http://localhost:1/mcp/oauth/callback?code=pasted&state=xyz\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, stderr := mcpCLIRoot(t, ctx, stdin, "mcp", "login", "linear",
		"--mcp-config", writeClientMCPConfig(t, `{"mcpServers":{}}`), "--no-browser")
	c.Require().NoError(root.Execute(), "mcp login --no-browser:\n%s", stderr.String())

	complete := fake.lastComplete(t)
	c.Require().NotNil(complete, "the daemon never saw CompleteMcpLogin")
	c.Eq("code=pasted&state=xyz", complete.GetCallbackQuery(), "the paste supplies the query")
	c.StrContains(stderr.String(), "logged in to linear", "success line")
	c.False(strings.Contains(stderr.String(), "pasted"), "the pasted query must not be echoed: %s", stderr.String())
	c.Eq("", stdout.String(), "stdout stays silent")
}

// TestMcpOAuthLoginOccupiedPort pins the fixed-port rule: the port comes
// from the daemon (it is the registered redirect URI), so a busy one is an error
// naming the port and the fix — never a silent substitution to another port.
func TestMcpOAuthLoginOccupiedPort(t *testing.T) {
	c := assert.NewAborting(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	c.Require().NoError(err, "hold a port")
	held := uint32(ln.Addr().(*net.TCPAddr).Port)
	t.Cleanup(func() { _ = ln.Close() })

	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-busy",
		AuthorizeUrl:  "https://auth.example.net/authorize",
		RedirectPort:  held,
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	serveMcpScratch(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, _, _ := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "login", "linear",
		"--mcp-config", writeClientMCPConfig(t, `{"mcpServers":{}}`), "--no-browser")
	err = root.Execute()
	c.Require().Error(err, "a busy callback port must fail")
	msg := err.Error()
	c.StrContains(msg, strconv.FormatUint(uint64(held), 10), "the error names the port: %s", msg)
	c.StrContains(msg, "free the port, then retry", "the error names the fix: %s", msg)
	c.Eq(0, fake.completeCount(), "no completion is attempted on a port that could not be bound")
}

// TestMcpOAuthLoginKeepsTheConnectCode: a daemon refusal reaches the user with its
// code, not as a generic failure.
func TestMcpOAuthLoginKeepsTheConnectCode(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeMcpLoginRPC()
	fake.beginErr = connect.NewError(connect.CodeInvalidArgument,
		errors.New(`cannot read the MCP config "/nope/.mcp.json"`))
	serveMcpScratch(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, _, _ := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "login", "linear",
		"--mcp-config", "/nope/.mcp.json", "--no-browser")
	err := root.Execute()
	c.Require().Error(err, "a refused Begin must fail the command")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "the Connect code is preserved: %v", err)
	c.StrContains(err.Error(), "cannot read the MCP config", "the daemon's message reaches the user: %v", err)
}

// TestMcpOAuthLogoutSelectors pins both forms and their exclusivity. A logout is the
// command's RESULT, so it goes to stdout; the server form names the resolved
// config path, the resource form names no config at all.
func TestMcpOAuthLogoutSelectors(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeMcpLoginRPC()
	serveMcpScratch(t, fake)
	configPath := writeClientMCPConfig(t, `{"mcpServers":{"linear":{"url":"https://mcp.example.net/mcp"}}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	root, stdout, stderr := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "logout", "linear",
		"--mcp-config", configPath)
	c.Require().NoError(root.Execute(), "mcp logout linear:\n%s", stderr.String())
	c.Eq("logged out of linear\n", stdout.String(), "the logout result goes to stdout")
	req := fake.lastDelete(t)
	c.Require().NotNil(req, "the daemon never saw DeleteMcpCredential")
	c.Eq(configPath, req.GetConfigPath(), "config_path")
	c.Eq("linear", req.GetServer(), "server")
	c.Eq("", req.GetResource(), "no resource on the entry form")

	root, stdout, stderr = mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "logout",
		"--resource", "https://mcp.example.net/mcp")
	c.Require().NoError(root.Execute(), "mcp logout --resource:\n%s", stderr.String())
	c.Eq("logged out of https://mcp.example.net/mcp\n", stdout.String(), "the resource is echoed")
	req = fake.lastDelete(t)
	c.Eq("https://mcp.example.net/mcp", req.GetResource(), "resource")
	c.Eq("", req.GetConfigPath(), "the resource form names no config file")
	c.Eq("", req.GetServer(), "and no server")

	// Both forms, and neither, are refused before any RPC is sent.
	before := fake.deleteCount()
	for _, args := range [][]string{
		{"mcp", "logout", "linear", "--resource", "https://mcp.example.net/mcp"},
		{"mcp", "logout"},
		{"mcp", "logout", "linear", "--resource", "https://x/y", "--mcp-config", configPath},
	} {
		root, _, _ := mcpCLIRoot(t, ctx, strings.NewReader(""), args...)
		c.Error(root.Execute(), "%v must be refused", args)
	}
	c.Eq(before, fake.deleteCount(), "a malformed selector must never reach the daemon")
}

// TestMcpOAuthLogoutNotFound: a grant the caller does not have is
// not_found on the wire and in the message.
func TestMcpOAuthLogoutNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeMcpLoginRPC()
	fake.delErr = connect.NewError(connect.CodeNotFound, errors.New(`no MCP credential is stored for server "linear"`))
	serveMcpScratch(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, _ := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "logout", "linear",
		"--mcp-config", "/srv/app/.mcp.json")
	err := root.Execute()
	c.Require().Error(err, "an absent grant must fail the logout")
	c.Eq(connect.CodeNotFound, connect.CodeOf(err), "code preserved: %v", err)
	c.Eq("", stdout.String(), "a failed logout prints no success line")
}

// TestMcpOAuthConfigPathPrecedence pins the documented order: explicit flag, then the
// working directory's .mcp.json, then $RAFIKI_MCP_CONFIG, then the config dir's
// mcp.json — and that every result is absolute.
func TestMcpOAuthConfigPathPrecedence(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	explicit := filepath.Join(t.TempDir(), "explicit.json")
	fromEnv := filepath.Join(t.TempDir(), "env.json")
	t.Setenv(paths.MCPConfig, fromEnv)

	got, err := mcpConfigPath(explicit)
	c.Require().NoError(err, "explicit")
	c.Eq(explicit, got, "an explicit --mcp-config wins")

	// No explicit flag: the working directory's .mcp.json, when there is one.
	work := t.TempDir()
	t.Chdir(work)

	got, err = mcpConfigPath("")
	c.Require().NoError(err, "env fallback")
	c.Eq(fromEnv, got, "$RAFIKI_MCP_CONFIG is the fallback when the cwd has no .mcp.json")

	c.Require().NoError(os.WriteFile(filepath.Join(work, ".mcp.json"), []byte(`{"mcpServers":{}}`), 0o600), "write cwd config")
	got, err = mcpConfigPath("")
	c.Require().NoError(err, "cwd config")
	c.Eq(filepath.Join(work, ".mcp.json"), got, "the working directory's .mcp.json outranks the env var")

	// And with neither, the config directory's mcp.json.
	t.Setenv(paths.MCPConfig, "")
	c.Require().NoError(os.Remove(filepath.Join(work, ".mcp.json")), "remove the cwd config")
	got, err = mcpConfigPath("")
	c.Require().NoError(err, "config-dir fallback")
	c.Eq(paths.GlobalMCPConfig(), got, "the config directory's mcp.json is the last resort")
	c.True(filepath.IsAbs(got), "the path sent to the daemon is always absolute: %s", got)
}

// writeClientMCPConfig writes an MCP config a test names explicitly and returns
// its absolute path.
func writeClientMCPConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".mcp.json")
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write .mcp.json")
	return path
}

// TestMcpOAuthLoginCarriesTheProfileBearer is the brief's "the fakes must assert
// headers" gap: the commands resolve an endpoint through the profile, and the
// profile's credential rides the TRANSPORT (bearerTransport), not a per-call
// header. A verb that bypassed newConnectEndpoint would send nothing here.
func TestMcpOAuthLoginCarriesTheProfileBearer(t *testing.T) {
	c := assert.NewAborting(t)
	port := freeCallbackPort(t)
	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-bearer",
		AuthorizeUrl:  "https://auth.example.net/authorize",
		RedirectPort:  port,
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.compResps = []*rafikiv1.CompleteMcpLoginResponse{{Server: "linear"}}
	serveMcpScratchWithToken(t, fake, "rfk_mcp_bearer")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, _, stderr := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "login", "linear",
		"--mcp-config", writeClientMCPConfig(t, `{"mcpServers":{}}`), "--no-browser")
	exec := make(chan error, 1)
	go func() { exec <- root.Execute() }()

	resp := getCallbackWhenServing(t, &http.Client{Timeout: 2 * time.Second},
		fmt.Sprintf("http://%s:%d/mcp/oauth/callback?code=c&state=s", loopbackHost, port))
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	c.Require().NoError(<-exec, "mcp login failed:\n%s", stderr.String())

	for _, auth := range fake.authorizations() {
		c.Eq("Bearer rfk_mcp_bearer", auth, "every RPC must carry the profile's bearer on its transport")
	}
	c.Eq(2, len(fake.authorizations()), "both calls must arrive, each with the credential")
}

// TestMcpOAuthLoginCancelReturnsPromptly pins the abort path: Ctrl-C (or any
// caller cancellation) while the callback is still awaited stops the command
// immediately and says so, rather than sitting on the listener until the 10
// minute callback deadline. The daemon's own pending login is left to its TTL —
// there is deliberately no cancel RPC.
func TestMcpOAuthLoginCancelReturnsPromptly(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-cancel",
		AuthorizeUrl:  "https://auth.example.net/authorize",
		RedirectPort:  freeCallbackPort(t),
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	serveMcpScratch(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root, _, stderr := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "login", "linear",
		"--mcp-config", writeClientMCPConfig(t, `{"mcpServers":{}}`), "--no-browser")
	exec := make(chan error, 1)
	go func() { exec <- root.Execute() }()

	// Cancel once the daemon has answered Begin: by then the listener is bound
	// and the flow is waiting for a browser that never comes.
	deadline := time.Now().Add(10 * time.Second)
	for fake.lastBegin(t) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the daemon never saw BeginMcpLogin")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-exec:
		c.Require().Error(err, "a cancelled login must fail")
		c.StrContains(err.Error(), "login canceled", "the cancellation reads as one: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("mcp login did not return after its context was cancelled")
	}
	c.Eq(0, fake.completeCount(), "a cancelled login must not complete")
	c.StrContains(stderr.String(), "open this URL in a browser", "the prompt had been printed")
}

// TestMcpOAuthLoginStripsOneLeadingQuestionMark pins the normalisation against
// the SDK contract: the engine parses the query with url.ParseQuery, which reads
// "?code=x" as the key "?code". The listener path never produces one (RawQuery
// is already stripped), so the helper is asserted directly and the flow is
// asserted to send the stripped form end to end.
func TestMcpOAuthLoginStripsOneLeadingQuestionMark(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeMcpLoginRPC()
	fake.beginResp = &rafikiv1.BeginMcpLoginResponse{
		LoginId:       "ml-qmark",
		AuthorizeUrl:  "https://auth.example.net/authorize",
		RedirectPort:  freeCallbackPort(t),
		CallbackPath:  "/mcp/oauth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.compResps = []*rafikiv1.CompleteMcpLoginResponse{{Server: "linear"}}
	serveMcpScratch(t, fake)

	// A pasted URL whose query keeps its "?" is what awaitCallback strips, so
	// drive the flow the way an SDK caller would: the listener receives a
	// redirect whose RawQuery has no "?", and the CLI's own normalisation is
	// what guards the other entry point.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, _, stderr := mcpCLIRoot(t, ctx, strings.NewReader(""), "mcp", "login", "linear",
		"--mcp-config", writeClientMCPConfig(t, `{"mcpServers":{}}`), "--no-browser")
	exec := make(chan error, 1)
	go func() { exec <- root.Execute() }()

	resp := getCallbackWhenServing(t, &http.Client{Timeout: 2 * time.Second},
		fmt.Sprintf("http://%s:%d/mcp/oauth/callback?code=ok&state=s", loopbackHost, fake.beginResp.GetRedirectPort()))
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	c.Require().NoError(<-exec, "mcp login failed:\n%s", stderr.String())

	c.Eq("code=ok&state=s", fake.lastComplete(t).GetCallbackQuery(),
		"the daemon receives a raw query with no leading '?'")

	// The helper itself, both directions: exactly ONE "?" goes, and a query that
	// never had one is untouched.
	c.Eq("code=ok&state=s", normalizeCallbackQuery("?code=ok&state=s"), "one leading ? is dropped")
	c.Eq("code=ok&state=s", normalizeCallbackQuery("code=ok&state=s"), "a bare query is untouched")
	c.Eq("?code=x", normalizeCallbackQuery("??code=x"), "only ONE ? is dropped")
	c.Eq("", normalizeCallbackQuery("?"), "a lone ? becomes the empty query")
}
