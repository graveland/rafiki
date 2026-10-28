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
			"credential — a tokenless remote profile is its normal caller.\n\n" +
			"A local callback listener receives the IdP's redirect; if your browser runs " +
			"on another machine, paste the redirect URL instead (--no-browser skips opening " +
			"a browser at all).",
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
// Every message goes to stderr — including the final success line. The verb
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
		loginCallbackPath, begin.Msg.GetAuthorizeUrl(), loginPinnedBy)
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
// A port the daemon or the provider fixed cannot move, so a bind failure says
// who fixed it and offers the way out: paste the redirect URL instead. That
// failure is also what a second login holding the same port looks like, which
// is why the message names the port and not just the system error.
//
// Shutdown is this function's guarantee, not the caller's: no listener is left
// behind when it returns, on success and failure alike.
func loopbackCallback(ctx context.Context, ui loginIO, hosts []string, port uint32, path, authorize, pinnedBy string) (string, error) {
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

	query, err := awaitCallback(ctx, queries, ui.stdin, serveErr, ui.stderr, path)
	// One Server serves every listener, so Shutdown stops all of them (it
	// closes the listeners each Serve call registered) before the query is
	// returned to a caller about to act on it.
	shutdownLoginServer(srv)
	if err != nil {
		return "", err
	}
	return query, nil
}

// listenCallbackHosts binds port on every host in hosts and returns the
// listeners in the order given. An empty host list means the daemon named no
// host, which can only mean this machine's loopback.
//
// A port of 0 asks for any free port, and every host must then share the one
// the first bind picked: listeners on unrelated ephemeral ports would all
// answer, while only one of them can match the redirect URI the caller
// announced. A failure on any host closes what was already bound and reports
// the port it was trying to take.
func listenCallbackHosts(hosts []string, port uint32, pinnedBy string) ([]net.Listener, error) {
	if len(hosts) == 0 {
		hosts = []string{loopbackHost}
	}
	listeners := make([]net.Listener, 0, len(hosts))
	bindPort := port
	for _, host := range hosts {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.FormatUint(uint64(bindPort), 10)))
		if err != nil {
			for _, bound := range listeners {
				_ = bound.Close()
			}
			return nil, fmt.Errorf(
				"cannot listen on callback port %d (%s): %w; free the port, "+
					"or retry with --no-browser and paste the redirect URL",
				bindPort, pinnedBy, err)
		}
		if bindPort == 0 {
			bindPort = uint32(ln.Addr().(*net.TCPAddr).Port)
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
// bounded, and anything still active when it expires is hard-closed — the
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
// listener, or as a URL pasted on stdin. A pasted line is parsed with
// url.Parse and only its RawQuery is used, and only when it carried the
// callback path the daemon asked for: a URL that parses to nothing, or to
// some other path, is warned about and the wait continues: the listener may
// still deliver, and the pasted line may simply have been a stray one. stdin
// EOF only retires the paste path: a --no-browser login behind an ssh -L
// forward has no local stdin, and its listener is exactly what still answers.
func awaitCallback(ctx context.Context, queries <-chan string, stdin io.Reader, serveErr <-chan error, stderr io.Writer, path string) (string, error) {
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
				return "", fmt.Errorf("timed out waiting for the login callback after %s; rerun 'rafiki login'", loginCallbackDeadline)
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
// the user can do nothing about. Start rather than Run — the opener must not
// be waited on.
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
