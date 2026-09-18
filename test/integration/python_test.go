package integration_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roundtripModCode is the fixture body `rafiki py` round-trips. It ends in a
// newline, so `py get`'s raw print (fmt.Println) emits exactly body + "\n".
const roundtripModCode = "VALUE = \"round-trip\"\n\n\ndef greet():\n    return \"hello from roundtrip_mod\"\n"

// writeRoundtripFixture writes the fixture body to a temp .py file and returns
// its path — the --file argument every put in this test carries.
func writeRoundtripFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "roundtrip_mod.py")
	if err := os.WriteFile(path, []byte(roundtripModCode), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestPythonRoundTrip exercises `rafiki py` end to end against a live,
// DB-backed daemon through the real client binary: put → list (codeless
// inventory) → get (raw code back out) → delete → get fails with not-found,
// plus the client-side rejection of a name that is not a bare Python
// identifier. Every step execs the real rafiki binary via cliCmd, which aims
// it at d's socket through a scratch profile with the ambient client
// environment blanked.
func TestPythonRoundTrip(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)
	fixture := writeRoundtripFixture(t)

	// ── 1. put: saved, with a version number ─────────────────────────────
	// Table mode (the default on a pipe) prints "saved <name> as version N".
	var putStderr bytes.Buffer
	putCmd := cliCmd(t, d, "py", "put", "roundtrip_mod",
		"--file", fixture, "--description", "integration fixture")
	putCmd.Stderr = &putStderr
	putOut, err := putCmd.Output() // stdout only
	if err != nil {
		t.Fatalf("py put failed: %v\nstderr: %s", err, putStderr.String())
	}
	if !strings.Contains(string(putOut), "saved roundtrip_mod as version") {
		t.Fatalf("py put output %q does not announce the saved version", putOut)
	}

	// ── 2. list -j: the row is there, the code is not ────────────────────
	// An inventory is not a document: the list response omits code entirely
	// (the field is json omitempty, so an empty code never even appears as a
	// key). Parse rather than substring-match so the assertion is about the
	// row's shape, not its rendering.
	listOut, err := cliCmd(t, d, "py", "list", "-j").Output()
	if err != nil {
		t.Fatalf("py list -j failed: %v", err)
	}
	var env struct {
		Rows []struct {
			Name string  `json:"name"`
			Code *string `json:"code"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(listOut, &env); err != nil {
		t.Fatalf("decode py list -j output: %v\noutput: %s", err, listOut)
	}
	var found bool
	for _, row := range env.Rows {
		if row.Name != "roundtrip_mod" {
			continue
		}
		found = true
		if row.Code != nil {
			t.Fatalf("py list -j row for roundtrip_mod carries a code field; inventory must not: %s", listOut)
		}
	}
	if !found {
		t.Fatalf("py list -j has no row named roundtrip_mod: %s", listOut)
	}
	// The parse above is the real codeless guard; this substring check is the
	// belt to those braces. The fragment is chosen escape-free on purpose —
	// no quotes, no newlines — so it survives JSON escaping and the check can
	// actually fire if the code ever leaks.
	if bytes.Contains(listOut, []byte("hello from roundtrip_mod")) {
		t.Fatalf("py list -j leaked the module body into the inventory: %s", listOut)
	}

	// ── 3. get: the code comes back byte-for-byte ────────────────────────
	// Table mode prints the code raw to stdout (fmt.Fprintln on os.Stdout),
	// so the output is the file's contents plus the one trailing newline
	// the print adds on top of the one already in the body.
	getOut, err := cliCmd(t, d, "py", "get", "roundtrip_mod").Output()
	if err != nil {
		t.Fatalf("py get failed: %v", err)
	}
	if want := roundtripModCode + "\n"; string(getOut) != want {
		t.Fatalf("py get output mismatch:\n got: %q\nwant: %q", getOut, want)
	}

	// ── 4. delete: confirmed on stdout ───────────────────────────────────
	var delStderr bytes.Buffer
	delCmd := cliCmd(t, d, "py", "delete", "roundtrip_mod")
	delCmd.Stderr = &delStderr
	delOut, err := delCmd.Output()
	if err != nil {
		t.Fatalf("py delete failed: %v\nstderr: %s", err, delStderr.String())
	}
	if !strings.Contains(string(delOut), "deleted roundtrip_mod") {
		t.Fatalf("py delete output %q does not confirm the deletion", delOut)
	}

	// ── 5. get after delete: not found, nonzero, on stderr ───────────────
	// The daemon maps pymodules.ErrNotFound to Connect's CodeNotFound; the
	// client prints the error to stderr and exits nonzero. stdout and stderr
	// are captured separately because which stream carries the failure is
	// the point.
	var goneOut, goneErrBuf bytes.Buffer
	goneCmd := cliCmd(t, d, "py", "get", "roundtrip_mod")
	goneCmd.Stdout = &goneOut
	goneCmd.Stderr = &goneErrBuf
	if err := goneCmd.Run(); err == nil {
		t.Fatalf("py get after delete must exit nonzero; stdout: %q", goneOut.String())
	}
	if !strings.Contains(goneErrBuf.String(), "not found") {
		t.Fatalf("py get after delete: stderr %q does not mention not found", goneErrBuf.String())
	}

	// ── 6. put with a non-identifier name: refused CLIENT-side ───────────
	// "9bad" cannot be a path segment or an import target. Both the client
	// and the daemon run the same ValidName check with the same message, so
	// a put against the live daemon cannot tell which side fired. Run BOTH
	// shapes with no daemon at all — cliCmd(t, nil, …) leaves the scratch
	// profile dir empty, so any name that survives the client-side check
	// dies later, in endpoint resolution, with a different error. The
	// control below proves the two failures differ; "9bad" failing with the
	// identifier message therefore proves the client-side check fired
	// before any dial.
	var badOut, badErrBuf bytes.Buffer
	badCmd := cliCmd(t, nil, "py", "put", "9bad", "--file", fixture)
	badCmd.Stdout = &badOut
	badCmd.Stderr = &badErrBuf
	if err := badCmd.Run(); err == nil {
		t.Fatalf("py put 9bad must exit nonzero; stdout: %q", badOut.String())
	}
	if !strings.Contains(badErrBuf.String(), "bare Python identifier") {
		t.Fatalf("py put 9bad: stderr %q does not carry the identifier error", badErrBuf.String())
	}

	// Control: the same command with a VALID name and no daemon fails
	// somewhere else — profile or endpoint resolution — and never with the
	// identifier message. If this ever starts carrying "bare Python
	// identifier", the client-side pre-check has been dropped or moved
	// behind the dial and step 6's premise is gone.
	var ctlOut, ctlErrBuf bytes.Buffer
	ctlCmd := cliCmd(t, nil, "py", "put", "roundtrip_mod", "--file", fixture)
	ctlCmd.Stdout = &ctlOut
	ctlCmd.Stderr = &ctlErrBuf
	if err := ctlCmd.Run(); err == nil {
		t.Fatalf("py put with a valid name and no daemon must exit nonzero; stdout: %q", ctlOut.String())
	}
	if ctlErr := ctlErrBuf.String(); strings.Contains(ctlErr, "bare Python identifier") {
		t.Fatalf("py put control: no daemon yet the error is the identifier message; the client-side ValidName pre-check did not fire first: %q", ctlErr)
	}
}
