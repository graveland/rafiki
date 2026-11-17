// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// controlProtocolDocPath is docs/reference/control-protocol.md, read from this
// package's directory.
const controlProtocolDocPath = "../../docs/reference/control-protocol.md"

// protocolDocSection returns the section of docs/reference/control-protocol.md
// that starts with heading (an exact line) and ends before the next heading of
// the same or a higher level. An empty string means no such heading.
func protocolDocSection(t *testing.T, heading string) string {
	t.Helper()
	raw, err := os.ReadFile(controlProtocolDocPath)
	assert.NewAborting(t).NoError(err, "read %s", controlProtocolDocPath)
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if line != heading {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "### ") || strings.HasPrefix(lines[j], "## ") {
				return strings.Join(lines[i:j], "\n")
			}
		}
		return strings.Join(lines[i:], "\n")
	}
	return ""
}

// TestChildCredentialListScopeIsDocumented pins the ListConversations
// child-credential paragraph against childConversationScope: the doc must
// state both arms, the OWNER's corpus and the fallback to the caller's own
// subtree when its lineage has no owner. Naming only the first arm overclaims,
// which is what the paragraph said before.
func TestChildCredentialListScopeIsDocumented(t *testing.T) {
	c := assert.NewCollecting(t)
	section := protocolDocSection(t, "### Browsing conversations (`ListConversations`)")
	c.Require().NotEq("", section,
		"docs/reference/control-protocol.md has no ListConversations section")
	c.StrContains(section, "OWNER's whole corpus",
		"the section does not state the owner-corpus arm")
	c.StrContains(section, "no owner",
		"the section does not name the ownerless lineage")
	c.StrContains(section, "SUBTREE",
		"the section does not state the ownerless-lineage fallback to the caller's subtree")
}
