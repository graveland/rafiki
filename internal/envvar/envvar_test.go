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
//
// Driven off the deprecated map itself rather than a hand-maintained copy, so a
// newly migrated variable is covered the moment it is added.
func TestGetFallsBackToDeprecatedName(t *testing.T) {
	for current, old := range deprecated {
		t.Setenv(current, "")
		t.Setenv(old, "from-old")
		if got := Get(current); got != "from-old" {
			t.Errorf("Get(%s) = %q, want fallback to %s", current, got, old)
		}
	}
}

// A deprecated spelling must differ from the name that replaced it. If they are
// equal the fallback silently becomes a no-op — and worse, any "the old variable
// is still set" diagnostic built on it starts firing on the current one. This is
// not hypothetical: a mechanical rename across the tree produced exactly this
// collision twice during the fundi rename (once here, once in the presets
// filename constant), because the substitution also rewrote the legacy literal.
func TestDeprecatedNamesDifferFromCurrent(t *testing.T) {
	for current, old := range deprecated {
		if current == old {
			t.Errorf("deprecated[%s] == %s: a rename sweep clobbered the legacy spelling", current, old)
		}
		if len(old) >= 6 && old[:6] == "FUNDI_" {
			t.Errorf("deprecated[%s] = %q, which is already FUNDI_-prefixed and cannot be the old name", current, old)
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
	names := []string{AgentDB}
	for current := range deprecated {
		names = append(names, current)
	}
	for _, n := range names {
		if len(n) < 6 || n[:6] != "FUNDI_" {
			t.Errorf("%q is not FUNDI_-prefixed", n)
		}
	}
}

// Guards against a migrated variable being declared but never wired into the
// deprecated map, which would drop its fallback without any visible symptom.
func TestEveryMigratedVariableHasAFallback(t *testing.T) {
	for _, n := range []string{
		Socket, ChildID, GraceHours, PiBinary,
		NoAutoInstallHelpers, DefaultModel, DefaultPreset, DefaultLabels, AttachTail,
	} {
		if _, ok := deprecated[n]; !ok {
			t.Errorf("%s has no entry in deprecated: an existing export under its old name would silently stop working", n)
		}
	}
}

func TestNewVarsHaveNoDeprecatedSpelling(t *testing.T) {
	for _, name := range []string{Instructions, SkillsDirs, MCPConfig} {
		t.Setenv(name, "/from/new/name")
		if got := Get(name); got != "/from/new/name" {
			t.Fatalf("Get(%s) = %q, want /from/new/name", name, got)
		}
	}
}

func TestNewVarNamesAreFundiPrefixed(t *testing.T) {
	want := map[string]string{
		Instructions: "FUNDI_INSTRUCTIONS",
		SkillsDirs:   "FUNDI_SKILLS_DIRS",
		MCPConfig:    "FUNDI_MCP_CONFIG",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("constant = %q, want %q", got, expect)
		}
	}
}
