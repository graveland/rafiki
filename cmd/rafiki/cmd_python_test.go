// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// The tree contract: `python` sits under the root with the alias `py`, and
// carries exactly the four blob verbs plus the `repo` group that manages git
// sources. Aliases are guarded the same way aliases_test.go guards the rest
// of the CLI — presence is a test failure away from silent regression.
func TestPythonCommandTree(t *testing.T) {
	cmd, _, err := newRootCmd().Find([]string{"python"})
	if err != nil {
		t.Fatalf("find python: %v", err)
	}
	if !cmd.HasAlias("py") {
		t.Errorf("python aliases = %v, want \"py\" present", cmd.Aliases)
	}
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"list", "get", "put", "delete", "repo"} {
		if !got[want] {
			t.Errorf("python subcommands missing %q (have %v)", want, got)
		}
	}
	if len(got) != 5 {
		t.Errorf("python subcommands = %v, want exactly list/get/put/delete/repo", got)
	}

	// The alias resolves to the same command: `rafiki py list` must be
	// `rafiki python list`, not a second registration of it.
	aliased, _, err := newRootCmd().Find([]string{"py", "list"})
	if err != nil {
		t.Fatalf("find py list: %v", err)
	}
	if aliased.Name() != "list" || aliased.Parent().Name() != "python" {
		t.Errorf("py list resolved to %q under %q, want list under python",
			aliased.Name(), aliased.Parent().Name())
	}
}

// The argument and completion contracts: get/delete/put take exactly one
// name, list takes none, and the name-completing verbs carry
// completePyModuleNames. Arg validation runs before RunE, so every Execute
// here fails without dialing anything.
func TestPythonArgAndCompletionContracts(t *testing.T) {
	get := newPythonGetCmd()
	del := newPythonDeleteCmd()
	put := newPythonPutCmd()

	for _, c := range []*cobra.Command{get, del, put} {
		if c.ValidArgsFunction == nil {
			t.Errorf("%s: ValidArgsFunction is nil, want completePyModuleNames", c.Name())
		}
	}

	// Exactly-one-name, exercised through the parser the user hits.
	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{get, nil}, {get, []string{"a", "b"}},
		{del, nil}, {del, []string{"a", "b"}},
		{put, nil}, {put, []string{"a", "b"}},
	} {
		buf := &bytes.Buffer{}
		tc.cmd.SetOut(buf)
		tc.cmd.SetErr(buf)
		tc.cmd.SetArgs(tc.args)
		if err := tc.cmd.Execute(); err == nil {
			t.Errorf("%s %v: executed without error, want an arg-count refusal", tc.cmd.Name(), tc.args)
		}
	}

	// list takes no arguments at all.
	list := newPythonListCmd()
	list.SetOut(&bytes.Buffer{})
	list.SetErr(&bytes.Buffer{})
	list.SetArgs([]string{"extra"})
	if err := list.Execute(); err == nil {
		t.Error("list extra: executed without error, want a NoArgs refusal")
	}

	// Past the one name a verb takes, completion offers nothing — and this
	// branch returns before it touches a profile, so it is safe to call bare.
	if got, dir := completePyModuleNames(get, []string{"some"}, ""); got != nil ||
		dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completePyModuleNames(past the name) = (%v, %v), want (nil, NoFileComp)", got, dir)
	}

	// With nothing typed yet, an unresolvable endpoint degrades to "no
	// candidates" rather than an error or a hang — the completion contract.
	isolateProfiles(t)
	if got, dir := completePyModuleNames(get, nil, ""); got != nil ||
		dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("completePyModuleNames(no endpoint) = (%v, %v), want (nil, NoFileComp)", got, dir)
	}
}

