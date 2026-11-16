package execpool

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
)

// The install ticket is the target-side half of a direct install: the daemon
// mints it for one redemption, the executor presents it on its FIRST connect,
// and the credential the 101 carries is the one every later attempt uses. These
// tests pin that whole arc — the header chosen, the file written, the loop's
// decision to stop presenting the ticket, and which failures are terminal.

// installTicketFixture is a TLS listener speaking the executor upgrade. It
// records the Authorization of every dial and lets the test answer each one,
// so an install-ticket test drives the real Connect loop rather than a single
// hand-rolled attempt.
type installTicketFixture struct {
	addr string
	pin  string

	mu    sync.Mutex
	auths []string
}

func (f *installTicketFixture) presented() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

// answer decides the 101's headers for dial n (1-based) given the Authorization
// it carried; an error is an ordinary upgrade refusal.
func newInstallTicketFixture(t *testing.T, answer func(n int, auth string) (http.Header, error)) *installTicketFixture {
	t.Helper()
	cert := testCert(t)
	sum := sha256.Sum256(cert.Certificate[0])

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   ALPNProtocols,
	})
	assert.NewAborting(t).NoError(err, "tls.Listen")
	t.Cleanup(func() { _ = ln.Close() })

	f := &installTicketFixture{addr: ln.Addr().String(), pin: fmt.Sprintf("%x", sum[:])}
	handler := upgradeconn.Handler(upgradeconn.Executor, func(r *http.Request) (struct{}, http.Header, error) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		n := len(f.auths)
		f.mu.Unlock()

		hdr, aerr := answer(n, r.Header.Get("Authorization"))
		if aerr != nil {
			return struct{}{}, nil, aerr
		}
		if hdr == nil {
			hdr = http.Header{}
		}
		return struct{}{}, hdr, nil
	}, func(c *upgradeconn.Conn, _ struct{}) { _ = c.Close() })

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return f
}

// fastReconnect makes the reconnect ladder millisecond-scale so a two-dial test
// does not spend a second in the backoff, restored on cleanup.
func fastReconnect(t *testing.T) {
	t.Helper()
	oldInitial, oldMax := initialBackoff, maxBackoff
	initialBackoff, maxBackoff = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { initialBackoff, maxBackoff = oldInitial, oldMax })
}

// installOpts builds the options a ticket-authenticated executor dials with:
// the fixture's address and pin, and whatever credential file the test means.
func installOpts(f *installTicketFixture, credFile, ticket string) ConnectOptions {
	_, handler := executorpbconnect.NewExecutorServiceHandler(&stubHandler{executorID: "install-1"})
	return ConnectOptions{
		Addr:           f.addr,
		ServerName:     "localhost",
		PinCert:        f.pin,
		CredentialFile: credFile,
		InstallTicket:  ticket,
		Handler:        handler,
	}
}

// A relaunch presents a fresh install ticket while the PREVIOUS credential is
// still on disk. The ticket must win: falling through to the file would
// present a credential the daemon is about to rotate away and never redeem the
// ticket at all.
func TestBuildAuthInstallTicketWins(t *testing.T) {
	c := assert.NewAborting(t)

	credFile := filepath.Join(t.TempDir(), "executor.cred")
	c.NoError(os.WriteFile(credFile, []byte("old-credential\n"), 0o600), "seed credential file")

	hdr, err := buildAuth(ConnectOptions{InstallTicket: "ticket-1", CredentialFile: credFile})
	c.Require().NoError(err, "buildAuth")
	c.Eq("Ticket ticket-1", hdr.Get("Authorization"),
		"the install ticket must win over the credential file on disk")
}

