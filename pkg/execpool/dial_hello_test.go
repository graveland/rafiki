package execpool

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/upgradeconn"

	"github.com/multigres/testkit/assert"
)

func TestBuildAuthCarriesATicket(t *testing.T) {
	c := assert.NewCollecting(t)
	hdr, err := buildAuth(ConnectOptions{Ticket: "tkt-abc"})
	c.Require().NoError(err, "a ticket is a complete credential on its own; buildAuth "+
		"must not demand --credential or --enroll-token beside it: %v", err)
	got, want := hdr.Get("Authorization"), "Ticket tkt-abc"
	c.Require().Eq(want, got, "Authorization")
	c.Eq("", hdr.Get(upgradeconn.HeaderSelfReported), "no self-reported facts were given; the header must be absent")
}

// A ticket is mutually exclusive with the durable paths (see ConnectOptions.Ticket).
// It wins so that an interactive client with a stale executor.cred on disk still
// gets a transient executor rather than silently reusing another identity.
func TestBuildAuthPrefersTheTicketOverACredential(t *testing.T) {
	c := assert.NewAborting(t)
	hdr, err := buildAuth(ConnectOptions{Ticket: "tkt", Credential: "cred"})
	c.NoError(err)
	got, want := hdr.Get("Authorization"), "Ticket tkt"
	c.Eq(want, got, "ticket must win: got Authorization")
}

func TestBuildAuthStillRefusesAnEmptyOptions(t *testing.T) {
	_, err := buildAuth(ConnectOptions{})
	assert.NewAborting(t).Error(err, "no ticket, no credential, no token: buildAuth must refuse "+
		"rather than send an unauthenticated upgrade request")
}

// Self-reported capability facts ride the upgrade request as a
// url.Values-encoded Rafiki-Self-Reported header, and the credential is still
// the explicit one.
func TestBuildAuthEncodesSelfReported(t *testing.T) {
	c := assert.NewAborting(t)
	hdr, err := buildAuth(ConnectOptions{
		Credential:   "cred",
		SelfReported: map[string]string{"os": "linux", "arch": "arm64"},
	})
	c.NoError(err)
	if got, want := hdr.Get("Authorization"), "Bearer cred"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	got, want := hdr.Get(upgradeconn.HeaderSelfReported), "arch=arm64&os=linux"
	c.Eq(want, got, "Rafiki-Self-Reported")
}

// The real-world case for an interactive client: a machine that also runs
// `rafiki executor serve` has a credential file on disk, and a session ticket
// must still win over it — otherwise the session executor silently connects
// as the durable one.
func TestBuildAuthPrefersTheTicketOverACredentialFile(t *testing.T) {
	c := assert.NewAborting(t)
	credFile := filepath.Join(t.TempDir(), "credential")
	c.NoError(os.WriteFile(credFile, []byte("cred\n"), 0o600))
	hdr, err := buildAuth(ConnectOptions{Ticket: "tkt", CredentialFile: credFile})
	c.NoError(err)
	got, want := hdr.Get("Authorization"), "Ticket tkt"
	c.Eq(want, got, "the ticket must win over the credential file: got Authorization")
}

// The lower half of the precedence: an explicit Credential beats the file, and
// the file beats the Enroll token. The second is the one that matters — an
// enrolled executor must reconnect with its credential, not re-send a token
// that enrollment already consumed.
func TestBuildAuthPrefersCredentialThenFileThenEnrollToken(t *testing.T) {
	credFile := filepath.Join(t.TempDir(), "credential")
	assert.NewAborting(t).NoError(os.WriteFile(credFile, []byte("filecred\n"), 0o600))
	for _, tc := range []struct {
		name string
		o    ConnectOptions
		want string
	}{
		{"credential over file", ConnectOptions{Credential: "cred", CredentialFile: credFile, EnrollToken: "tok"}, "Bearer cred"},
		{"file over enroll token", ConnectOptions{CredentialFile: credFile, EnrollToken: "tok"}, "Bearer filecred"},
		{"enroll token when no file", ConnectOptions{CredentialFile: filepath.Join(t.TempDir(), "absent"), EnrollToken: "tok"}, "Enroll tok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			hdr, err := buildAuth(tc.o)
			c.NoError(err)
			c.Eq(tc.want, hdr.Get("Authorization"), "Authorization")
		})
	}
}
