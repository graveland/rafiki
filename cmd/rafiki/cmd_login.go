// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"
)

// loginCallbackDeadline bounds the wait for the IdP's redirect, however it
// arrives. Ten minutes: an SSO round trip through a corporate IdP (MFA, device
// approval) can take minutes, but a flow that waits forever leaks the listener
// and holds the terminal open with no way out but the signal.
const loginCallbackDeadline = 10 * time.Minute

// loginShutdownGrace bounds the graceful stop of the callback server: long
// enough for the confirmation page's handler to finish and its connection to
// go idle, short enough that a wedged client cannot stall the flow's exit.
const loginShutdownGrace = 2 * time.Second

// loginCallbackPath is the path the daemon builds into the IdP's redirect_uri.
const loginCallbackPath = "/oidc/callback"

// loopbackHost is the address a callback listener binds when nothing names a
// host. An IP literal, never a name: the bind must not be redirectable by a
// hosts file.
const loopbackHost = "127.0.0.1"

// loginPinnedBy is why `rafiki login`'s callback port cannot move: the IdP's
// redirect URI is registered against the daemon's oidc.toml, so only that port
// can receive the redirect. It is the parenthetical of a bind failure.
const loginPinnedBy = "pinned by the daemon's oidc.toml"

// loginRetryHint is the suggestion a timed-out login prints. It names the verb
// that was running, so a provider login never tells the user to rerun
// `rafiki login`.
const loginRetryHint = "rerun 'rafiki login'"

// loginPastePrompt is the stderr prompt shown while waiting. The listener
// covers a browser on this machine; a browser on another one (an ssh -L
// forward that lands elsewhere, a headless box) still delivers the redirect
// query by paste.
const loginPastePrompt = "…or paste the URL your browser was redirected to:"

// loginIO is the flow's outside world: where prompts and progress go, where a
// pasted redirect URL comes from, and whether a browser may be opened. stdin
// and stderr ride it so tests can drive the flow without a terminal. Both
// browser-login flows (`rafiki login` and `rafiki providers login`) take it.
type loginIO struct {
	// noBrowser suppresses the platform opener and nothing else: the callback
	// listener still binds, which is what lets a headless run deliver the
	// redirect itself (or receive it through an ssh -L forward) while a test
	// stays clear of a real browser.
	noBrowser bool
	stdin     io.Reader
	stderr    io.Writer
}

// newLoginCmd builds `rafiki login`.
func newLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to a rafiki daemon with SSO and store the token in the profile",
		Long: "Authenticates through the daemon's OIDC provider and writes the minted " +
			"token to the profile. The daemon is reached exactly like every other verb " +
			"(the profile's socket or url), but the Login service itself needs no " +
			"credential: a tokenless remote profile is its normal caller.\n\n" +
			"A local callback listener receives the IdP's redirect; if your browser runs " +
			"on another machine, paste the redirect URL instead (--no-browser skips opening " +
			"a browser, but still binds the callback port).",
		Args: cobra.NoArgs,
		RunE: runLogin,
	}
	cmd.Flags().Bool("no-browser", false,
		"do not open a browser; print the authorize URL and wait for the pasted redirect URL")
	return cmd
}

// runLogin resolves the profile (for the token destination) and the Login
// endpoint, then runs the flow. resolveProfile is memoized per process, so the
// profile loginFlow writes to is exactly the one newLoginEndpoint resolved.
func runLogin(cmd *cobra.Command, _ []string) error {
	p, err := resolveProfile(cmd)
	if err != nil {
		return err
	}
	ep, err := newLoginEndpoint(cmd)
	if err != nil {
		return err
	}
	noBrowser, _ := cmd.Flags().GetBool("no-browser")
	return loginFlow(cmdCtx(cmd), ep, p, loginIO{
		noBrowser: noBrowser,
		stdin:     cmd.InOrStdin(),
		stderr:    cmd.ErrOrStderr(),
	})
}

