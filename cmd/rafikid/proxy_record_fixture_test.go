// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// captureFixtureDir is the committed snapshot `make capture-claude` writes.
const captureFixtureDir = "../../testdata/claude-capture"

// TestCaptureFixtureIsUsableAndShareable guards the one capture checked into
// this repo. It has two jobs.
//
// First, the fixture must be a usable record of a real session: request bodies
// that parse, SSE responses with the frames a client sees, and a tool call with
// the client's answer to it. That is what makes it worth keeping across client
// releases — the bodies are the protocol.
//
// Second, and more important, it must stay SHAREABLE. A capture is recorded from
// a live account, and the recorder digests credentials and the account identity;
// this test is the backstop that fails the build if either ever reaches the
// repository in the clear. Regenerate with `make capture-claude`.
func TestCaptureFixtureIsUsableAndShareable(t *testing.T) {
	c := assert.NewCollecting(t)
	entries, err := os.ReadDir(captureFixtureDir)
	c.Require().NoError(err, "read %s (regenerate with `make capture-claude`)", captureFixtureDir)

	var requests, responses []string
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), "-request.http"):
			requests = append(requests, e.Name())
		case strings.HasSuffix(e.Name(), "-response.http"):
			responses = append(responses, e.Name())
		}
	}
	sort.Strings(requests)
	sort.Strings(responses)
	c.Require().True(len(requests) > 0, "the fixture must hold at least one exchange")
	c.Eq(len(requests), len(responses), "every request must have its response")

	toolCalls, toolResults := 0, 0
	for _, name := range requests {
		raw, err := os.ReadFile(filepath.Join(captureFixtureDir, name))
		c.Require().NoError(err, "read %s", name)
		req := parseFixtureHTTP(t, raw, name)

		var body struct {
			Model    string            `json:"model"`
			Messages []json.RawMessage `json:"messages"`
			Tools    []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		c.Require().NoError(json.Unmarshal([]byte(req.body), &body), "%s body must be JSON", name)
		c.True(body.Model != "", "%s must name a model", name)
		c.True(len(body.Messages) > 0, "%s must carry messages", name)
		c.StrContains(body.Metadata.UserID, "sha256:",
			"%s: the account identity must be a digest, never the real one", name)
		if strings.Contains(req.body, "tool_result") {
			toolResults++
		}
	}
	for _, name := range responses {
		raw, err := os.ReadFile(filepath.Join(captureFixtureDir, name))
		c.Require().NoError(err, "read %s", name)
		resp := parseFixtureHTTP(t, raw, name)
		c.StrContains(resp.status, "200", "%s: the fixture records successful turns", name)
		c.StrContains(resp.headers["Content-Type"], "text/event-stream", "%s must be an SSE response", name)
		c.StrContains(resp.body, "message_start", "%s must carry the SSE frames a client sees", name)
		if strings.Contains(resp.body, `"name":"Bash"`) {
			toolCalls++
		}
	}
	c.True(toolCalls > 0, "the fixture must contain a tool call, or it proves nothing about tool traffic")
	c.True(toolResults > 0, "the fixture must contain the client's tool_result, or the round trip is missing")

	// The shareability backstop: a live credential in the tree would be a leak,
	// and a digest is the only form allowed.
	for _, name := range append(append([]string{}, requests...), responses...) {
		raw, err := os.ReadFile(filepath.Join(captureFixtureDir, name))
		c.Require().NoError(err, "read %s", name)
		text := string(raw)
		for _, leak := range []string{"sk-ant-", "Bearer sk-", "api03-", "oat01-"} {
			c.NotStrContains(text, leak, "%s must not carry a live credential", name)
		}
	}
}

// fixtureHTTP is one parsed .http file: the status or request line, the header
// values by canonical name, and the body verbatim.
type fixtureHTTP struct {
	status  string
	headers map[string]string
	body    string
}

func parseFixtureHTTP(t *testing.T, raw []byte, name string) fixtureHTTP {
	t.Helper()
	c := assert.NewCollecting(t)
	head, body, ok := strings.Cut(string(raw), "\r\n\r\n")
	c.Require().True(ok, "%s must have a header/body separator", name)
	lines := strings.Split(head, "\r\n")
	out := fixtureHTTP{status: lines[0], headers: map[string]string{}, body: body}
	for _, line := range lines[1:] {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		out.headers[k] = v
	}
	c.True(strings.HasPrefix(out.status, "HTTP/1.1") || strings.HasPrefix(out.status, "POST "),
		"%s must start with a request or status line, got %q", name, out.status)
	return out
}