// Each of the other three auth paths is a different identity, so combining one
// with an install ticket is refused rather than resolved by header order.
func TestBuildAuthInstallTicketIsExclusive(t *testing.T) {
	c := assert.NewAborting(t)
	for name, extra := range map[string]ConnectOptions{
		"session ticket": {Ticket: "sess-ticket"},
		"credential":     {Credential: "a-credential"},
		"enroll token":   {EnrollToken: "t_enroll"},
	} {
		t.Run(name, func(t *testing.T) {
			o := extra
			o.InstallTicket = "ticket-1"
			_, err := buildAuth(o)
			c.Require().Error(err, "an install ticket combined with a %s must be refused", name)
			c.StrContains(err.Error(),
				"install ticket cannot be combined with a session ticket, a credential or an enrollment token",
				"the refusal must name all three conflicting auth paths")
		})
	}
}

// The whole arc in one test: dial 1 redeems the ticket, the credential lands on
// disk at 0600, OnEnrolled runs once, and dial 2 presents Bearer <credential>.
func TestConnectInstallTicketThenBearer(t *testing.T) {
	c := assert.NewAborting(t)
	fastReconnect(t)

	f := newInstallTicketFixture(t, func(n int, _ string) (http.Header, error) {
		if n == 1 {
			return http.Header{
				upgradeconn.HeaderCredential: {"minted-cred"},
				upgradeconn.HeaderExecutorID: {"install-1"},
			}, nil
		}
		return http.Header{}, &upgradeconn.Refusal{Status: http.StatusUnauthorized, Reason: "unknown credential"}
	})

	// A directory that does not exist yet: Connect creates it, which is the
	// default <data dir>/executor.cred case.
	credFile := filepath.Join(t.TempDir(), "state", "executor.cred")
	enrolled := 0
	o := installOpts(f, credFile, "one-time-ticket")
	o.OnEnrolled = func() { enrolled++ }

	err := Connect(t.Context(), o)
	c.ErrorIs(err, ErrEnrollmentRejected, "the second dial is refused 401, which ends the loop")

	c.EqDeep([]string{"Ticket one-time-ticket", "Bearer minted-cred"}, f.presented(),
		"dial 1 must redeem the ticket and dial 2 must present the stored credential, not the ticket again")
	c.Eq(1, enrolled, "OnEnrolled must run exactly once, after the credential is on disk")

	b, err := os.ReadFile(credFile)
	c.Require().NoError(err, "read the credential file Connect wrote")
	c.Eq("minted-cred\n", string(b), "the credential file holds the minted credential")
	st, err := os.Stat(credFile)
	c.Require().NoError(err, "stat the credential file")
	c.Eq(os.FileMode(0o600), st.Mode().Perm(), "the credential file must be 0600")
}

// A daemon that answers the ticket with a 101 carrying no credential predates
// install tickets. The ticket is spent, so the answer is terminal: no retry.
func TestConnectInstallTicketWithoutCredentialIsTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	fastReconnect(t)

	f := newInstallTicketFixture(t, func(int, string) (http.Header, error) {
		return http.Header{}, nil
	})

	err := Connect(t.Context(), installOpts(f, filepath.Join(t.TempDir(), "executor.cred"), "ticket-1"))
	c.ErrorIs(err, ErrEnrollmentRejected, "no credential on the 101 is terminal")
	c.StrContains(err.Error(), "issued no credential for the install ticket",
		"the error must say the daemon minted nothing")
	c.Eq(1, len(f.presented()), "a terminal answer must not be retried")
}

// A credential the daemon has ALREADY minted and rotated into place has nowhere
// to go without a credential file, so this is terminal too — retrying would
// present a spent ticket.
func TestConnectInstallTicketRequiresCredentialFile(t *testing.T) {
	c := assert.NewAborting(t)
	fastReconnect(t)

	f := newInstallTicketFixture(t, func(int, string) (http.Header, error) {
		return http.Header{upgradeconn.HeaderCredential: {"minted-cred"}}, nil
	})

	err := Connect(t.Context(), installOpts(f, "", "ticket-1"))
	c.ErrorIs(err, ErrEnrollmentRejected, "a missing credential file is terminal")
	c.StrContains(err.Error(), "install ticket requires a credential file",
		"the error must name what is missing")
	c.Eq(1, len(f.presented()), "a terminal answer must not be retried")
}

