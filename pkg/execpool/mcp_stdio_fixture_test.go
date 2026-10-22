// SPDX-License-Identifier: Apache-2.0

package execpool

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The test binary doubles as the MCP server the executor hosts, so the relay is
// exercised against a real stdio MCP server with a real handshake — nothing in
// this path is a stand-in for either side.
//
//	-mcpstdio        a server exposing an "echo" tool
//	-mcpstdio-crash  a server that dies on startup, with a diagnostic on stderr
func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-mcpstdio":
			runMCPStdioFixture()
			os.Exit(0)
		case "-mcpstdio-crash":
			fmt.Fprintln(os.Stderr, "fatal: credentials expired")
			os.Exit(3)
		}
	}
	os.Exit(m.Run())
}

// mcpFixtureArgv is the declared argv a test hands the executor, naming this
// test binary in the given mode.
func mcpFixtureArgv(mode string) []string {
	return []string{os.Args[0], mode}
}

func runMCPStdioFixture() {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo the message back"},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Message string `json:"message"`
		}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Message}},
			}, nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fixture server:", err)
	}
}
