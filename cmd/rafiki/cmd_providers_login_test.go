// SPDX-License-Identifier: Apache-2.0

package main

// The shared browser-login machinery (loopbackCallback and the wait behind it)
// and the two provider-login verbs. The provider tests drive the real
// `rafiki providers` subtree over the profile's socket, so the endpoint the CLI
// resolves, the Connect verbs it sends and the output contract are all pinned
// together. The callback tests exercise loopbackCallback directly: they need a
// second loopback address no daemon fake has to produce, and a port they can
// hold while the flow binds.

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
	"syscall"
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
// tests assert on the wire rather than on the flow's internals. Configure it
// BEFORE the server starts serving (that goroutine start is the happens-before
// edge for the fields the handlers read under the mutex).
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

// setDeleteErr arms a refusal for the second logout in a test, under the same
// lock the handler reads it with.
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

// freePortFor returns a port nothing is listening on for host, taken from a
// listener that is closed again. Deliberately never 1455: that is the port a
// real ChatGPT login uses, so a developer with `codex login` running would
// otherwise fail this suite.
func freePortFor(t *testing.T, host string) uint32 {
	t.Helper()
	c := assert.NewAborting(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	c.Require().NoError(err, "listen on %s", host)
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	c.Require().NoError(ln.Close(), "close the probe listener")
	return port
}

// freeCallbackPort is freePortFor on this machine's loopback address.
func freeCallbackPort(t *testing.T) uint32 {
	t.Helper()
	return freePortFor(t, loopbackHost)
}

// twoLoopbackHosts returns two DISTINCT bindable loopback IP literals, or skips
// when the platform has only one: the every-host rule needs a second address,
// and a machine that cannot bind one cannot express the test. macOS and Linux
// both offer 127.0.0.1 and ::1.
func twoLoopbackHosts(t *testing.T) (string, string) {
	t.Helper()
	var found []string
	for _, host := range []string{"127.0.0.1", "::1", "127.0.0.2", "127.0.0.3"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			continue
		}
		_ = ln.Close()
		found = append(found, host)
		if len(found) == 2 {
			return found[0], found[1]
		}
	}
	t.Skipf("this platform cannot bind two distinct loopback IPs (bindable: %v)", found)
	return "", ""
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
// the query the listener received, and the confirmation on stderr. It passes
// --no-browser, as every listener-driving test does, so the platform opener is
// never looked up: the flag suppresses the opener and nothing else.
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
// issuer redirects to. The flow still binds the port the daemon named (that is
// what --no-browser leaves alone); nothing delivers a redirect to it here, so
// the paste wins.
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

// TestProvidersLoginShowsTheDaemonsUnavailableMessage pins the ruling that a
// daemon-authored Unavailable keeps its own words. An unwired backend (and,
// later, a daemon with no RAFIKI_SECRETS_KEY) answers Unavailable with the
// reason; the "is rafikid running?" rewrite would send the operator to the
// wrong place.
func TestProvidersLoginShowsTheDaemonsUnavailableMessage(t *testing.T) {
	c := assert.NewAborting(t)
	fake := newFakeProviderLogin()
	fake.beginErr = connect.NewError(connect.CodeUnavailable,
		errors.New("provider login backend not yet wired"))
	serveProvidersScratch(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, _, stderr := providersCLIRoot(t, ctx, strings.NewReader(""), "providers", "login", "chatgpt")
	err := root.Execute()
	c.Require().Error(err, "an unwired backend must fail the verb:\n%s", stderr.String())
	c.StrContains(err.Error(), "provider login backend not yet wired", "the daemon's own message must reach the user")
	c.NotStrContains(err.Error(), "is rafikid running", "the reachability guess must not replace a daemon-authored reason")
}

// TestProvidersLoginKeepsTheReachabilityHintForATransportFailure is the other
// half of that ruling: a dial that never reached a daemon is the client's own
// Unavailable, not the daemon speaking, so the hint stays.
func TestProvidersLoginKeepsTheReachabilityHintForATransportFailure(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	dir, err := os.MkdirTemp("", "rafiki-pl-dead")
	c.Require().NoError(err, "MkdirTemp")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	writeTokenedProfile(t, filepath.Join(dir, "nothing.sock"), "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, _, stderr := providersCLIRoot(t, ctx, strings.NewReader(""), "providers", "login", "chatgpt")
	err = root.Execute()
	c.Require().Error(err, "a dead socket must fail the verb:\n%s", stderr.String())
	c.StrContains(err.Error(), "cannot reach the rafiki daemon", "the reachability hint must survive a transport failure")
	c.StrContains(err.Error(), "is rafikid running", "error text:")
}

// TestLoginLoopbackPortBusyError pins the shared flow's busiest failure: the
// port a provider or the daemon fixed is held (a running `codex login` is the
// usual holder), so the error names the port, who fixed it, and the escape.
// Nothing is printed before the bind succeeds, so no browser is ever sent to a
// listener that is not there. The advice in it is "free the port, then retry",
// because --no-browser still binds this port and would hit the same wall.
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
		providerLoginPinnedBy, providerRetryHint("chatgpt"))
	c.Require().Error(err, "a held callback port must fail")

	msg := err.Error()
	c.StrContains(msg, "cannot listen on callback port "+strconv.FormatUint(uint64(port), 10), "the port must be named")
	c.StrContains(msg, providerLoginPinnedBy, "who fixed the port must be named")
	c.StrContains(msg, "free the port, then retry", "the advice must work under --no-browser too")
	c.NotStrContains(stderr.String(), "open this URL in a browser", "no prompt may be printed before the bind succeeds:\n%s", stderr.String())
}

// TestLoginNoBrowserStillBindsTheCallbackPort pins what --no-browser means: it
// suppresses the platform opener and nothing else. The listener still binds, so
// a held port fails the flow exactly as it does in browser mode, and the opener
// never runs. test/integration/oidc_login_test.go relies on the binding half of
// this for every `rafiki login` it drives.
func TestLoginNoBrowserStillBindsTheCallbackPort(t *testing.T) {
	c := assert.NewAborting(t)
	logPath := stubBrowserOpener(t)
	held, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	c.Require().NoError(err, "listen")
	t.Cleanup(func() { _ = held.Close() })
	port := uint32(held.Addr().(*net.TCPAddr).Port)

	var stderr bytes.Buffer
	_, err = loopbackCallback(context.Background(),
		loginIO{
			noBrowser: true,
			stdin:     strings.NewReader("http://localhost:1/auth/callback?code=pasted&state=s\n"),
			stderr:    &stderr,
		},
		[]string{loopbackHost}, port, "/auth/callback", "https://auth.example.net/oauth/authorize",
		providerLoginPinnedBy, providerRetryHint("chatgpt"))
	c.Require().Error(err, "--no-browser must still bind the callback port, so a held port must fail")
	c.StrContains(err.Error(), "cannot listen on callback port "+strconv.FormatUint(uint64(port), 10),
		"the bind failure must name the port it could not take")
	c.False(strings.Contains(readStubLog(t, logPath), "OPEN-CALLED"),
		"--no-browser must not open a browser (log = %q)", readStubLog(t, logPath))
}

// TestLoginTimeoutNamesTheVerb pins that the timeout suggestion names the verb
// that was running, so a provider login never tells the user to rerun
// `rafiki login`. The wait's own deadline is ten minutes; an already-expired
// context reaches the same branch without waiting for it.
func TestLoginTimeoutNamesTheVerb(t *testing.T) {
	c := assert.NewAborting(t)
	for _, hint := range []string{loginRetryHint, providerRetryHint("chatgpt")} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		var stderr bytes.Buffer
		_, err := awaitCallback(ctx, nil, strings.NewReader(""), nil, &stderr, "/auth/callback", hint)
		cancel()
		c.Require().Error(err, "%s: the wait must not outlive its context", hint)
		c.StrContains(err.Error(), "timed out waiting for the login callback", "%s: error text:", hint)
		c.StrContains(err.Error(), hint, "the timeout must name the verb that was running")
	}
}

