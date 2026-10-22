// SPDX-License-Identifier: Apache-2.0

// Command mcpstdio is the stdio MCP server the via_executor integration test
// has an executor host. It is a REAL MCP server (the go-sdk's), so the relay is
// exercised against a real handshake rather than a stand-in, and it can be told
// to die on its first tool call so the test can assert that its stderr reaches
// the model in the failed call's error.
//
//	-echo     (default) answer "echo <message>"
//	-crash-on-call  write a diagnostic to stderr and exit 3 on the first call
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	crashOnCall := flag.Bool("crash-on-call", false, "die on the first tool call, with a diagnostic on stderr")
	flag.Parse()

	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo the message back"},
		func(_ context.Context, _ *mcp.CallToolRequest, in struct {
			Message string `json:"message"`
		}) (*mcp.CallToolResult, any, error) {
			if *crashOnCall {
				fmt.Fprintln(os.Stderr, "fatal: credentials expired")
				os.Exit(3)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Message}},
			}, nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fixture server:", err)
		os.Exit(1)
	}
}
