package integration_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// End-to-end OIDC login: a real daemon, the real `rafiki login` CLI binary,
// and the fake IdP from oidc_fake_issuer_test.go. Every test drives the full
// flow — authorize URL from BeginLogin, the redirect into the CLI's loopback
// listener (or a pasted redirect URL), CompleteLogin, token minted into the
// profile — exactly as the task brief describes.
//
// The no-browser invariant: EVERY `rafiki login` invocation in this file goes
// through cliLoginCmd, which refuses to build the invocation without
// --no-browser and guts PATH so the platform opener (open/xdg-open) cannot be
// looked up at all. In --no-browser mode the CLI's loopback listener still
// runs — that listener is what the Loopback tests deliver the redirect into —
// so nothing here needs a browser, and a regression that dropped the flag
// would fail as a binary-lookup error, never by opening one.

// oidcTestDomain is the email domain every daemon's oidc.toml accepts.
const oidcTestDomain = "example.test"

// oidcTestTokenLSDeadline bounds a `rafiki token ls`: the minted token
// authenticates immediately, so anything longer means the flow failed.
const oidcTestTokenLSDeadline = 15 * time.Second

// oidcLoginSettle bounds how long a started `rafiki login` may take to finish
// after its callback query has been delivered. The flow is local HTTP plus a
// token exchange against the fake IdP; thirty seconds is generous under -race.
const oidcLoginSettle = 30 * time.Second

// oidcAuthorizeWait bounds how long the CLI has to print its authorize URL.
const oidcAuthorizeWait = 15 * time.Second

// authorizeURLRe extracts the authorize URL from the CLI's stderr, which
// prints it as the line after "open this URL in a browser to log in:".
var authorizeURLRe = regexp.MustCompile(`(?m)^open this URL in a browser to log in:\n(\S+)`)

// browserClient is the test's stand-in for the browser: it fetches the
// authorize URL and (only where a test says so) follows the IdP's 302. The
// timeout bounds the whole drive; every hop in it is loopback.
func browserClient(follow bool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			if follow {
				return nil
			}
			return http.ErrUseLastResponse
		},
	}
}

// bootOIDCDaemon boots a DB-backed daemon whose oidc.toml points at issuer
// with the given client secret, under the env var client_secret_env names.
// oidcExtra splices extra oidc.toml lines (a session_ttl, say) into the file.
// The config tree lives under a dedicated XDG_CONFIG_HOME, appended after the
// harness defaults so it wins, because the daemon loads oidc.toml from that
// directory at startup.
func bootOIDCDaemon(t *testing.T, issuer, secret, oidcExtra string, extraEnv ...string) *daemon {
	t.Helper()
	c := assert.NewAborting(t)

	configHome := t.TempDir()
	rafikiDir := filepath.Join(configHome, "rafiki")
	c.NoError(os.MkdirAll(rafikiDir, 0o700), "mkdir %s", rafikiDir)
	body := "issuer = " + fmt.Sprintf("%q", issuer) + "\n" +
		"client_id = " + fmt.Sprintf("%q", oidcTestClientID) + "\n" +
		"client_secret_env = " + fmt.Sprintf("%q", oidcTestSecretEnv) + "\n" +
		"email_domains = [" + fmt.Sprintf("%q", oidcTestDomain) + "]\n" +
		oidcExtra
	c.NoError(os.WriteFile(filepath.Join(rafikiDir, "oidc.toml"), []byte(body), 0o600), "write oidc.toml")

	env := append(noRealProviderEnv(),
		"XDG_CONFIG_HOME="+configHome,
		oidcTestSecretEnv+"="+secret,
	)
	return bootDaemonDB(t, nextDaemonID(), append(env, extraEnv...)...)
}

// createOIDCUser runs the recovery-path `rafikid user create` against the
// suite's shared test database — no running daemon is contacted. No token is
// minted: these users authenticate only through the OIDC flow under test.
func createOIDCUser(t *testing.T, name, email string) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	c.NotEq("", dsn, "RAFIKI_TEST_DSN is required: rafikid user create opens the database directly")
	cmd := exec.Command(daemonBinary(), "user", "--db", dsn, "create", name, "--email", email)
	out, err := cmd.CombinedOutput()
	c.NoError(err, "rafikid user create %s --email %s failed: %s", name, email, out)
}

// oidcUser is a per-run unique identity: the suite's database persists across
// runs, so both the username and the email must be fresh every time (the
// emails are unique among active users).
func oidcUser(prefix string) (name, email string) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	return prefix + "-" + suffix, prefix + "-" + suffix + "@" + oidcTestDomain
}