// TestLoginLoopbackCallbackBindsEveryHost is the every-host rule the daemon's
// redirect_hosts depends on: the redirect URI names a set of addresses, so a
// flow that bound only the first would leave the redirect unanswered from
// whichever host the browser used. Both are listening here, and the query
// arrives on the SECOND.
func TestLoginLoopbackCallbackBindsEveryHost(t *testing.T) {
	c := assert.NewAborting(t)
	first, second := twoLoopbackHosts(t)
	port := freePortFor(t, first)

	firstAddr := net.JoinHostPort(first, strconv.FormatUint(uint64(port), 10))
	secondAddr := net.JoinHostPort(second, strconv.FormatUint(uint64(port), 10))

	var stderr bytes.Buffer
	type result struct {
		query string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		q, err := loopbackCallback(context.Background(),
			loginIO{noBrowser: true, stdin: strings.NewReader(""), stderr: &stderr},
			[]string{first, second}, port, "/auth/callback",
			"https://auth.example.net/oauth/authorize", providerLoginPinnedBy,
			providerRetryHint("chatgpt"))
		done <- result{q, err}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	// A wrong path proves the FIRST host is listening without consuming the
	// query the second host is about to deliver.
	other := getCallbackWhenServing(t, client, "http://"+firstAddr+"/ping")
	_ = other.Body.Close()
	c.Eq(http.StatusNotFound, other.StatusCode, "the first host must be listening (and 404 a stray path)")

	resp, err := client.Get("http://" + secondAddr + "/auth/callback?code=second&state=s")
	c.Require().NoError(err, "GET the second host")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode, "second-host callback status")
	c.True(strings.Contains(string(body), "close this tab"), "second-host body = %s", body)

	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("loopbackCallback did not return after the query arrived")
	}
	c.Require().NoError(got.err, "loopbackCallback")
	c.Eq("code=second&state=s", got.query, "the query arrived on the second host")
	c.StrContains(stderr.String(), "https://auth.example.net/oauth/authorize", "the authorize URL is printed")
}

