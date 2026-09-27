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

	"github.com/multigres/testkit/assert"
)

// The tree contract: `python` sits under the root with the alias `py`, and
// carries exactly the four blob verbs plus the `repo` group that manages git
// sources. Aliases are guarded the same way aliases_test.go guards the rest
// of the CLI — presence is a test failure away from silent regression.
func TestPythonCommandTree(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd, _, err := newRootCmd().Find([]string{"python"})
	c.Require().NoError(err, "find python")
	if !cmd.HasAlias("py") {
		t.Errorf("python aliases = %v, want \"py\" present", cmd.Aliases)
	}
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"list", "get", "put", "delete", "repo"} {
		c.False(!got[want], "python subcommands missing %q (have %v)", want, got)
	}
	c.Len(got, 5, "python subcommands")

	// The alias resolves to the same command: `rafiki py list` must be
	// `rafiki python list`, not a second registration of it.
	aliased, _, err := newRootCmd().Find([]string{"py", "list"})
	c.Require().NoError(err, "find py list")
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
	ck := assert.NewCollecting(t)
	get := newPythonGetCmd()
	del := newPythonDeleteCmd()
	put := newPythonPutCmd()

	for _, c := range []*cobra.Command{get, del, put} {
		ck.NotNil(c.ValidArgsFunction, "%s: ValidArgsFunction is nil, want completePyModuleNames", c.Name())
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
		ck.Error(tc.cmd.Execute(), "%s %v: executed without error, want an arg-count refusal", tc.cmd.Name(), tc.args)
	}

	// list takes no arguments at all.
	list := newPythonListCmd()
	list.SetOut(&bytes.Buffer{})
	list.SetErr(&bytes.Buffer{})
	list.SetArgs([]string{"extra"})
	ck.Error(list.Execute(), "list extra: executed without error, want a NoArgs refusal")

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
	c := assert.NewAborting(t)
	isolateProfiles(t)
	put := newPythonPutCmd()
	put.SetOut(&bytes.Buffer{})
	put.SetErr(&bytes.Buffer{})
	put.SetArgs([]string{"helper"})
	err := put.Execute()
	c.False(err == nil || !strings.Contains(err.Error(), "--file is required"), "put helper (no --file) = %v, want \"--file is required\"", err)

	// With a file present, the client-side ValidName pre-check fires — the
	// same check the daemon re-runs, but here it costs no round trip.
	src := t.TempDir() + "/helper.py"
	c.NoError(writeFileForTest(src, "x = 1\n"))
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

// The table cells: repo scope, version as a plain number, SAVED as
// "2006-01-02 15:04", and "-" for an absent description — the fallbacks an
// inventory row must render rather than blanks.
func TestPythonListCells(t *testing.T) {
	c := assert.NewCollecting(t)
	row := func(name, repo string, version int64, createdAt, description string) *rafikiv1.PymoduleRow {
		return &rafikiv1.PymoduleRow{
			Name:        name,
			Repo:        repo,
			Version:     version,
			CreatedAt:   createdAt,
			Description: description,
		}
	}
	name, repo, version, saved, description := pymoduleCells(
		row("helper", "local", 7, "2026-09-18T12:34:56Z", ""))
	c.False(name != "helper" || repo != "local" || version != "7" || saved != "2026-09-18 12:34" || description != "-", "pymoduleCells = (%q,%q,%q,%q,%q), want (helper,local,7,2026-09-18 12:34,-)", name, repo, version, saved, description)

	if got := formatSavedAt(""); got != "-" {
		t.Errorf("formatSavedAt(\"\") = %q, want \"-\"", got)
	}
	// A timestamp that does not parse is displayed, not blanked: a daemon
	// clock this daemon wrote must stay visible even when malformed.
	c.Eq("not-a-timestamp", formatSavedAt("not-a-timestamp"), "formatSavedAt(unparseable)")

	// The version's absent marker mirrors the timestamp's: a git-sourced row
	// has no version, and 0 is the wire's way of saying "none".
	if got := formatVersion(0); got != "-" {
		t.Errorf("formatVersion(0) = %q, want \"-\"", got)
	}
	got := formatVersion(7)
	c.Eq("7", got, "formatVersion(7) = %q, want \"7\"", got)

	// The rendered table names the five columns and never prints CODE.
	var buf bytes.Buffer
	c.Require().NoError(renderPymoduleList(&buf, []*rafikiv1.PymoduleRow{
		row("helper", "local", 7, "2026-09-18T12:34:56Z", "reusable helpers"),
	}, false))
	out := buf.String()
	for _, want := range []string{"NAME", "REPO", "VERSION", "SAVED", "DESCRIPTION", "helper", "local", "7", "2026-09-18 12:34", "reusable helpers"} {
		c.StrContains(out, want, "renderPymoduleList output missing")
	}
	c.NotStrContains(out, "CODE", "renderPymoduleList prints a CODE column:\n")
}