// cliLoginCmd builds a `rafiki login` invocation with the two structural
// no-browser guarantees:
//
//   - it refuses to build the invocation at all unless the caller passed
//     --no-browser, so no test in this file can regress into the default;
//   - it guts PATH, so even if a code change ignored the flag, the platform
//     opener (`open` on darwin, `xdg-open` elsewhere) fails exec.LookPath and
//     the CLI's openBrowser drops that error silently — no browser starts.
//
// The config dir is caller-owned: every login writes its token into THAT
// profile, so follow-up verbs and the token-file assertions read the same
// directory.
func cliLoginCmd(t *testing.T, d *daemon, configDir string, args ...string) *exec.Cmd {
	t.Helper()
	if !slices.Contains(args, "--no-browser") {
		t.Fatal("integration logins must pass --no-browser: a test must never open a browser")
	}
	cmd := cliCmdIn(t, d, configDir, append([]string{"login"}, args...)...)
	cmd.Env = append(cmd.Env, "PATH=/nonexistent-rafiki-test-path")
	return cmd
}

// oidcTokenPath is the profile token file the login flow writes: profile "it"
// (writeCliProfile's manifest), under the config dir the login ran with.
func oidcTokenPath(configDir string) string {
	return filepath.Join(configDir, "rafiki", "profiles", "it", "token")
}

// loginRun is one `rafiki login --no-browser` in flight: its stderr buffer
// (the authorize URL and the flow's errors appear there), its optional stdin
// (the paste-back path), and the exit waiter.
type loginRun struct {
	stderr *stderrBuf
	stdin  io.WriteCloser // nil unless startLogin was asked for a paste pipe
	done   <-chan error
}

// startLogin starts the CLI. withStdin pipes the process's stdin so a test can
// paste the redirect URL after it has fetched the authorize response;
// otherwise stdin is the null device (an immediate EOF, which retires only the
// paste path — the loopback listener keeps waiting). extraEnv entries are
// appended after the harness's env, so later entries win.
func startLogin(t *testing.T, d *daemon, configDir string, withStdin bool, extraEnv ...string) *loginRun {
	t.Helper()
	c := assert.NewAborting(t)

	cmd := cliLoginCmd(t, d, configDir, "--no-browser")
	run := &loginRun{stderr: &stderrBuf{}}
	cmd.Stderr = run.stderr
	cmd.Env = append(cmd.Env, extraEnv...)
	if withStdin {
		p, err := cmd.StdinPipe()
		c.NoError(err, "open the login's stdin pipe")
		run.stdin = p
	}
	c.NoError(cmd.Start(), "start rafiki login --no-browser")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	run.done = done
	return run
}