// loginFlow runs the OIDC login: ask the daemon for an authorize URL, collect
// the IdP's redirect query from a local callback listener or a pasted URL, and
// exchange it for a token stored in the profile.
//
// The port is settled in two steps because the daemon builds the redirect_uri
// from a port the CLIENT announces: one has to be bound before BeginLogin so
// there is a number to announce, and the daemon may then pin a different one
// (the IdP's registered redirect_uri is what pinned it). So the first bind is
// a probe: it holds the announced port open across the round trip, carries no
// server, and is closed before loopbackCallback binds, which is what keeps the
// two from racing for the same port.
//
// Every message goes to stderr, including the final success line. The verb
// writes a token file, not command output; stdout stays silent so the flow can
// be observed with 2>&1 | tee without the URL or token summary reading as data.
//
// Shutdown: the flow's only listener belongs to loopbackCallback, which stops
// it before returning on every path (see shutdownLoginServer); the probe above
// is closed on both of its exits (BeginLogin error, successful round trip).
func loginFlow(ctx context.Context, ep connectEndpoint, p profile.Resolved, ui loginIO) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		return fmt.Errorf("listen for the login callback: %w", err)
	}
	bound := uint32(ln.Addr().(*net.TCPAddr).Port)

	begin, err := ep.login().BeginLogin(ctx, connect.NewRequest(&rafikiv1.BeginLoginRequest{
		RedirectPort: bound,
		ClientHost:   clientHost(),
	}))
	if err != nil {
		_ = ln.Close()
		return diagnoseConnectError(err, ep.describe)
	}

	// A RedirectPort of 0 is no pin at all (port 0 is not a listenable
	// address): the announced port is the one to keep.
	callbackPort := bound
	if pin := begin.Msg.GetRedirectPort(); pin != 0 {
		callbackPort = pin
	}
	_ = ln.Close()

	query, err := loopbackCallback(ctx, ui, []string{loopbackHost}, callbackPort,
		loginCallbackPath, begin.Msg.GetAuthorizeUrl(), loginPinnedBy, loginRetryHint)
	if err != nil {
		return err
	}

	complete, err := completeLogin(ctx, ep, p, begin.Msg.GetLoginId(), query)
	if err != nil {
		return err
	}
	fmt.Fprintf(ui.stderr, "logged in as %s (profile %s), token expires %s\n",
		complete.GetUsername(), p.Name, tokenExpiryText(complete.GetExpiresAt()))
	return nil
}

// loopbackCallback listens on port at path on every host in hosts (an
// IP literal each), prints authorize (and opens a browser unless
// ui.noBrowser), and returns the first callback query received or a pasted
// redirect URL's query.
//
// A port the daemon or the provider fixed cannot move, so a bind failure names
// the port and who fixed it; the way out is to free the port and retry, or to
// finish the flow from a browser elsewhere by pasting the redirect URL. That
// failure is also what a second login holding the same port looks like, which
// is why the message carries more than the system error.
//
// ui.noBrowser suppresses ONLY the platform opener: the listener binds either
// way, because the redirect URI is the one the daemon announced (test/
// integration/oidc_login_test.go depends on exactly this, delivering the
// redirect into a --no-browser flow).
//
// Shutdown is this function's guarantee, not the caller's: no listener is left
// behind when it returns, on success and failure alike.
func loopbackCallback(ctx context.Context, ui loginIO, hosts []string, port uint32, path, authorize, pinnedBy, retryHint string) (string, error) {
	hosts = callbackHosts(hosts)
	if err := validateCallbackTarget(hosts, port, path); err != nil {
		return "", err
	}

	listeners, err := listenCallbackHosts(hosts, port, pinnedBy)
	if err != nil {
		return "", err
	}

	// Cap 1 and a first-request-only send: a browser tab refresh or retry must
	// not overwrite the query the completion call will consume.
	queries := make(chan string, 1)
	srv := &http.Server{Handler: loginCallbackHandler(path, queries)}
	serveErr := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(ln net.Listener) { serveErr <- srv.Serve(ln) }(ln)
	}

	fmt.Fprintf(ui.stderr, "open this URL in a browser to log in:\n%s\n", authorize)
	if !ui.noBrowser {
		openBrowser(authorize)
	}
	fmt.Fprintln(ui.stderr, loginPastePrompt)

	query, err := awaitCallback(ctx, queries, ui.stdin, serveErr, ui.stderr, path, retryHint)
	// One Server serves every listener, so Shutdown stops all of them (it
	// closes the listeners each Serve call registered) before the query is
	// returned to a caller about to act on it.
	shutdownLoginServer(srv)
	if err != nil {
		return "", err
	}
	return query, nil
}

// callbackHosts fills in the loopback address when the daemon named no host.
func callbackHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return []string{loopbackHost}
	}
	return hosts
}

// validateCallbackTarget refuses a callback the client cannot safely serve,
// before anything is bound or printed. These values come off the wire, so a
// broken daemon must not be able to make this process listen anywhere it should
// not, take a port no issuer would ever redirect to, or wait for a redirect at
// a path it can never receive.
func validateCallbackTarget(hosts []string, port uint32, path string) error {
	if port == 0 {
		return errors.New("the daemon sent no callback port: the port its redirect URI was registered with must be named")
	}
	if path == "" {
		return errors.New("the daemon sent no callback path: the path its redirect URI was registered with must be named")
	}
	for _, host := range hosts {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf(
				"the daemon sent callback host %q, which is not a loopback IP: a login callback is only ever served on loopback",
				host)
		}
	}
	return nil
}