// The put contract: --file is required, and a name that can never be a path
// segment is refused locally — before any code is shipped to the daemon.
// isolateProfiles keeps the pinned message provably client-side: the ValidName
// check runs before newConnectEndpoint, so the string can only come from it,
// never from whatever profile an ambient environment points at.
func TestPythonPutRequiresFileAndValidName(t *testing.T) {
	isolateProfiles(t)
	put := newPythonPutCmd()
	put.SetOut(&bytes.Buffer{})
	put.SetErr(&bytes.Buffer{})
	put.SetArgs([]string{"helper"})
	err := put.Execute()
	if err == nil || !strings.Contains(err.Error(), "--file is required") {
		t.Fatalf("put helper (no --file) = %v, want \"--file is required\"", err)
	}

	// With a file present, the client-side ValidName pre-check fires — the
	// same check the daemon re-runs, but here it costs no round trip.
	src := t.TempDir() + "/helper.py"
	if err := writeFileForTest(src, "x = 1\n"); err != nil {
		t.Fatal(err)
	}
	put = newPythonPutCmd()
	put.SetOut(&bytes.Buffer{})
	put.SetErr(&bytes.Buffer{})
	put.SetArgs([]string{"../escape", "--file", src})
	if err := put.Execute(); err == nil || !strings.Contains(err.Error(), "bare Python identifier") {
		t.Fatalf("put ../escape = %v, want the ValidName refusal", err)
	}
}

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// The table cells: version as a plain number, SAVED as "2006-01-02 15:04",
// and "-" for an absent description — the fallbacks an empty inventory row
// must render rather than blanks.
func TestPythonListCells(t *testing.T) {
	row := func(name string, version int64, createdAt, description string) *rafikiv1.PymoduleRow {
		return &rafikiv1.PymoduleRow{
			Name:        name,
			Version:     version,
			CreatedAt:   createdAt,
			Description: description,
		}
	}
	name, version, saved, description := pymoduleCells(
		row("helper", 7, "2026-09-18T12:34:56Z", ""))
	if name != "helper" || version != "7" || saved != "2026-09-18 12:34" || description != "-" {
		t.Errorf("pymoduleCells = (%q,%q,%q,%q), want (helper,7,2026-09-18 12:34,-)",
			name, version, saved, description)
	}

	if got := formatSavedAt(""); got != "-" {
		t.Errorf("formatSavedAt(\"\") = %q, want \"-\"", got)
	}
	// A timestamp that does not parse is displayed, not blanked: a daemon
	// clock this daemon wrote must stay visible even when malformed.
	if got := formatSavedAt("not-a-timestamp"); got != "not-a-timestamp" {
		t.Errorf("formatSavedAt(unparseable) = %q, want the raw string", got)
	}

	// The rendered table names the four columns and never prints CODE.
	var buf bytes.Buffer
	if err := renderPymoduleList(&buf, []*rafikiv1.PymoduleRow{
		row("helper", 7, "2026-09-18T12:34:56Z", "reusable helpers"),
	}, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "VERSION", "SAVED", "DESCRIPTION", "helper", "7", "2026-09-18 12:34", "reusable helpers"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderPymoduleList output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CODE") {
		t.Errorf("renderPymoduleList prints a CODE column:\n%s", out)
	}
}

// The JSON shapes: list -j wraps the rows in the {"rows": …} envelope, list
// -J emits one bare row per line, and get/put -j/-J emit the full row
// including code with NO envelope. These are the shapes a script keys on —
// the likeliest silent-drift point in the emit helpers — so they are pinned
// here rather than only end-to-end.
func TestPythonJSONShapes(t *testing.T) {
	wireRow := func() *rafikiv1.PymoduleRow {
		// Mirrors the wire: list rows arrive codeless, get/put rows arrive
		// with the code. omitempty must keep the empty code out of list's
		// objects entirely.
		return &rafikiv1.PymoduleRow{
			Name: "helper", Version: 7, Description: "d",
			CreatedAt: "2026-09-18T12:34:56Z", Code: "x = 1",
		}
	}

	// list -j: one envelope, and no code key on any row.
	var listBuf bytes.Buffer
	codeless := wireRow()
	codeless.Code = ""
	if err := emitPymoduleList(&listBuf, []*rafikiv1.PymoduleRow{codeless}, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	var listEnv struct {
		Rows []*rafikiv1.PymoduleRow `json:"rows"`
	}
	if err := json.Unmarshal(listBuf.Bytes(), &listEnv); err != nil {
		t.Fatalf("list -j: not a {\"rows\": …} envelope: %v\n%s", err, listBuf.String())
	}
	if len(listEnv.Rows) != 1 || listEnv.Rows[0].Name != "helper" {
		t.Errorf("list -j rows = %+v, want [helper]", listEnv.Rows)
	}
	if bytes.Contains(listBuf.Bytes(), []byte(`"code"`)) {
		t.Errorf("list -j carries a code key:\n%s", listBuf.String())
	}

	// list -J: one compact row per line, no envelope.
	var jsonlBuf bytes.Buffer
	if err := emitPymoduleList(&jsonlBuf, []*rafikiv1.PymoduleRow{codeless, codeless}, outputJSONL, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(jsonlBuf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("list -J emitted %d lines, want 2:\n%s", len(lines), jsonlBuf.String())
	}
	for i, line := range lines {
		var row rafikiv1.PymoduleRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Errorf("list -J line %d: not a bare row: %v\n%s", i, err, line)
		}
		if row.Name != "helper" {
			t.Errorf("list -J line %d: name %q, want helper", i, row.Name)
		}
	}

	// get/put -j: the full row including code, with no envelope around it.
	for _, mode := range []outputMode{outputJSON, outputJSONL} {
		var buf bytes.Buffer
		if err := emitPymoduleCode(&buf, wireRow(), mode); err != nil {
			t.Fatal(err)
		}
		var row rafikiv1.PymoduleRow
		if err := json.Unmarshal(buf.Bytes(), &row); err != nil {
			t.Fatalf("emitPymoduleCode mode %v: not a bare row: %v\n%s", mode, err, buf.String())
		}
		if row.Code != "x = 1" || row.Version != 7 || row.Name != "helper" {
			t.Errorf("emitPymoduleCode mode %v: got name=%q version=%d code=%q, want helper/7/\"x = 1\"", mode, row.Name, row.Version, row.Code)
		}
	}

	// get in table mode writes the code raw to w — byte-for-byte, no
	// decoration — so it can feed a file or an editor unchanged.
	var rawBuf bytes.Buffer
	if err := emitPymoduleCode(&rawBuf, wireRow(), outputAuto); err != nil {
		t.Fatal(err)
	}
	if got := rawBuf.String(); got != "x = 1\n" {
		t.Errorf("emitPymoduleCode table mode = %q, want %q", got, "x = 1\n")
	}
}