// authorizeURL waits for the CLI to print its authorize URL. It also fails
// early when the CLI exits first — a daemon that refused BeginLogin must
// surface its stderr, not a timeout.
func (r *loginRun) authorizeURL(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(oidcAuthorizeWait)
	for time.Now().Before(deadline) {
		if m := authorizeURLRe.FindStringSubmatch(r.stderr.String()); m != nil {
			return m[1]
		}
		select {
		case err := <-r.done:
			t.Fatalf("rafiki login exited before printing the authorize URL: %v\nstderr:\n%s", err, r.stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("rafiki login never printed the authorize URL within %v\nstderr:\n%s", oidcAuthorizeWait, r.stderr.String())
	return ""
}

// finish waits for the CLI to exit after the callback query has been
// delivered, failing the test with the stderr tail if the flow wedges.
func (r *loginRun) finish(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(oidcLoginSettle):
		t.Fatalf("rafiki login did not exit within %v of the callback being delivered\nstderr:\n%s", oidcLoginSettle, r.stderr.String())
		return nil
	}
}

// loginOutcome is what a driven login returns: the CLI's exit error and its
// full stderr (the flow's messages, including any daemon refusal, land there).
type loginOutcome struct {
	err    error
	stderr string
}

// runLoginLoopback drives a login the way a browser on the same machine would:
// GET the authorize URL with a redirect-following client, so the fake IdP's
// 302 lands on the CLI's own loopback listener and the flow completes without
// anything pasted.
func runLoginLoopback(t *testing.T, d *daemon, configDir string, extraEnv ...string) loginOutcome {
	t.Helper()
	c := assert.NewAborting(t)
	run := startLogin(t, d, configDir, false, extraEnv...)
	resp, err := browserClient(true).Get(run.authorizeURL(t))
	c.NoError(err, "GET the authorize URL, following the IdP's redirect into the CLI's listener")
	c.Eq(http.StatusOK, resp.StatusCode, "the CLI's callback confirmation page")
	c.NoError(resp.Body.Close())
	return loginOutcome{err: run.finish(t), stderr: run.stderr.String()}
}

// runLoginPasteBack drives a login the way a browser on ANOTHER machine would:
// fetch the authorize response WITHOUT following the redirect, then hand the
// Location — the raw callback URL — to the CLI's stdin.
func runLoginPasteBack(t *testing.T, d *daemon, configDir string) loginOutcome {
	t.Helper()
	c := assert.NewAborting(t)
	run := startLogin(t, d, configDir, true)
	authURL := run.authorizeURL(t)

	resp, err := browserClient(false).Get(authURL)
	c.NoError(err, "GET the authorize URL without following the redirect")
	c.Eq(http.StatusFound, resp.StatusCode, "the IdP's 302 must not be followed here")
	location := resp.Header.Get("Location")
	c.NoError(resp.Body.Close())
	c.StrContains(location, "code=", "the IdP's redirect must carry the authorization code")

	_, err = io.WriteString(run.stdin, location+"\n")
	c.NoError(err, "paste the redirect URL into the CLI's stdin")
	c.NoError(run.stdin.Close(), "close the stdin pipe")
	return loginOutcome{err: run.finish(t), stderr: run.stderr.String()}
}

// runTokenLS runs `rafiki token ls` against d with the config dir's profile
// and returns its combined output plus the process's error. It is the Control
// RPC the tests use to prove (or disprove) that the profile's token
// authenticates.
func runTokenLS(t *testing.T, d *daemon, configDir string, extraEnv ...string) (string, error) {
	t.Helper()
	cmd := cliCmdIn(t, d, configDir, "token", "ls")
	cmd.Env = append(cmd.Env, extraEnv...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return out.String(), err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(oidcTestTokenLSDeadline):
		_ = cmd.Process.Kill()
		t.Fatalf("rafiki token ls did not finish within %v", oidcTestTokenLSDeadline)
		return out.String(), nil
	}
}

// TestOIDCLoginLoopback is the happy path with the redirect delivered by the
// loopback listener: user created via the recovery path, `rafiki login
// --no-browser`, the test plays the browser (GET the authorize URL, let the
// redirect follow into the CLI), the CLI exits 0, and the minted token
// authenticates a Control RPC from the profile.
func TestOIDCLoginLoopback(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	idp := newFakeIssuer(t, "loopback-test-secret")
	d := bootOIDCDaemon(t, idp.URL(), "loopback-test-secret", "")
	name, email := oidcUser("alice")
	createOIDCUser(t, name, email)
	idp.setClaims(fakeUserClaims{Sub: "sub-" + name, Email: email, EmailVerified: boolPtr(true)})

	configDir := t.TempDir()
	login := runLoginLoopback(t, d, configDir)
	c.NoError(login.err, "rafiki login --no-browser exited non-zero; stderr:\n%s", login.stderr)
	c.FileExists(oidcTokenPath(configDir), "the login wrote no token into the profile")

	out, lsErr := runTokenLS(t, d, configDir)
	c.NoError(lsErr, "token ls must authenticate with the minted token; output: %s", out)
	c.StrContains(out, name, "token ls should list the user's token")
	c.StrContains(out, "oidc", "the minted token's origin is oidc")
}

// TestOIDCLoginPasteBack covers the other half of the split: the same
// --no-browser flow, but the redirect query arrives as a PASTED URL on stdin
// and the CLI's loopback listener receives nothing at all.
func TestOIDCLoginPasteBack(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	idp := newFakeIssuer(t, "pasteback-test-secret")
	d := bootOIDCDaemon(t, idp.URL(), "pasteback-test-secret", "")
	name, email := oidcUser("paste")
	createOIDCUser(t, name, email)
	idp.setClaims(fakeUserClaims{Sub: "sub-" + name, Email: email, EmailVerified: boolPtr(true)})

	configDir := t.TempDir()
	login := runLoginPasteBack(t, d, configDir)
	c.NoError(login.err, "pasted-redirect login exited non-zero; stderr:\n%s", login.stderr)

	out, lsErr := runTokenLS(t, d, configDir)
	c.NoError(lsErr, "token ls must authenticate with the minted token; output: %s", out)
	c.StrContains(out, name, "token ls should list the user's token")
}

// TestOIDCLoginExpiredThenRelogin exercises the expiry story end to end: a
// session_ttl of 2s, a login, a successful Control RPC, then — once the token
// is past its expires_at AND the daemon's 5s auth cache has drained (7s
// total) — the same RPC refuses Unauthenticated with the CLI's 'rafiki login'
// hint, and a SECOND login succeeds while the expired token is still in the
// profile, replacing it with one that authenticates again.
func TestOIDCLoginExpiredThenRelogin(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	idp := newFakeIssuer(t, "expired-test-secret")
	d := bootOIDCDaemon(t, idp.URL(), "expired-test-secret", "session_ttl = \"2s\"\n")
	name, email := oidcUser("expire")
	createOIDCUser(t, name, email)
	idp.setClaims(fakeUserClaims{Sub: "sub-" + name, Email: email, EmailVerified: boolPtr(true)})

	configDir := t.TempDir()
	first := runLoginLoopback(t, d, configDir)
	c.NoError(first.err, "first login failed; stderr:\n%s", first.stderr)

	tokenPath := oidcTokenPath(configDir)
	firstToken, err := os.ReadFile(tokenPath)
	c.NoError(err, "read the minted token")

	out, lsErr := runTokenLS(t, d, configDir)
	c.NoError(lsErr, "token ls must authenticate immediately after login; output: %s", out)

	// TTL (2s) + the daemon's auth cache (5s): inside that window a cached
	// identity can still answer for an expired token, so the refusal is only
	// guaranteed after both have drained.
	time.Sleep(7 * time.Second)

	out, lsErr = runTokenLS(t, d, configDir)
	c.Error(lsErr, "token ls must fail once the token expired; output: %s", out)
	c.StrContains(out, "run 'rafiki login'", "the refusal must carry the CLI's login hint")

	c.FileExists(tokenPath, "the expired token must still be in the profile before the re-login")
	again := runLoginLoopback(t, d, configDir)
	c.NoError(again.err, "re-login with an expired token in the profile failed; stderr:\n%s", again.stderr)

	secondToken, err := os.ReadFile(tokenPath)
	c.NoError(err, "read the re-login's token")
	c.NotEq(strings.TrimSpace(string(firstToken)), strings.TrimSpace(string(secondToken)), "the re-login must replace the expired token")

	out, lsErr = runTokenLS(t, d, configDir)
	c.NoError(lsErr, "token ls must authenticate with the re-login's token; output: %s", out)
}

// TestOIDCLoginWrongDomain: the IdP asserts an email outside email_domains.
// The daemon refuses before resolving any user; the CLI exits non-zero with
// the daemon's reason on stderr and writes no token.
func TestOIDCLoginWrongDomain(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	idp := newFakeIssuer(t, "wrongdomain-test-secret")
	d := bootOIDCDaemon(t, idp.URL(), "wrongdomain-test-secret", "")
	// No user is created: the domain check fires before identity resolution.
	idp.setClaims(fakeUserClaims{
		Sub:           "sub-wrong-domain",
		Email:         "mallory@evil.test",
		EmailVerified: boolPtr(true),
	})

	configDir := t.TempDir()
	login := runLoginLoopback(t, d, configDir)
	c.Error(login.err, "a login at a refused domain must exit non-zero")
	c.StrContains(login.stderr, "is not accepted by this daemon", "the daemon's domain refusal must reach stderr")
	c.NoFileExists(oidcTokenPath(configDir), "a refused login must not write a token")
}

// TestOIDCLoginUnknownUser: the claims are fine but resolve to nobody. The
// daemon refuses with its "no rafiki user for <email>" message; no token is
// written.
func TestOIDCLoginUnknownUser(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	idp := newFakeIssuer(t, "unknownuser-test-secret")
	d := bootOIDCDaemon(t, idp.URL(), "unknownuser-test-secret", "")
	_, nobody := oidcUser("nobody")
	idp.setClaims(fakeUserClaims{Sub: "sub-unknown-user", Email: nobody, EmailVerified: boolPtr(true)})

	configDir := t.TempDir()
	login := runLoginLoopback(t, d, configDir)
	c.Error(login.err, "a login that resolves to no user must exit non-zero")
	c.StrContains(login.stderr, "no rafiki user for "+nobody, "the daemon's no-user refusal must reach stderr")
	c.NoFileExists(oidcTokenPath(configDir), "a refused login must not write a token")
}

// TestOIDCLoginUnconfigured: no oidc.toml at all — BeginLogin answers
// FailedPrecondition, the CLI exits non-zero with "not configured" on stderr,
// and the flow never reaches a token file.
func TestOIDCLoginUnconfigured(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	configDir := t.TempDir()
	cmd := cliLoginCmd(t, d, configDir, "--no-browser")
	out, err := cmd.CombinedOutput()
	c.Error(err, "login without oidc.toml must exit non-zero")
	c.StrContains(string(out), "not configured", "the unconfigured refusal must name the situation")
	c.NoFileExists(oidcTokenPath(configDir), "an unconfigured daemon must not mint a token")
}

// TestOIDCLoginRemoteNoToken: a remote (TLS) profile with NO token — the
// normal state of a machine that has not logged in yet — completes the whole
// flow over the daemon's TLS listener, because Login mounts outside the
// proxy-face credential middleware on both planes. The harness CAN run the
// TLS listener (RAFIKI_CONTROL_LISTEN with a self-signed cert, the same
// posture bootGrantDaemon uses), so this runs rather than skips; it skips
// only when the harness cannot boot a DB-backed daemon at all.
func TestOIDCLoginRemoteNoToken(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	idp := newFakeIssuer(t, "remote-test-secret")
	certPath, keyPath, _ := grantCert(t, t.TempDir())

	// A port for the TLS listener, picked while free like bootGrantDaemon
	// does: the daemon cannot announce the resolved port of a :0 listen.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err, "pick a free port for the TLS listener")
	tlsAddr := probe.Addr().String()
	c.NoError(probe.Close(), "free the probe port")

	d := bootOIDCDaemon(t, idp.URL(), "remote-test-secret", "",
		"RAFIKI_CONTROL_LISTEN="+tlsAddr,
		"RAFIKI_CONTROL_TLS_CERT="+certPath,
		"RAFIKI_CONTROL_TLS_KEY="+keyPath,
	)
	waitTLSAccepting(t, tlsAddr)

	name, email := oidcUser("remote")
	createOIDCUser(t, name, email)
	idp.setClaims(fakeUserClaims{Sub: "sub-" + name, Email: email, EmailVerified: boolPtr(true)})

	configDir := t.TempDir()
	writeCliRemoteProfile(t, configDir, "https://"+tlsAddr)

	// The harness's TLS cert is self-signed, so the CLI needs it in its root
	// pool: SSL_CERT_FILE is honored by crypto/x509 everywhere Go runs, and
	// cliCmd does not blank it.
	sslEnv := "SSL_CERT_FILE=" + certPath
	login := runLoginLoopback(t, d, configDir, sslEnv)
	c.NoError(login.err, "tokenless remote login over TLS failed; stderr:\n%s", login.stderr)
	c.FileExists(oidcTokenPath(configDir), "the remote login wrote no token")

	out, lsErr := runTokenLS(t, d, configDir, sslEnv)
	c.NoError(lsErr, "token ls must authenticate with the minted token over the TLS face; output: %s", out)
	c.StrContains(out, name, "token ls should list the user's token")
}

// writeCliRemoteProfile writes a manifest whose profile points at a remote
// https daemon — writeCliProfile only writes the socket form, and a remote
// profile is exactly what Login exists to bootstrap (no token needed).
func writeCliRemoteProfile(t *testing.T, configDir, tlsURL string) {
	t.Helper()
	c := assert.NewAborting(t)
	dir := filepath.Join(configDir, "rafiki")
	c.NoError(os.MkdirAll(dir, 0o700), "mkdir %s", dir)
	manifest := fmt.Sprintf("[profile.it]\nurl = %q\n", tlsURL)
	c.NoError(os.WriteFile(filepath.Join(dir, "profiles.toml"), []byte(manifest), 0o600), "write profiles.toml")
	c.NoError(os.WriteFile(filepath.Join(dir, "current-profile"), []byte("it\n"), 0o600), "write current-profile")
}

// waitTLSAccepting dials the daemon's TLS listener until it accepts, the same
// proof-by-connecting bootDaemonDB insists on for the UDS. tls.Listen happens
// only after the daemon loaded its cert pair, so a successful dial means the
// listener is real; the login itself proves the handshake works.
func waitTLSAccepting(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the daemon's TLS listener never accepted on %s: %v", addr, lastErr)
}

func boolPtr(b bool) *bool { return &b }
