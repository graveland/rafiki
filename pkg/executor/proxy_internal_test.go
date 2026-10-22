// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// A request for "/" is a request for the operator's base URL itself, path and
// all — which is how an MCP endpoint declared as
// `--proxy codescan=http://host:8080/mcp` is reached at /mcp. Every other path
// still joins onto the base, and the escape guard still runs first.
func TestJoinProxyPathRootRequestKeepsTheBasePath(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"http://host:8080/mcp", "/", "http://host:8080/mcp"},
		{"http://host:8080/mcp/", "/", "http://host:8080/mcp/"},
		{"http://host:8080", "/", "http://host:8080/"},
		{"http://docker", "/", "http://docker/"},
		{"http://host:8080/mcp", "/tools", "http://host:8080/mcp/tools"},
		{"http://host:8080/api/v1", "/v1/messages", "http://host:8080/api/v1/v1/messages"},
		{"http://host:8080/mcp", "/?x=1", "http://host:8080/mcp?x=1"},
	}
	for _, tc := range cases {
		got, err := joinProxyPath(tc.base, tc.path)
		assert.NewAborting(t).Eq(got, tc.want, "joinProxyPath(%q, %q) (err %v)", tc.base, tc.path, err)
	}
}

// The escape guard is checked BEFORE the "/" short-circuit, so a climbing path
// can never be laundered through the base path.
func TestJoinProxyPathStillRefusesEscapes(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, p := range []string{"/..", "../x", "/a/../../b", "relative"} {
		_, err := joinProxyPath("http://host:8080/mcp", p)
		c.Require().Error(err, "joinProxyPath accepted %q", p)
	}
}
