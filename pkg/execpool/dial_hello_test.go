package execpool

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/upgradeconn"
)

func TestBuildAuthCarriesATicket(t *testing.T) {
	hdr, err := buildAuth(ConnectOptions{Ticket: "tkt-abc"})
	if err != nil {
		t.Fatalf("a ticket is a complete credential on its own; buildAuth "+
			"must not demand --credential or --enroll-token beside it: %v", err)
	}
	if got, want := hdr.Get("Authorization"), "Ticket tkt-abc"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if hdr.Get(upgradeconn.HeaderSelfReported) != "" {
		t.Error("no self-reported facts were given; the header must be absent")
	}
}

// A ticket is mutually exclusive with the durable paths (see ConnectOptions.Ticket).
// It wins so that an interactive client with a stale executor.cred on disk still
// gets a transient executor rather than silently reusing another identity.
func TestBuildAuthPrefersTheTicketOverACredential(t *testing.T) {
	hdr, err := buildAuth(ConnectOptions{Ticket: "tkt", Credential: "cred"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hdr.Get("Authorization"), "Ticket tkt"; got != want {
		t.Fatalf("ticket must win: got Authorization %q, want %q", got, want)
	}
}

func TestBuildAuthStillRefusesAnEmptyOptions(t *testing.T) {
	if _, err := buildAuth(ConnectOptions{}); err == nil {
		t.Fatal("no ticket, no credential, no token: buildAuth must refuse " +
			"rather than send an unauthenticated upgrade request")
	}
}

// Self-reported capability facts ride the upgrade request as a
// url.Values-encoded Rafiki-Self-Reported header, and the credential is still
// the explicit one.
func TestBuildAuthEncodesSelfReported(t *testing.T) {
	hdr, err := buildAuth(ConnectOptions{
		Credential:   "cred",
		SelfReported: map[string]string{"os": "linux", "arch": "arm64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hdr.Get("Authorization"), "Bearer cred"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if got, want := hdr.Get(upgradeconn.HeaderSelfReported), "arch=arm64&os=linux"; got != want {
		t.Fatalf("Rafiki-Self-Reported = %q, want %q", got, want)
	}
}

// The real-world case for an interactive client: a machine that also runs
// `rafiki executor serve` has a credential file on disk, and a session ticket
// must still win over it — otherwise the session executor silently connects
// as the durable one.
func TestBuildAuthPrefersTheTicketOverACredentialFile(t *testing.T) {
	credFile := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credFile, []byte("cred\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hdr, err := buildAuth(ConnectOptions{Ticket: "tkt", CredentialFile: credFile})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hdr.Get("Authorization"), "Ticket tkt"; got != want {
		t.Fatalf("the ticket must win over the credential file: got Authorization %q, want %q", got, want)
	}
}

// The lower half of the precedence: an explicit Credential beats the file, and
// the file beats the Enroll token. The second is the one that matters — an
// enrolled executor must reconnect with its credential, not re-send a token
// that enrollment already consumed.
func TestBuildAuthPrefersCredentialThenFileThenEnrollToken(t *testing.T) {
	credFile := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credFile, []byte("filecred\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
			hdr, err := buildAuth(tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if got := hdr.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}
