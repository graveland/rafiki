// SPDX-License-Identifier: Apache-2.0

package rafikidconfig_test

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/rafikidconfig"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

// write puts body in a fresh temp file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rafikid.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadMissingFileIsZero(t *testing.T) {
	got, err := rafikidconfig.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assert.NewCollecting(t).EqDeep(rafikidconfig.File{}, got, "missing file must yield the zero File")
}

func TestLoadTelemetrySection(t *testing.T) {
	p := write(t, `
[telemetry]
endpoint = "http://collector:4318"
protocol = "grpc"
content = "rules"

[[telemetry.content_rule]]
user = "alice"
provider = "openrouter"

[[telemetry.content_rule]]
model = "anthropic/*"
`)
	got, err := rafikidconfig.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := telemetry.Config{
		Endpoint: "http://collector:4318",
		Protocol: "grpc",
		Content:  "rules",
		Rules: []telemetry.Rule{
			{User: "alice", Provider: "openrouter"},
			{Model: "anthropic/*"},
		},
	}
	assert.NewCollecting(t).EqDeep(want, got.Telemetry, "telemetry section")
}

func TestLoadUnknownKeyFails(t *testing.T) {
	p := write(t, "[telemetry]\nendpont = \"x\"\n")
	_, err := rafikidconfig.Load(p)
	if err == nil {
		t.Fatal("Load accepted an unknown key")
	}
	if want := "telemetry.endpont"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
}

func TestLoadUnknownSectionFails(t *testing.T) {
	p := write(t, "[nope]\na = 1\n")
	_, err := rafikidconfig.Load(p)
	if err == nil {
		t.Fatal("Load accepted an unknown section")
	}
	if want := "nope.a"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
}

// TestMcpOAuthConfigSectionLoads pins the [mcp_oauth] keys: the three operator
// opt-ins, decoded as written.
func TestMcpOAuthConfigSectionLoads(t *testing.T) {
	p := write(t, `
[mcp_oauth]
allow_hosts = ["127.0.0.1", "internal.example"]
allow_cidrs = ["10.0.0.0/8", "fd00::/8"]
allow_http_hosts = ["127.0.0.1"]
`)
	got, err := rafikidconfig.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assert.NewCollecting(t).EqDeep(mcpoauth.Config{
		AllowHosts:     []string{"127.0.0.1", "internal.example"},
		AllowCIDRs:     []string{"10.0.0.0/8", "fd00::/8"},
		AllowHTTPHosts: []string{"127.0.0.1"},
	}, got.MCPOAuth, "mcp_oauth section")
}

// TestMcpOAuthConfigDefaultsToEmpty: an absent section is the default policy —
// public HTTPS only — not an error.
func TestMcpOAuthConfigDefaultsToEmpty(t *testing.T) {
	got, err := rafikidconfig.Load(write(t, "[telemetry]\ncontent = \"off\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assert.NewCollecting(t).EqDeep(mcpoauth.Config{}, got.MCPOAuth, "no [mcp_oauth] section")
}

// TestMcpOAuthConfigUnknownKeyFails keeps the decoder strict inside the section:
// a misspelled opt-in must not sit there looking like a permission.
func TestMcpOAuthConfigUnknownKeyFails(t *testing.T) {
	for _, body := range []string{
		"[mcp_oauth]\nallow_host = [\"127.0.0.1\"]\n",
		"[mcp_oauth]\nallow_private = true\n",
	} {
		_, err := rafikidconfig.Load(write(t, body))
		if err == nil {
			t.Fatalf("Load accepted an unknown key:\n%s", body)
		}
		if !strings.Contains(err.Error(), "mcp_oauth.") {
			t.Errorf("error %q does not name the section", err)
		}
	}
}

// TestMcpOAuthConfigRejectsInvalidEntries refuses every entry that would look
// like a permission and match nothing.
func TestMcpOAuthConfigRejectsInvalidEntries(t *testing.T) {
	cases := []struct{ name, body string }{
		{"wildcard host", "[mcp_oauth]\nallow_hosts = [\"*.example.com\"]\n"},
		{"bare wildcard", "[mcp_oauth]\nallow_hosts = [\"*\"]\n"},
		{"host with a scheme", "[mcp_oauth]\nallow_hosts = [\"https://example.com\"]\n"},
		{"host with a port", "[mcp_oauth]\nallow_hosts = [\"example.com:8443\"]\n"},
		{"host with a path", "[mcp_oauth]\nallow_hosts = [\"example.com/mcp\"]\n"},
		{"empty host", "[mcp_oauth]\nallow_hosts = [\"\"]\n"},
		{"surrounding whitespace", "[mcp_oauth]\nallow_hosts = [\" example.com\"]\n"},
		{"malformed CIDR", "[mcp_oauth]\nallow_cidrs = [\"10.0.0.0/33\"]\n"},
		{"CIDR with a host name", "[mcp_oauth]\nallow_cidrs = [\"internal.example\"]\n"},
		{"malformed cleartext host", "[mcp_oauth]\nallow_http_hosts = [\"http://example.com\"]\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := rafikidconfig.Load(write(t, c.body))
			if err == nil {
				t.Fatalf("Load accepted:\n%s", c.body)
			}
			if !strings.Contains(err.Error(), "mcp_oauth.") {
				t.Errorf("error %q does not name the section", err)
			}
		})
	}
}

// TestMcpOAuthConfigAcceptsValidEntries covers the spellings an operator
// reasonably writes: a bracketed IPv6 literal, a CIDR with host bits set,
// uppercase, and a trailing dot.
func TestMcpOAuthConfigAcceptsValidEntries(t *testing.T) {
	p := write(t, `
[mcp_oauth]
allow_hosts = ["[::1]", "INTERNAL.example", "example.com.", "10.0.0.7"]
allow_cidrs = ["10.1.2.3/16"]
allow_http_hosts = ["::1"]
`)
	got, err := rafikidconfig.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	policy, err := got.MCPOAuth.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	// The CIDR's host bits were masked, so any address in 10.1.0.0/16 is in.
	if err := policy.ValidateURL(t.Context(), mustParse(t, "https://10.1.9.9/mcp")); err != nil {
		t.Errorf("10.1.0.0/16 should cover 10.1.9.9: %v", err)
	}
	// And the bracketed literal in allow_hosts is the loopback address, so the
	// bracketed spelling in a URL matches it.
	if err := policy.ValidateURL(t.Context(), mustParse(t, "https://[::1]/mcp")); err != nil {
		t.Errorf("the allow_hosts entry [::1] should permit ::1: %v", err)
	}
}

// TestMcpOAuthConfigPolicyIsPublicHTTPSOnly is the default's promise, checked
// through the policy the daemon will actually use.
func TestMcpOAuthConfigPolicyIsPublicHTTPSOnly(t *testing.T) {
	policy, err := rafikidconfig.File{}.MCPOAuth.Policy()
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	ctx := t.Context()
	cases := []struct {
		url     string
		allowed bool
	}{
		{"https://93.184.216.34/mcp", true},
		{"https://10.0.0.7/mcp", false},
		{"https://127.0.0.1/mcp", false},
		{"https://169.254.169.254/latest/meta-data/", false},
		{"http://93.184.216.34/mcp", false}, // cleartext needs allow_http_hosts
	}
	for _, c := range cases {
		err := policy.ValidateURL(ctx, mustParse(t, c.url))
		if c.allowed && err != nil {
			t.Errorf("%s should be allowed by the default policy: %v", c.url, err)
		}
		if !c.allowed && err == nil {
			t.Errorf("%s should be refused by the default policy", c.url)
		}
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}