// TestPythonListRepoColumnRendersLocalAndGitRows pins the REPO column: a
// local row and a git-sourced row render side by side, each carrying its own
// repo cell, and a git-sourced row's VERSION/SAVED cells render "-" — it has
// neither a version nor a save time, its source of truth being the repo's own
// history.
func TestPythonListRepoColumnRendersLocalAndGitRows(t *testing.T) {
	c := assert.NewCollecting(t)
	rows := []*rafikiv1.PymoduleRow{
		{Name: "helper", Repo: "local", Version: 7, CreatedAt: "2026-09-18T12:34:56Z", Description: "saved here"},
		{Name: "rotate_keys", Repo: "ops_tools", Description: "rotates keys"},
	}
	var buf bytes.Buffer
	c.Require().NoError(renderPymoduleList(&buf, rows, false))
	out := buf.String()
	for _, want := range []string{"NAME", "REPO", "VERSION", "SAVED", "DESCRIPTION",
		"helper", "local", "7", "2026-09-18 12:34", "saved here",
		"rotate_keys", "ops_tools", "rotates keys"} {
		c.StrContains(out, want, "renderPymoduleList output missing")
	}

	var gitLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "rotate_keys") {
			gitLine = line
			break
		}
	}
	c.Require().NotEq("", gitLine, "rotate_keys row not rendered:\n%s", out)
	n := strings.Count(gitLine, "-")
	c.GreaterOrEqual(2, n, "git-sourced row %q: only %d \"-\" cell(s), want the absent VERSION and SAVED both rendered \"-\"", gitLine, n)
	c.NotStrContains(gitLine, "0", "git-sourced row rendered a zero version")
}

// The JSON shapes: list -j wraps the rows in the {"rows": …} envelope, list
// -J emits one bare row per line, and get/put -j/-J emit the full row
// including code with NO envelope. These are the shapes a script keys on —
// the likeliest silent-drift point in the emit helpers — so they are pinned
// here rather than only end-to-end.
func TestPythonJSONShapes(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(emitPymoduleList(&listBuf, []*rafikiv1.PymoduleRow{codeless}, outputJSON, false))
	var listEnv struct {
		Rows []*rafikiv1.PymoduleRow `json:"rows"`
	}
	if err := json.Unmarshal(listBuf.Bytes(), &listEnv); err != nil {
		t.Fatalf("list -j: not a {\"rows\": …} envelope: %v\n%s", err, listBuf.String())
	}
	c.False(len(listEnv.Rows) != 1 || listEnv.Rows[0].Name != "helper", "list -j rows = %+v, want [helper]", listEnv.Rows)
	if bytes.Contains(listBuf.Bytes(), []byte(`"code"`)) {
		t.Errorf("list -j carries a code key:\n%s", listBuf.String())
	}

	// list -J: one compact row per line, no envelope.
	var jsonlBuf bytes.Buffer
	c.Require().NoError(emitPymoduleList(&jsonlBuf, []*rafikiv1.PymoduleRow{codeless, codeless}, outputJSONL, false))
	lines := strings.Split(strings.TrimRight(jsonlBuf.String(), "\n"), "\n")
	c.Require().Len(lines, 2, "list -J emitted %d lines, want 2:\n%s", len(lines), jsonlBuf.String())
	for i, line := range lines {
		var row rafikiv1.PymoduleRow
		err := json.Unmarshal([]byte(line), &row)
		c.NoError(err, "list -J line %d: not a bare row: %v\n%s", i, err, line)
		c.Eq("helper", row.Name, "list -J line %d: name %q, want helper", i, row.Name)
	}

	// get/put -j: the full row including code, with no envelope around it.
	for _, mode := range []outputMode{outputJSON, outputJSONL} {
		var buf bytes.Buffer
		c.Require().NoError(emitPymoduleCode(&buf, wireRow(), mode))
		var row rafikiv1.PymoduleRow
		err := json.Unmarshal(buf.Bytes(), &row)
		c.Require().NoError(err, "emitPymoduleCode mode %v: not a bare row: %v\n%s", mode, err, buf.String())
		c.False(row.Code != "x = 1" || row.Version != 7 || row.Name != "helper", "emitPymoduleCode mode %v: got name=%q version=%d code=%q, want helper/7/\"x = 1\"", mode, row.Name, row.Version, row.Code)
	}

	// get in table mode writes the code raw to w — byte-for-byte, no
	// decoration — so it can feed a file or an editor unchanged.
	var rawBuf bytes.Buffer
	c.Require().NoError(emitPymoduleCode(&rawBuf, wireRow(), outputAuto))
	c.Eq("x = 1\n", rawBuf.String(), "emitPymoduleCode table mode")
}
