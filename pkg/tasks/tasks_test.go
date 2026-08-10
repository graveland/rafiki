package tasks

import "testing"

func TestStatusTerminal(t *testing.T) {
	// StatusOrphaned must NOT be terminal — it is reassignable by design.
	if StatusOrphaned.Terminal() {
		t.Error("orphaned must not be terminal; it exists to be reassigned")
	}
	if !StatusDropped.Terminal() {
		t.Error("dropped must be terminal")
	}
	if StatusPending.Terminal() || StatusBlocked.Terminal() {
		t.Error("pending and blocked are not terminal")
	}
	if Status("nonsense").Valid() {
		t.Error("unknown status must be invalid")
	}
}