// TestLoginLoopbackCallbackPartialBindFailure: a bind that fails on ANY host
// must leave none listening, so a second host taking the port can never leave a
// half-answering redirect URI behind.
func TestLoginLoopbackCallbackPartialBindFailure(t *testing.T) {
	c := assert.NewAborting(t)
	first, second := twoLoopbackHosts(t)
	port := freePortFor(t, first)

	held, err := net.Listen("tcp", net.JoinHostPort(second, strconv.FormatUint(uint64(port), 10)))
	c.Require().NoError(err, "hold the port on the second host")
	t.Cleanup(func() { _ = held.Close() })

	var stderr bytes.Buffer
	_, err = loopbackCallback(context.Background(),
		loginIO{noBrowser: true, stdin: strings.NewReader(""), stderr: &stderr},
		[]string{first, second}, port, "/auth/callback",
		"https://auth.example.net/oauth/authorize", providerLoginPinnedBy,
		providerRetryHint("chatgpt"))
	c.Require().Error(err, "a port held on any host must fail the whole bind")

	msg := err.Error()
	c.StrContains(msg, "cannot listen on callback port "+strconv.FormatUint(uint64(port), 10), "the busy-port text must name the port")
	c.StrContains(msg, providerLoginPinnedBy, "who fixed the port")
	c.StrContains(msg, "free the port, then retry", "the advice")
	c.NotStrContains(stderr.String(), "open this URL in a browser", "nothing is printed before the bind succeeds")

	// The first host's listener must be gone: connection refused, not a timeout,
	// so a leaked listener cannot pass this.
	stale := &http.Client{Timeout: time.Second}
	_, err = stale.Get("http://" + net.JoinHostPort(first, strconv.FormatUint(uint64(port), 10)) + "/auth/callback")
	c.Require().Error(err, "the first host must not still be listening")
	c.True(errors.Is(err, syscall.ECONNREFUSED),
		"want connection refused on the first host %s (err = %v)", first, err)
}

// TestLoginLoopbackCallbackRejectsUnusableTarget covers the callback target
// coming off the wire: a daemon that names no port, no path, or a routable host
// is refused before anything is bound or printed, with an error that says which.
func TestLoginLoopbackCallbackRejectsUnusableTarget(t *testing.T) {
	c := assert.NewAborting(t)
	port := freeCallbackPort(t)
	cases := []struct {
		name  string
		hosts []string
		port  uint32
		path  string
		want  string
	}{
		{name: "no port", hosts: []string{loopbackHost}, port: 0, path: "/auth/callback", want: "no callback port"},
		{name: "no path", hosts: []string{loopbackHost}, port: port, path: "", want: "no callback path"},
		{name: "routable host", hosts: []string{"10.0.0.1"}, port: port, path: "/auth/callback", want: "not a loopback IP"},
		{name: "hostname not IP", hosts: []string{"auth.example.net"}, port: port, path: "/auth/callback", want: "not a loopback IP"},
	}
	for _, tc := range cases {
		var stderr bytes.Buffer
		_, err := loopbackCallback(context.Background(),
			loginIO{stdin: strings.NewReader(""), stderr: &stderr},
			tc.hosts, tc.port, tc.path, "https://auth.example.net/oauth/authorize",
			providerLoginPinnedBy, providerRetryHint("chatgpt"))
		c.Require().Error(err, "%s: a rejected target must fail", tc.name)
		c.StrContains(err.Error(), tc.want, "%s: error text:", tc.name)
		c.NotStrContains(stderr.String(), "open this URL in a browser",
			"%s: nothing may be printed for a rejected target", tc.name)
	}
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
