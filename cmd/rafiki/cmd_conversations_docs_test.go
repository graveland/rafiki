// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// agentCLIDocPath is docs/agent-cli.md, read from this package's directory.
// The doc is the only place `rafiki conversations list` is described, so the
// test reads the file itself: the command's help text is a summary, and the
// finding it guards was a verb that changed meaning with no documentation.
const agentCLIDocPath = "../../docs/agent-cli.md"

// readAgentCLIDoc loads docs/agent-cli.md.
func readAgentCLIDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(agentCLIDocPath)
	assert.NewAborting(t).NoError(err, "read %s", agentCLIDocPath)
	return string(raw)
}

// markdownSection returns one level-2 section of doc: the heading line through
// the line before the next line that starts with "## ". An empty string means
// the section does not exist.
func markdownSection(doc, heading string) string {
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if line != heading {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "## ") {
				return strings.Join(lines[i:j], "\n")
			}
		}
		return strings.Join(lines[i:], "\n")
	}
	return ""
}

// TestConversationsListIsDocumented pins docs/agent-cli.md's listing section
// against the command itself: every flag the verb registers is named there,
// the output modes are spelled, and the alias move away from search is stated.
// A flag added to the command without a line in the doc fails here, which is
// what keeps a silently changed verb from being invisible again.
func TestConversationsListIsDocumented(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newConversationsListCmd()
	doc := readAgentCLIDoc(t)

	section := markdownSection(doc, "## `rafiki conversations list` (alias `ls`)")
	c.Require().NotEq("", section, "docs/agent-cli.md has no `rafiki conversations list` section")

	for _, name := range []string{
		"limit", "cursor", "all", "sort", "parent", "query", "state", "kind",
		"name", "cwd", "label", "since", "until",
	} {
		c.True(cmd.Flags().Lookup(name) != nil, "--%s is not a flag of the list command", name)
		c.StrContains(section, "--"+name, "the listing section does not document --%s", name)
	}

	// The alias move is the finding: `ls` and `list` used to run search, and the
	// search section is where a reader of the old spelling must learn it.
	c.Contains(cmd.Aliases, "ls", "list must stay aliased to ls")
	search := markdownSection(doc, "## `search`")
	c.Require().NotEq("", search, "docs/agent-cli.md has no `search` section")
	c.StrContains(search, "`rafiki conversations ls` and `list` no longer mean search",
		"the search section must say that `rafiki conversations ls`/`list` used to run search")
	c.StrContains(search, "`rafiki conversations search`",
		"the alias note does not point at the verb that still runs search")

	// Output modes: the three-way contract, spelled on this verb too.
	c.StrContains(section, "`-o json`", "the listing section does not document the pretty JSON arm")
	c.StrContains(section, "`-o jsonl`", "the listing section does not document the JSONL arm")
	c.StrContains(section, "next_cursor", "the listing section does not document the cursor")
}
