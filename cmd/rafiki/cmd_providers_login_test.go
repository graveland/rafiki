// SPDX-License-Identifier: Apache-2.0

package main

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

	"github.com/multigres/testkit/assert"
)

// fakeProviderLogin is a ControlHandler double for the three provider-login
// RPCs: it records what the CLI sent and answers with canned values, so the
// tests assert on the wire rather than on the flow's internals.
type fakeProviderLogin struct {
	rafikiv1connect.UnimplementedControlHandler

	mu           sync.Mutex
	beginReqs    []*connect.Request[rafikiv1.BeginProviderLoginRequest]
	completeReqs []*connect.Request[rafikiv1.CompleteProviderLoginRequest]
	deleteReqs   []*connect.Request[rafikiv1.DeleteProviderCredentialRequest]

	beginResp    *rafikiv1.BeginProviderLoginResponse
	completeResp *rafikiv1.CompleteProviderLoginResponse
	beginErr     error
	completeErr  error
	deleteErr    error
}

func newFakeProviderLogin() *fakeProviderLogin { return &fakeProviderLogin{} }

func (f *fakeProviderLogin) BeginProviderLogin(
	_ context.Context, req *connect.Request[rafikiv1.BeginProviderLoginRequest],
) (*connect.Response[rafikiv1.BeginProviderLoginResponse], error) {
	f.mu.Lock()
	f.beginReqs = append(f.beginReqs, req)
	resp, err := f.beginResp, f.beginErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeProviderLogin) CompleteProviderLogin(
	_ context.Context, req *connect.Request[rafikiv1.CompleteProviderLoginRequest],
) (*connect.Response[rafikiv1.CompleteProviderLoginResponse], error) {
	f.mu.Lock()
	f.completeReqs = append(f.completeReqs, req)
	resp, err := f.completeResp, f.completeErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeProviderLogin) DeleteProviderCredential(
	_ context.Context, req *connect.Request[rafikiv1.DeleteProviderCredentialRequest],
) (*connect.Response[rafikiv1.DeleteProviderCredentialResponse], error) {
	f.mu.Lock()
	f.deleteReqs = append(f.deleteReqs, req)
	err := f.deleteErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&rafikiv1.DeleteProviderCredentialResponse{}), nil
}

func (f *fakeProviderLogin) setDeleteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

func (f *fakeProviderLogin) lastBegin(t *testing.T) *rafikiv1.BeginProviderLoginRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.beginReqs) == 0 {
		return nil
	}
	return f.beginReqs[len(f.beginReqs)-1].Msg
}

func (f *fakeProviderLogin) lastComplete(t *testing.T) *rafikiv1.CompleteProviderLoginRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.completeReqs) == 0 {
		return nil
	}
	return f.completeReqs[len(f.completeReqs)-1].Msg
}

func (f *fakeProviderLogin) lastDelete(t *testing.T) *rafikiv1.DeleteProviderCredentialRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deleteReqs) == 0 {
		return nil
	}
	return f.deleteReqs[len(f.deleteReqs)-1].Msg
}

// freeCallbackPort returns a port nothing is listening on, taken from a
// listener that is closed again. Deliberately not 1455: that is the port a real
// ChatGPT login uses, so a developer with `codex login` running would otherwise
// fail this suite.
func freeCallbackPort(t *testing.T) uint32 {
	t.Helper()
	c := assert.NewAborting(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	c.Require().NoError(err, "listen for a free port")
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	c.Require().NoError(ln.Close(), "close the probe listener")
	return port
}

// serveProvidersScratch isolates the profile environment and serves the fake
// Control plane on the socket profile that newConnectEndpoint dials. The temp
// dir is short on purpose: unix socket paths are capped at ~104 bytes.
func serveProvidersScratch(t *testing.T, fake *fakeProviderLogin) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	dir, err := os.MkdirTemp("", "rafiki-pl")
	c.Require().NoError(err, "MkdirTemp")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	path, handler := rafikiv1connect.NewControlHandler(fake)
	serveConnectOnUnixSocket(t, sock, path, handler)
	writeTokenedProfile(t, sock, "")
}

