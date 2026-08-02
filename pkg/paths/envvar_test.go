package paths

import "testing"

// The FUNDI_* and PIC_*/PI_CONTROLLER_* spellings are gone. Get reads exactly
// one name — a fallback chain three renames deep is the drift this
// consolidation exists to end.
func TestGet_ReadsOnlyTheCurrentName(t *testing.T) {
	t.Setenv("RAFIKI_SOCKET", "/run/new.sock")
	t.Setenv("FUNDI_SOCKET", "/run/old.sock")
	t.Setenv("PI_CONTROLLER_SOCKET", "/run/ancient.sock")

	if got := Get(Socket); got != "/run/new.sock" {
		t.Errorf("Get(Socket) = %q, want /run/new.sock", got)
	}
}

func TestGet_DoesNotFallBackToRetiredSpellings(t *testing.T) {
	t.Setenv("FUNDI_SOCKET", "/run/old.sock")
	t.Setenv("PI_CONTROLLER_SOCKET", "/run/ancient.sock")

	if got := Get(Socket); got != "" {
		t.Errorf("Get(Socket) = %q with only retired spellings set, want \"\"", got)
	}
}

// The three merges: six variables became three, and neither survivor is
// overloaded. RAFIKI_URL/RAFIKI_TOKEN are client-side (what this process
// presents); RAFIKI_SERVE_TOKEN is server-side (what the face accepts).
func TestOwnedVariableNames(t *testing.T) {
	cases := map[string]string{
		Socket:       "RAFIKI_SOCKET",
		ChildID:      "RAFIKI_CHILD_ID",
		DB:           "RAFIKI_DB",
		URL:          "RAFIKI_URL",
		Token:        "RAFIKI_TOKEN",
		ServeToken:   "RAFIKI_SERVE_TOKEN",
		DefaultModel: "RAFIKI_DEFAULT_MODEL",
		Instructions: "RAFIKI_INSTRUCTIONS",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("variable constant = %q, want %q", got, want)
		}
	}
}
