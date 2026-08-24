package executors

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The assertion that actually stops the loop.
//
// writeAuthFailure asks IsTerminalAuthError whether a failed Enroll is an
// ANSWER about the credential or a failure to check it, and answers Retryable
// for anything it does not recognise. A name collision is an answer -- no
// amount of retrying frees a name another executor holds -- so an unclassified
// one makes the loser reconnect forever while the daemon logs "could not verify
// an executor credential" every time, naming nothing an operator can act on.
func TestMachineNameTakenIsTerminal(t *testing.T) {
	if !IsTerminalAuthError(ErrMachineNameTaken) {
		t.Fatal("a claimed machine name must be TERMINAL: retrying cannot free " +
			"the name, so a retryable answer loops the executor forever")
	}
	if !IsTerminalAuthError(fmt.Errorf("enroll: %w", ErrMachineNameTaken)) {
		t.Fatal("classification must survive wrapping — a caller that adds " +
			"context must not silently turn the answer back into a retry")
	}
}

// Its text is what reaches the peer verbatim (writeAuthFailure forwards a
// terminal error's Error()), so it has to read as operator advice rather than
// as a database diagnostic — and it must never be a pgx message, which carries
// the DSN to a peer that has not proved who it is.
func TestMachineNameTakenReadsAsOperatorAdvice(t *testing.T) {
	msg := ErrMachineNameTaken.Error()
	for _, want := range []string{"--name", "relabel"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message reaches the operator verbatim and must say what "+
				"to do; %q is missing from %q", want, msg)
		}
	}
}

// An error nobody classified stays retryable: the cost of retrying a dead
// credential is a log line, the cost of quitting on a transient one is a fleet.
func TestUnclassifiedErrorsStayRetryable(t *testing.T) {
	if IsTerminalAuthError(errors.New("dial tcp: connection refused")) {
		t.Fatal("IsTerminalAuthError must fail toward RETRY")
	}
}