// 401 is an ANSWER about this ticket — spent, unknown, or revoked — so Connect
// stops rather than redialling a ticket the daemon has already consumed.
func TestConnectInstallTicketRefusedIsTerminalAndNotRetried(t *testing.T) {
	c := assert.NewAborting(t)
	fastReconnect(t)

	f := newInstallTicketFixture(t, func(int, string) (http.Header, error) {
		return http.Header{}, &upgradeconn.Refusal{Status: http.StatusUnauthorized, Reason: "the install ticket is spent"}
	})

	err := Connect(t.Context(), installOpts(f, filepath.Join(t.TempDir(), "executor.cred"), "spent-ticket"))
	c.ErrorIs(err, ErrEnrollmentRejected, "a refused install ticket is terminal")
	c.Eq(1, len(f.presented()), "a refused install ticket must not be retried")
}

// Connect dials with a COPY of the options, so its own clearing leaves the
// caller's value holding the spent ticket for the life of the process — a
// secret readable from a heap profile or a core dump.
// ClearInstallTicketOnEnroll is the hook that clears it, and it must still call
// the caller's own hook.
func TestConnectInstallTicketClearsTheCallersCopy(t *testing.T) {
	c := assert.NewAborting(t)
	fastReconnect(t)

	f := newInstallTicketFixture(t, func(n int, _ string) (http.Header, error) {
		if n == 1 {
			return http.Header{
				upgradeconn.HeaderCredential: {"minted-cred"},
				upgradeconn.HeaderExecutorID: {"install-1"},
			}, nil
		}
		return http.Header{}, &upgradeconn.Refusal{Status: http.StatusUnauthorized, Reason: "unknown credential"}
	})

	credFile := filepath.Join(t.TempDir(), "executor.cred")
	o := installOpts(f, credFile, "one-time-ticket")
	hooks := 0
	o.OnEnrolled = ClearInstallTicketOnEnroll(&o, func() { hooks++ })

	err := Connect(t.Context(), o)
	c.ErrorIs(err, ErrEnrollmentRejected, "the second dial is refused 401, which ends the loop")

	c.Eq("", o.InstallTicket, "the caller's copy of the spent ticket must be cleared on redemption")
	c.Eq(1, hooks, "the caller's own OnEnrolled hook must still run, once")
	c.EqDeep([]string{"Ticket one-time-ticket", "Bearer minted-cred"}, f.presented(),
		"clearing the caller's copy must not disturb which credential each dial presents")
}

// A credential the daemon minted and rotated into place cannot be stored. The
// ticket is spent by that rotation, so retrying would present it again and earn
// a 401 that names the wrong cause: the failure is terminal, and the message
// leads with the write that actually failed.
func TestConnectInstallTicketCredentialWriteFailureIsTerminal(t *testing.T) {
	c := assert.NewAborting(t)
	fastReconnect(t)

	f := newInstallTicketFixture(t, func(int, string) (http.Header, error) {
		return http.Header{upgradeconn.HeaderCredential: {"minted-cred"}}, nil
	})

	// A regular FILE where the credential's directory belongs: creating the
	// directory fails, which is the whole class of "cannot store it here".
	blocker := filepath.Join(t.TempDir(), "blocker")
	c.Require().NoError(os.WriteFile(blocker, []byte("not a directory"), 0o600), "seed the blocker file")

	enrolled := 0
	o := installOpts(f, filepath.Join(blocker, "executor.cred"), "ticket-1")
	o.OnEnrolled = func() { enrolled++ }

	// A bounding context: the regression this guards is a Connect that treats the
	// failure as retryable, and that loops rather than returning.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err := Connect(ctx, o)
	c.Require().Error(err, "a credential that cannot be stored must fail the connect")
	c.ErrorIs(err, ErrEnrollmentRejected,
		"the daemon already rotated the credential, so this is terminal, not retryable")
	c.True(strings.HasPrefix(err.Error(), "create credential directory"),
		"the message must lead with the write failure; got %q", err.Error())
	c.Eq(0, enrolled, "the install marker is recorded only once the credential is on disk")
	c.Eq(1, len(f.presented()), "a spent ticket must not be presented a second time")
}
