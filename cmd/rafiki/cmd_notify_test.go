// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// A positional field splits on the FIRST "=" only, so a value may itself
// contain "="; an empty value is still a value. The target, source and key
// ride as given — the daemon resolves the target name, not the client.
func TestNotifyCmdParsesFields(t *testing.T) {
	c := assert.NewCollecting(t)
	req, err := buildNotifyRequest(
		"ops-bot", "github", "repos/acme/rafiki/push",
		[]string{"a=b=c", "empty=", "plain=value"},
		nil, strings.NewReader(""),
	)
	c.Require().NoError(err, "buildNotifyRequest")

	c.Eq("ops-bot", req.GetTarget(), "Target")
	c.Eq("github", req.GetSource(), "Source")
	c.Eq("repos/acme/rafiki/push", req.GetKey(), "Key")
	c.EqDeep(map[string]string{
		"a":     "b=c",
		"empty": "",
		"plain": "value",
	}, req.GetFields(), "Fields")

	// Zero fields is allowed; the map is simply empty, not nil-checked by
	// the daemon.
	empty, err := buildNotifyRequest("ops-bot", "github", "", nil, nil, strings.NewReader(""))
	c.Require().NoError(err, "buildNotifyRequest with no fields")
	c.Len(empty.GetFields(), 0, "Fields with no field arguments")

	// A positional without "=" cannot be shaped into a field.
	_, err = buildNotifyRequest("ops-bot", "github", "", []string{"oops"}, nil, strings.NewReader(""))
	c.Require().Error(err, "a field without = must error")
	c.ErrorContains(err, `field "oops" is not name=value`, "unexpected message for a missing =")
}

// A field name given twice is refused rather than silently resolved, and
// that holds across positionals and --field-file entries alike.
func TestNotifyCmdRejectsDuplicateField(t *testing.T) {
	c := assert.NewCollecting(t)

	_, err := buildNotifyRequest("ops-bot", "github", "", []string{"a=1", "a=2"}, nil, strings.NewReader(""))
	c.Require().Error(err, "duplicate positional field must error")
	c.ErrorContains(err, `field "a" given twice`, "unexpected duplicate message")

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	c.Require().NoError(os.WriteFile(path, []byte("from-file"), 0o600), "write field file")

	_, err = buildNotifyRequest("ops-bot", "github", "", []string{"a=1"}, []string{"a=" + path}, strings.NewReader(""))
	c.Require().Error(err, "a field-file repeating a positional field must error")
	c.ErrorContains(err, `field "a" given twice`, "unexpected duplicate message across positional and field-file")

	_, err = buildNotifyRequest("ops-bot", "github", "", nil,
		[]string{"b=" + path, "b=" + path}, strings.NewReader(""))
	c.Require().Error(err, "two field-files naming the same field must error")
	c.ErrorContains(err, `field "b" given twice`, "unexpected duplicate message across field-files")
}

// --field-file uses file content VERBATIM (unlike a positional value) and a
// path of "-" reads stdin; at most one field may use stdin.
func TestNotifyCmdFieldFileStdin(t *testing.T) {
	c := assert.NewCollecting(t)

	stdin := "{\"alert\":\"disk full\"}\n"
	req, err := buildNotifyRequest("ops-bot", "pagerduty", "",
		[]string{"event=trigger"}, []string{"body=-"}, strings.NewReader(stdin))
	c.Require().NoError(err, "buildNotifyRequest reading a field from stdin")
	c.EqDeep(map[string]string{
		"body":  stdin, // trailing newline and all: no trimming
		"event": "trigger",
	}, req.GetFields(), "Fields")

	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	content := "  spaced \n\tcontent\n"
	c.Require().NoError(os.WriteFile(path, []byte(content), 0o600), "write field file")
	req, err = buildNotifyRequest("ops-bot", "sentry", "", nil,
		[]string{"body=" + path}, strings.NewReader(""))
	c.Require().NoError(err, "buildNotifyRequest reading a field from a file")
	c.EqDeep(map[string]string{"body": content}, req.GetFields(), "Fields from a file")

	_, err = buildNotifyRequest("ops-bot", "pagerduty", "", nil,
		[]string{"one=-", "two=-"}, strings.NewReader(stdin))
	c.Require().Error(err, "two fields reading stdin must error")
	c.ErrorContains(err, "at most one field may read stdin", "unexpected message for two stdin fields")

	_, err = buildNotifyRequest("ops-bot", "pagerduty", "", nil,
		[]string{"body=" + filepath.Join(dir, "missing.json")}, strings.NewReader(""))
	c.Require().Error(err, "an unreadable field file must error")
	c.ErrorContains(err, `field "body"`, "unexpected message for an unreadable field file")
}

// --source is required: cobra refuses the missing flag before RunE runs (so
// no connection is attempted), and the request builder refuses an empty one
// for any caller that reaches it another way.
func TestNotifyCmdRequiresSource(t *testing.T) {
	c := assert.NewCollecting(t)

	cmd := newNotifyCmd()
	cmd.SetArgs([]string{"ops-bot", "a=b"})
	err := cmd.Execute()
	c.Require().Error(err, "notify without --source must error")
	c.ErrorContains(err, `"source"`, "unexpected message for a missing --source")

	_, err = buildNotifyRequest("ops-bot", "", "", nil, nil, strings.NewReader(""))
	c.Require().Error(err, "buildNotifyRequest with an empty source must error")
	c.ErrorContains(err, "--source is required", "unexpected message for an empty source")
}

// The verb is reachable from the root command, not merely constructible.
func TestNotifyCmdIsRegistered(t *testing.T) {
	c := assert.NewCollecting(t)
	root := newRootCmd()
	notify, _, err := root.Find([]string{"notify"})
	c.Require().NoError(err, "locate notify")
	c.Eq("notify", notify.Name(), "resolved command name")
	c.Eq("notify <agent> [field=value ...]", notify.Use, "Use")
}
