// SPDX-License-Identifier: Apache-2.0

package integration_test

// Proves the seam the claude MCP injection wires together: proxyenv.ClaudeEnv's
// Values.MCPConfig (rendered into --mcp-config= by the argv producer, exactly
// as the real spawn paths do), dialed for real against a live daemon with the
// RAFIKI_MCP_TOKEN placeholder substituted, must reach the exact agent-control
// tool surface a real user token gets. Nothing else in this package
// touches the argv/env-injection path — mcp_test.go's own tests build their
// MCP client directly against the daemon's real /mcp mount.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
)

func TestMCPInjectedConfigReachesTheAgentControlSurface(t *testing.T) {
	t.Parallel()
	c := assert.NewCollecting(t)
	d := bootMCPDaemon(t)
	token := d.createMCPUser(t)

	env, vals := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{
		URL:   d.proxyURL,
		Token: token,
	})

	// RAFIKI_MCP_TOKEN must be in env, and the JSON's Authorization header
	// must be the placeholder that expands to it — this is the same
	// substitution Claude Code performs at connect time; do it by hand here
	// since the test drives an MCP client directly, not a real claude binary.
	var mcpToken string
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == "RAFIKI_MCP_TOKEN" {
			mcpToken = v
		}
	}
	c.Require().Eq(token, mcpToken, "RAFIKI_MCP_TOKEN")

	var mcpConfigJSON string
	c.Require().NotEq("", vals.MCPConfig, "values = %+v, missing MCPConfig", vals)
	mcpConfigJSON = vals.MCPConfig

	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpConfigJSON), &doc); err != nil {
		t.Fatalf("--mcp-config is not valid JSON: %v (%s)", err, mcpConfigJSON)
	}
	rafiki, ok := doc.MCPServers["rafiki"]
	c.Require().True(ok, "mcpServers = %v, missing \"rafiki\"", doc.MCPServers)
	c.Eq("http", rafiki.Type, "type = %q, want \"http\"", rafiki.Type)
	c.Require().Eq(d.proxyURL+"/mcp", rafiki.URL, "url")
	gotHeader := rafiki.Headers["Authorization"]
	wantPlaceholder := "Bearer ${RAFIKI_MCP_TOKEN}"
	c.Require().Eq(wantPlaceholder, gotHeader, "Authorization header")
	// The token must never be inlined into the JSON — argv is world-readable
	// via ps; only the placeholder may travel there.
	c.NotStrContains(mcpConfigJSON, mcpToken, "--mcp-config inlines the user token; only the ${RAFIKI_MCP_TOKEN} placeholder may travel in argv (")
	// Perform the substitution Claude Code would do, then actually connect.
	resolvedHeader := strings.ReplaceAll(gotHeader, "${RAFIKI_MCP_TOKEN}", mcpToken)
	resolvedToken := strings.TrimPrefix(resolvedHeader, "Bearer ")

	// mcpConnect appends /mcp itself, so pass the base URL.
	sess := mcpConnect(t, rafiki.URL[:len(rafiki.URL)-len("/mcp")], resolvedToken)
	tools, err := sess.ListTools(context.Background(), nil)
	c.Require().NoError(err, "ListTools")
	var got []string
	for _, tool := range tools.Tools {
		got = append(got, tool.Name)
	}
	for _, want := range mcpToolNames {
		c.Contains(got, want, "tool")
	}
	c.Len(got, len(mcpToolNames), "got %d tools %v, want exactly %d (%v)", len(got), got, len(mcpToolNames), mcpToolNames)
}