// listenCallbackHosts binds port on every host in hosts and returns the
// listeners in the order given. Every host gets a listener: redirect_hosts is
// the set of addresses the issuer may send the browser back to, so binding one
// and skipping the rest would leave the redirect unanswered from whichever host
// the browser used.
//
// A failure on any host closes what was already bound and reports the port it
// was trying to take, so a partial bind never leaves a listener behind.
func listenCallbackHosts(hosts []string, port uint32, pinnedBy string) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(hosts))
	for _, host := range hosts {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10)))
		if err != nil {
			for _, bound := range listeners {
				_ = bound.Close()
			}
			// The suffix is the advice that is TRUE: --no-browser still binds
			// this port, so suggesting it would send the user into the same
			// wall. Amended by ruling from the brief's original wording.
			return nil, fmt.Errorf(
				"cannot listen on callback port %d (%s): %w; free the port, then retry",
				port, pinnedBy, err)
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

// completeLogin exchanges the IdP's callback query for a token, writes the
// token to the profile, and returns the daemon's response. It is the token
// exchange both `rafiki login` and `rafiki web` share: the difference between
// them is only what they do with the result, so the refusal of an empty token
// and the profile write live here, once.
func completeLogin(ctx context.Context, ep connectEndpoint, p profile.Resolved, loginID, query string) (*rafikiv1.CompleteLoginResponse, error) {
	resp, err := ep.login().CompleteLogin(ctx, connect.NewRequest(&rafikiv1.CompleteLoginRequest{
		LoginId:       loginID,
		CallbackQuery: query,
	}))
	if err != nil {
		return nil, diagnoseConnectError(err, ep.describe)
	}
	if resp.Msg.GetToken() == "" {
		return nil, errors.New("the daemon completed the login but returned no token; try again")
	}
	if err := profile.WriteToken(p.Name, resp.Msg.GetToken()); err != nil {
		return nil, fmt.Errorf("write token to profile %q: %w", p.Name, err)
	}
	return resp.Msg, nil
}

// shutdownLoginServer stops the callback server. Shutdown is preferred over a
// bare Close: it closes the listener at once (new connections refused) but
// lets the in-flight confirmation page finish, so the browser actually sees
// "you can close this tab" instead of a reset connection. The grace is
// bounded, and anything still active when it expires is hard-closed, so the
// guarantee the flow relies on (nothing left listening once the wait is over)
// holds either way. Errors are deliberately dropped: the server's only job is
// done, or the flow is leaving regardless.
func shutdownLoginServer(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), loginShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

// awaitCallback waits for the redirect query to arrive: over the local
// listener, or as a URL pasted on stdin. A nil queries or serveErr channel
// never fires, which is how a caller (or a test) waits on the paste path and
// the deadline alone.
//
// A pasted line is parsed with url.Parse and only its RawQuery is used, and
// only when it carried the callback path the daemon asked for: a URL that
// parses to nothing, or to some other path, is warned about and the wait
// continues, because the listener may still deliver and the pasted line may
// simply have been a stray one. stdin EOF only retires the paste path: a
// browser-mode login behind an ssh -L forward has no local stdin, and its
// listener is exactly what still answers.
func awaitCallback(ctx context.Context, queries <-chan string, stdin io.Reader, serveErr <-chan error, stderr io.Writer, path, retryHint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, loginCallbackDeadline)
	defer cancel()

	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				lines <- line
			}
		}
		close(lines)
	}()

	for {
		select {
		case q := <-queries:
			return q, nil
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			u, err := url.Parse(line)
			if err != nil || u.RawQuery == "" || u.Path != path {
				fmt.Fprintf(stderr, "ignoring %q: not a login redirect URL\n", line)
				continue
			}
			return u.RawQuery, nil
		case err := <-serveErr:
			return "", fmt.Errorf("callback listener failed: %w", err)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("timed out waiting for the login callback after %s; %s", loginCallbackDeadline, retryHint)
			}
			return "", errors.New("login canceled")
		}
	}
}

// loginCallbackHandler answers the redirect at path. The first GET's query is
// handed to the flow; later ones are answered politely but ignored, because the
// completion call takes one query. Any other path 404s, so a stray port scan
// or a misdirected ssh -L forward reads as what it is.
func loginCallbackHandler(path string, queries chan<- string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		// The confirmation page is written AND flushed before the query is
		// handed to the flow: the flow stops the server the moment it receives
		// the query, and a signal written before the response could let that
		// stop truncate the page the browser is still reading.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><p>rafiki: login received — you can close this tab.</p></body></html>")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case queries <- r.URL.RawQuery:
		default:
		}
	})
}

// openBrowser tries to open rawURL with the platform's opener and reports
// nothing: a missing xdg-open (headless server) or a refused launch is exactly
// the situation the pasted-URL path exists for, and it is not worth an error
// the user can do nothing about. Start rather than Run, because the opener must
// not be waited on.
func openBrowser(rawURL string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, rawURL).Start()
}

// clientHost is the hostname the daemon folds into the redirect_uri. An
// unresolvable hostname is "", which the daemon treats as localhost.
func clientHost() string {
	h, _ := os.Hostname()
	return h
}

// tokenExpiryText renders the token's expiry in local time; unset or the
// epoch means the token never expires, which is what a daemon-issued token
// with no TTL reports.
func tokenExpiryText(expires *timestamppb.Timestamp) string {
	if expires == nil || expires.AsTime().Unix() <= 0 {
		return "never"
	}
	return expires.AsTime().Local().Format("2006-01-02 15:04")
}
