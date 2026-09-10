// SPDX-License-Identifier: Apache-2.0

package integration_test

// Proves the seam the claude MCP injection wires together: proxyenv.ClaudeEnv's
// Values.MCPConfig (rendered into --mcp-config= by the argv producer, exactly
// as the real spawn paths do), dialed for real against a live daemon with the
// RAFIKI_MCP_TOKEN placeholder substituted, must reach the exact 12-tool
// agent-control surface a real user token gets. Nothing else in this package
// touches the argv/env-injection path — mcp_test.go's own tests build their
// MCP client directly against the daemon's real /mcp mount.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/proxyenv"
)

func TestMCPInjectedConfigReachesTwelveTools(t *testing.T) {
	t.Parallel()
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
	if mcpToken != token {
		t.Fatalf("RAFIKI_MCP_TOKEN = %q, want the user token %q", mcpToken, token)
	}

	var mcpConfigJSON string
	if vals.MCPConfig == "" {
		t.Fatalf("values = %+v, missing MCPConfig", vals)
	}
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
	if !ok {
		t.Fatalf("mcpServers = %v, missing \"rafiki\"", doc.MCPServers)
	}
	if rafiki.Type != "http" {
		t.Errorf("type = %q, want \"http\"", rafiki.Type)
	}
	if rafiki.URL != d.proxyURL+"/mcp" {
		t.Fatalf("url = %q, want %q", rafiki.URL, d.proxyURL+"/mcp")
	}
	gotHeader := rafiki.Headers["Authorization"]
	wantPlaceholder := "Bearer ${RAFIKI_MCP_TOKEN}"
	if gotHeader != wantPlaceholder {
		t.Fatalf("Authorization header = %q, want the literal placeholder %q", gotHeader, wantPlaceholder)
	}
	// The token must never be inlined into the JSON — argv is world-readable
	// via ps; only the placeholder may travel there.
	if strings.Contains(mcpConfigJSON, mcpToken) {
		t.Errorf("--mcp-config inlines the user token; only the ${RAFIKI_MCP_TOKEN} placeholder may travel in argv (%s)", mcpConfigJSON)
	}
	// Perform the substitution Claude Code would do, then actually connect.
	resolvedHeader := strings.ReplaceAll(gotHeader, "${RAFIKI_MCP_TOKEN}", mcpToken)
	resolvedToken := strings.TrimPrefix(resolvedHeader, "Bearer ")

	// mcpConnect appends /mcp itself, so pass the base URL.
	sess := mcpConnect(t, rafiki.URL[:len(rafiki.URL)-len("/mcp")], resolvedToken)
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var got []string
	for _, tool := range tools.Tools {
		got = append(got, tool.Name)
	}
	for _, want := range mcpToolNames {
		if !slices.Contains(got, want) {
			t.Errorf("tool %q missing from injected-config session; got %v", want, got)
		}
	}
	if len(got) != len(mcpToolNames) {
		t.Errorf("got %d tools %v, want exactly %d (%v)", len(got), got, len(mcpToolNames), mcpToolNames)
	}
}
