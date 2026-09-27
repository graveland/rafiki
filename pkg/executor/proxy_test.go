// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executor"

	"github.com/multigres/testkit/assert"
)

func TestParseProxyFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := executor.ParseProxyFlags([]string{
		"vmlx=http://localhost:8005",
		"ollama=http://localhost:11434",
	})
	c.Require().NoError(err, "ParseProxyFlags")
	c.False(got["vmlx"] != "http://localhost:8005" || got["ollama"] != "http://localhost:11434", "got %v", got)
}

func TestParseProxyFlagsRejects(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no equals", "vmlx", "want name=url"},
		{"empty name", "=http://x", "empty proxy name"},
		{"empty url", "vmlx=", "empty base url"},
		{"not a url", "vmlx=:::", "invalid base url"},
		{"non-http scheme", "vmlx=file:///etc/passwd", "must be http or https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, err := executor.ParseProxyFlags([]string{tc.in})
			c.Require().Error(err, "accepted %q", tc.in)
			c.StrContains(err.Error(), tc.want, "error")
		})
	}
}

func TestParseProxyFlagsRejectsDuplicate(t *testing.T) {
	_, err := executor.ParseProxyFlags([]string{"a=http://1", "a=http://2"})
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "duplicate"), "err = %v, want a duplicate-name error", err)
}
