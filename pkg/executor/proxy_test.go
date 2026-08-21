// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executor"
)

func TestParseProxyFlags(t *testing.T) {
	got, err := executor.ParseProxyFlags([]string{
		"vmlx=http://localhost:8005",
		"ollama=http://localhost:11434",
	})
	if err != nil {
		t.Fatalf("ParseProxyFlags: %v", err)
	}
	if got["vmlx"] != "http://localhost:8005" || got["ollama"] != "http://localhost:11434" {
		t.Errorf("got %v", got)
	}
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
			_, err := executor.ParseProxyFlags([]string{tc.in})
			if err == nil {
				t.Fatalf("accepted %q", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseProxyFlagsRejectsDuplicate(t *testing.T) {
	_, err := executor.ParseProxyFlags([]string{"a=http://1", "a=http://2"})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want a duplicate-name error", err)
	}
}
