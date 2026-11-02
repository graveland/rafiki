package mcpoauth

import (
	"net/url"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestMcpOAuthURLCanonicalizes pins the storage-key spelling: lowercase scheme
// and host, no default port, "/" for an empty path, and everything after the
// authority kept exactly as given.
func TestMcpOAuthURLCanonicalizes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"scheme and host are lowercased, path case is not", "HTTPS://Example.COM/MCP", "https://example.com/MCP"},
		{"the default https port is dropped", "https://example.com:443/mcp", "https://example.com/mcp"},
		{"the default http port is dropped", "http://example.com:80/mcp", "http://example.com/mcp"},
		{"a non-default port is kept", "https://example.com:8443/mcp", "https://example.com:8443/mcp"},
		{"an empty path becomes /", "https://example.com", "https://example.com/"},
		{"an empty path with a query still becomes /", "https://example.com?a=1", "https://example.com/?a=1"},
		{"an escaped path stays escaped", "https://example.com/a%2Fb", "https://example.com/a%2Fb"},
		{"query order is preserved", "https://example.com/mcp?b=2&a=1", "https://example.com/mcp?b=2&a=1"},
		{
			"an escaped resource parameter is preserved",
			"https://example.com/mcp?resource=https%3A%2F%2Fexample.com%2Fmcp",
			"https://example.com/mcp?resource=https%3A%2F%2Fexample.com%2Fmcp",
		},
		{"a trailing dot on a DNS name is dropped", "https://example.com./mcp", "https://example.com/mcp"},
		{"an IPv6 literal keeps its brackets, minus the default port", "https://[::1]:443/mcp", "https://[::1]/mcp"},
		{"an IPv6 literal with a non-default port keeps both", "https://[::1]:8443/mcp", "https://[::1]:8443/mcp"},
		{
			"an IPv4-mapped literal is the IPv4 address it encodes",
			"https://[::FFFF:10.0.0.1]/mcp",
			"https://10.0.0.1/mcp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Canonicalize(c.in)
			assert.NewAborting(t).NoError(err)
			assert.NewCollecting(t).Eq(c.want, got, c.in)
		})
	}
}

// TestMcpOAuthURLRejects covers everything the canonicalizer refuses: a URL
// that is not absolute HTTP(S), or that carries the two things that would make
// one stored key name two different requests.
func TestMcpOAuthURLRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"relative path", "/mcp"},
		{"relative host", "example.com/mcp"},
		{"no host", "https:///mcp"},
		{"non-HTTP scheme", "ftp://example.com/mcp"},
		{"opaque URL", "mailto:someone@example.com"},
		{"scheme without an authority", "https:example.com/mcp"},
		{"userinfo", "https://user:secret@example.com/mcp"},
		{"fragment", "https://example.com/mcp#section"},
		{"empty fragment", "https://example.com/mcp#"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Canonicalize(c.in)
			collect := assert.NewCollecting(t)
			collect.ErrorIs(err, ErrInvalidURL, c.in)
			collect.Eq("", got, c.in)
		})
	}
}

// TestMcpOAuthURLDoesNotCollide is the other half of canonicalization: two URLs
// that address different resources must never share a key, or a grant stored for
// one endpoint would authorize another.
func TestMcpOAuthURLDoesNotCollide(t *testing.T) {
	cases := []struct{ left, right string }{
		// An endpoint is not its origin: a server at /mcp and one at / are
		// separate resources, and Linear's PRM resource is the root while its
		// endpoint is /mcp.
		{"https://example.com/mcp", "https://example.com"},
		{"https://example.com/mcp", "https://example.com/"},
		{"https://example.com/mcp", "https://example.com/mcp/"},
		{"https://example.com/MCP", "https://example.com/mcp"},
		{"https://example.com/a%2Fb", "https://example.com/a/b"},
		{"https://example.com/mcp?a=1&b=2", "https://example.com/mcp?b=2&a=1"},
		{"https://example.com/mcp?a=1", "https://example.com/mcp"},
		{"https://example.com:8443/mcp", "https://example.com/mcp"},
		{"http://example.com/mcp", "https://example.com/mcp"},
		{"https://example.com/mcp", "https://other.example/mcp"},
	}
	for _, c := range cases {
		t.Run(c.left+" != "+c.right, func(t *testing.T) {
			left, err := Canonicalize(c.left)
			assert.NewAborting(t).NoError(err)
			right, err := Canonicalize(c.right)
			assert.NewAborting(t).NoError(err)
			assert.NewCollecting(t).NotEq(left, right, "these are different resources")
		})
	}
}

// TestMcpOAuthURLEquivalentSpellings is the positive side: spellings that name
// the same resource share a key.
func TestMcpOAuthURLEquivalentSpellings(t *testing.T) {
	cases := []struct{ left, right string }{
		{"https://example.com/mcp", "https://EXAMPLE.com:443/mcp"},
		{"https://example.com/mcp", "HTTPS://example.com/mcp"},
		{"https://example.com", "https://example.com:443/"},
		{"https://example.com/mcp", "https://example.com./mcp"},
		{"http://example.com/mcp", "http://example.com:80/mcp"},
		{"https://[::1]/mcp", "https://[::1]:443/mcp"},
		{"https://10.0.0.1/mcp", "https://[::ffff:10.0.0.1]/mcp"},
	}
	for _, c := range cases {
		t.Run(c.left+" == "+c.right, func(t *testing.T) {
			left, err := Canonicalize(c.left)
			assert.NewAborting(t).NoError(err)
			right, err := Canonicalize(c.right)
			assert.NewAborting(t).NoError(err)
			assert.NewCollecting(t).Eq(left, right, "these are the same resource")
		})
	}
}

func TestMcpOAuthURLSameOrigin(t *testing.T) {
	cases := []struct {
		left, right string
		want        bool
	}{
		{"https://example.com/a", "https://example.com:443/b", true},
		{"http://example.com/a", "http://example.com:80/b", true},
		{"https://example.com/a", "https://EXAMPLE.com/b", true},
		{"https://example.com/a", "http://example.com/a", false},
		{"https://example.com/a", "https://example.com:8443/a", false},
		{"https://example.com/a", "https://other.example/a", false},
		{"https://example.com/a", "https://example.com.evil/a", false},
	}
	for _, c := range cases {
		t.Run(c.left+" vs "+c.right, func(t *testing.T) {
			left, err := url.Parse(c.left)
			assert.NewAborting(t).NoError(err)
			right, err := url.Parse(c.right)
			assert.NewAborting(t).NoError(err)
			assert.NewCollecting(t).Eq(c.want, SameOrigin(left, right))
		})
	}
	assert.NewCollecting(t).False(SameOrigin(nil, nil), "nil URLs are not the same origin")
}