// providersCLIRoot runs the real `rafiki providers` subtree behind the
// persistent flags the root command carries, with stdin, stdout and stderr kept
// apart: a login writes its prompts AND its success line to stderr, so a test
// that merged the two could not tell the output contract from a silent stdout.
func providersCLIRoot(t *testing.T, ctx context.Context, stdin io.Reader, args ...string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.PersistentFlags().StringP("color", "c", "auto", "")
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.AddCommand(newProvidersCmd())

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(stdin)
	root.SetArgs(args)
	root.SetContext(ctx)
	return root, &stdout, &stderr
}

// getCallbackWhenServing GETs url until the callback listener answers or the
// deadline passes, and returns the first response. The flow binds the port only
// after BeginProviderLogin returns, so the browser's GET (here, the test's) can
// arrive before there is a listener; retrying is what a browser tab reload
// does, and the assertions are about what the listener answers.
func getCallbackWhenServing(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for {
		resp, err := client.Get(url)
		if err == nil {
			return resp
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("callback listener never answered %s: %v", url, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProvidersLoginUsesDaemonPathAndHosts is the whole verb over the real
// wire: BeginProviderLogin on the profile's socket, a callback served at the
// PATH the daemon named (rafiki login's own path must 404 there), Complete with
// the query the listener received, and the confirmation on stderr.
func TestProvidersLoginUsesDaemonPathAndHosts(t *testing.T) {
	c := assert.NewAborting(t)
	port := freeCallbackPort(t)
	fake := newFakeProviderLogin()
	fake.beginResp = &rafikiv1.BeginProviderLoginResponse{
		LoginId:       "pl-1",
		AuthorizeUrl:  "https://auth.example.net/oauth/authorize?client_id=rafiki",
		RedirectPort:  port,
		CallbackPath:  "/auth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.completeResp = &rafikiv1.CompleteProviderLoginResponse{Provider: "chatgpt", AccountId: "acct-7"}
	serveProvidersScratch(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, stderr := providersCLIRoot(t, ctx, strings.NewReader(""),
		"providers", "login", "chatgpt", "--no-browser")
	exec := make(chan error, 1)
	go func() { exec <- root.Execute() }()

	client := &http.Client{Timeout: 2 * time.Second}
	// Asked first, and at the OTHER flow's path: a 404 here is what proves the
	// served path came from the daemon's callback_path, not from a constant.
	other := getCallbackWhenServing(t, client,
		fmt.Sprintf("http://%s:%d/oidc/callback?code=stale&state=s", loopbackHost, port))
	_ = other.Body.Close()
	c.Eq(http.StatusNotFound, other.StatusCode, "the flow must serve the daemon's callback path, not rafiki login's")

	resp, err := client.Get(fmt.Sprintf("http://%s:%d/auth/callback?code=c&state=s", loopbackHost, port))
	c.Require().NoError(err, "GET /auth/callback")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode, "callback status")
	c.True(strings.Contains(string(body), "close this tab"), "callback body = %s", body)

	c.Require().NoError(<-exec, "providers login failed:\n%s", stderr.String())

	begin := fake.lastBegin(t)
	c.Require().NotNil(begin, "the daemon never saw BeginProviderLogin")
	c.Eq("chatgpt", begin.GetProvider(), "provider in BeginProviderLogin")

	complete := fake.lastComplete(t)
	c.Require().NotNil(complete, "the daemon never saw CompleteProviderLogin")
	c.Eq("pl-1", complete.GetLoginId(), "login id")
	c.Eq("code=c&state=s", complete.GetCallbackQuery(), "the query the listener received")

	out := stderr.String()
	c.StrContains(out, "https://auth.example.net/oauth/authorize?client_id=rafiki", "the authorize URL goes to stderr")
	c.StrContains(out, "logged in to chatgpt (account acct-7)", "success line")
	c.Eq("", stdout.String(), "stdout stays silent: the confirmation is a prompt, not data")
}

// TestProvidersLoginPasteFallback drives the browser-on-another-machine path: a
// pasted redirect URL supplies the query, and a paste for any other path is
// ignored with a warning, since the daemon's callback path is the one the
// issuer redirects to.
func TestProvidersLoginPasteFallback(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeProviderLogin()
	fake.beginResp = &rafikiv1.BeginProviderLoginResponse{
		LoginId:       "pl-2",
		AuthorizeUrl:  "https://auth.example.net/oauth/authorize",
		RedirectPort:  freeCallbackPort(t),
		CallbackPath:  "/auth/callback",
		RedirectHosts: []string{loopbackHost},
	}
	fake.completeResp = &rafikiv1.CompleteProviderLoginResponse{Provider: "chatgpt", AccountId: "acct-9"}
	serveProvidersScratch(t, fake)

	stdin := strings.NewReader(strings.Join([]string{
		"http://localhost:1/oidc/callback?code=wrong&state=s",
		"http://localhost:1/auth/callback?code=pasted&state=xyz",
	}, "\n") + "\n")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, stderr := providersCLIRoot(t, ctx, stdin, "providers", "login", "chatgpt", "--no-browser")
	c.Require().NoError(root.Execute(), "providers login --no-browser:\n%s", stderr.String())

	complete := fake.lastComplete(t)
	c.Require().NotNil(complete, "the daemon never saw CompleteProviderLogin")
	c.Eq("pl-2", complete.GetLoginId(), "login id")
	c.Eq("code=pasted&state=xyz", complete.GetCallbackQuery(), "only the paste for the daemon's path supplies the query")

	out := stderr.String()
	c.StrContains(out, "not a login redirect URL", "the other-path paste must be warned about")
	c.StrContains(out, "logged in to chatgpt (account acct-9)", "success line")
	c.Eq("", stdout.String(), "stdout stays silent")
}

// TestLoginLoopbackPortBusyError pins the shared flow's busiest failure: the
// port a provider or the daemon fixed is held (a running `codex login` is the
// usual holder), so the error names the port, who fixed it, and the escape.
// Nothing is printed before the bind succeeds, so no browser is ever sent to a
// listener that is not there.
func TestLoginLoopbackPortBusyError(t *testing.T) {
	c := assert.NewAborting(t)
	held, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	c.Require().NoError(err, "listen")
	t.Cleanup(func() { _ = held.Close() })
	port := uint32(held.Addr().(*net.TCPAddr).Port)

	var stderr bytes.Buffer
	_, err = loopbackCallback(context.Background(),
		loginIO{noBrowser: true, stdin: strings.NewReader(""), stderr: &stderr},
		[]string{loopbackHost}, port, "/auth/callback", "https://auth.example.net/oauth/authorize",
		providerLoginPinnedBy)
	c.Require().Error(err, "a held callback port must fail")

	msg := err.Error()
	c.StrContains(msg, "cannot listen on callback port "+strconv.FormatUint(uint64(port), 10), "the port must be named")
	c.StrContains(msg, providerLoginPinnedBy, "who fixed the port must be named")
	c.StrContains(msg, "--no-browser", "the escape must be suggested")
	c.StrContains(msg, "paste the redirect URL", "the escape must say what to paste")
	c.NotStrContains(stderr.String(), "open this URL in a browser", "no prompt may be printed before the bind succeeds:\n%s", stderr.String())
}

// TestProvidersLogoutDeletesTheCredential is logout end to end: the provider
// reaches DeleteProviderCredential, the confirmation goes to stdout, and the
// daemon's not_found for a provider with no stored credential surfaces as the
// command's error rather than a confirmation.
func TestProvidersLogoutDeletesTheCredential(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeProviderLogin()
	serveProvidersScratch(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, stdout, stderr := providersCLIRoot(t, ctx, strings.NewReader(""), "providers", "logout", "chatgpt")
	c.Require().NoError(root.Execute(), "providers logout:\n%s", stderr.String())

	del := fake.lastDelete(t)
	c.Require().NotNil(del, "the daemon never saw DeleteProviderCredential")
	c.Eq("chatgpt", del.GetProvider(), "provider")
	c.StrContains(stdout.String(), "logged out of chatgpt", "confirmation on stdout")

	fake.setDeleteErr(connect.NewError(connect.CodeNotFound,
		errors.New("no stored credential for provider chatgpt")))
	root, stdout, _ = providersCLIRoot(t, ctx, strings.NewReader(""), "providers", "logout", "chatgpt")
	err := root.Execute()
	c.Require().Error(err, "logout of a provider with no credential succeeded")
	c.StrContains(err.Error(), "no stored credential", "the daemon's refusal must surface")
	c.NotStrContains(stdout.String(), "logged out", "a refused logout must not confirm:\n%s", stdout.String())
}
