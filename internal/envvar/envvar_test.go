package envvar

import "testing"

func TestGetPrefersCurrentName(t *testing.T) {
	t.Setenv(Socket, "/new/sock")
	t.Setenv("PI_CONTROLLER_SOCKET", "/old/sock")

	if got := Get(Socket); got != "/new/sock" {
		t.Errorf("Get(%s) = %q, want the current name to win", Socket, got)
	}
}

// The whole point of the fallback: an existing shell export under the old name
// must keep working rather than silently resolving to "".
func TestGetFallsBackToDeprecatedName(t *testing.T) {
	for current, old := range map[string]string{
		Socket:     "PI_CONTROLLER_SOCKET",
		ChildID:    "PI_CONTROLLER_CHILD_ID",
		GraceHours: "PI_CONTROLLER_GRACE_HOURS",
		PiBinary:   "PI_BINARY",
	} {
		t.Setenv(current, "")
		t.Setenv(old, "from-old")
		if got := Get(current); got != "from-old" {
			t.Errorf("Get(%s) = %q, want fallback to %s", current, got, old)
		}
	}
}

func TestGetEmptyWhenNeitherSet(t *testing.T) {
	t.Setenv(Socket, "")
	t.Setenv("PI_CONTROLLER_SOCKET", "")
	if got := Get(Socket); got != "" {
		t.Errorf("Get(%s) = %q, want empty", Socket, got)
	}
}

// A name with no deprecated predecessor must not panic on the map lookup.
func TestGetUnmappedNameIsSafe(t *testing.T) {
	t.Setenv(AgentDB, "postgres://x")
	if got := Get(AgentDB); got != "postgres://x" {
		t.Errorf("Get(%s) = %q", AgentDB, got)
	}
	t.Setenv(AgentDB, "")
	if got := Get(AgentDB); got != "" {
		t.Errorf("Get(%s) = %q, want empty", AgentDB, got)
	}
}

// Every owned variable must carry the FUNDI_ prefix — that is the rename.
func TestAllOwnedNamesAreFundiPrefixed(t *testing.T) {
	for _, n := range []string{Socket, ChildID, GraceHours, PiBinary, AgentDB} {
		if len(n) < 6 || n[:6] != "FUNDI_" {
			t.Errorf("%q is not FUNDI_-prefixed", n)
		}
	}
}
