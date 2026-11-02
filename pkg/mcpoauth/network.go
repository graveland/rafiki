// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The bounds every request made through a Policy is held to. They are constants
// rather than settings on purpose: an operator cannot widen them from config,
// because a config that could would be a config that can make a redirect chain
// or a response body unbounded.
const (
	// OperationTimeout bounds one non-streaming operation end to end,
	// including reading the body.
	OperationTimeout = 30 * time.Second
	// ConnectTimeout bounds the TCP connect to one already-checked address.
	ConnectTimeout = 10 * time.Second
	// TLSHandshakeTimeout bounds the handshake, which is what proves the peer
	// is the host we resolved rather than the address we dialled.
	TLSHandshakeTimeout = 10 * time.Second
	// ResponseHeaderTimeout bounds how long a server may take to start
	// answering. It is the only bound a streaming client gets for the initial
	// response: an SSE session may live as long as its caller's context, but it
	// may not hang before the first byte.
	ResponseHeaderTimeout = 30 * time.Second
	// MaxResponseBytes caps a non-streaming response body. Reading past it is
	// an ERROR, never a truncated success: a JSON body cut in half parses as
	// garbage a caller cannot tell from a server bug, and a token response that
	// happens to fit would hide the ones that did not.
	MaxResponseBytes = 1 << 20
	// MaxRedirects is how many redirects one request may follow. Every hop is
	// re-checked, and only same-origin hops are followed at all.
	MaxRedirects = 5
)

var (
	// ErrDestinationDenied reports a destination the [mcp_oauth] policy
	// refuses: a non-public address, a non-public host with no allow_hosts or
	// allow_cidrs entry, a cleartext origin that is not in allow_http_hosts, or
	// a name that does not resolve.
	ErrDestinationDenied = errors.New("mcpoauth: destination not allowed by [mcp_oauth]")

	// ErrRedirectRefused reports a redirect that was not followed, because it
	// changed origin, downgraded HTTPS to HTTP, or ran past MaxRedirects.
	ErrRedirectRefused = errors.New("mcpoauth: redirect refused")

	// ErrResponseTooLarge reports a body longer than MaxResponseBytes.
	ErrResponseTooLarge = fmt.Errorf("mcpoauth: response body exceeds %d bytes", MaxResponseBytes)
)

// Config is the [mcp_oauth] section of rafikid.toml: the daemon operator's
// opt-in for destinations that are not public HTTPS.
//
// This is daemon configuration and nothing else. A project .mcp.json entry, a
// login flag from an ordinary user or a discovery response naming a new origin
// cannot widen it — none of them can reach a Policy except through this struct,
// which only rafikid's own config file produces.
type Config struct {
	// AllowHosts are the exact hostnames and IP literals the daemon may reach
	// even when they are not public: 127.0.0.1, "internal.example", 10.0.0.7.
	// No wildcards, no ports, no schemes. Case-insensitive, and an IP's mapped
	// spelling is the same key as its plain one.
	AllowHosts []string `toml:"allow_hosts"`

	// AllowCIDRs are netip prefixes (10.0.0.0/8, fd00::/8) whose addresses are
	// permitted when the host is not in AllowHosts. Host bits are masked.
	AllowCIDRs []string `toml:"allow_cidrs"`

	// AllowHTTPHosts are the exact hostnames and IP literals that may be
	// reached over cleartext HTTP. It does not permit a non-public address: an
	// http:// URL still needs its address to be public or covered by AllowHosts
	// or AllowCIDRs. An https:// URL never needs an entry here.
	AllowHTTPHosts []string `toml:"allow_http_hosts"`
}

// Policy is the compiled form of the [mcp_oauth] section: the destination gate
// every OAuth leg and every authenticated MCP request passes through, and the
// two guarded HTTP clients built on it.
//
// Its zero value is the default — public HTTPS only — so a Policy built from an
// absent [mcp_oauth] section denies everything that is not a public HTTPS
// destination. A nil *Policy behaves the same way rather than panicking: an
// unwired seam degrades to the default, never to a wider one.
type Policy struct {
	allowHosts     map[string]struct{}
	allowCIDRs     []netip.Prefix
	allowHTTPHosts map[string]struct{}

	// Test seams. Production leaves them nil: the daemon resolves through
	// net.DefaultResolver and dials with the standard dialer. They exist so a
	// test can point a public-looking host at a fixture on loopback WITHOUT
	// touching the policy decisions — the address a test's resolver answers is
	// still checked exactly as a real answer would be.
	resolve   ipResolver
	dial      dialFunc
	tlsConfig *tls.Config

	// opTimeout and headerTimeout default to OperationTimeout and
	// ResponseHeaderTimeout; tests shorten them instead of sleeping for 30s.
	opTimeout     time.Duration
	headerTimeout time.Duration

	once sync.Once
	base http.RoundTripper
}

// ipResolver is the resolution seam; *net.Resolver is one.
type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// dialFunc connects to an address. It is only ever handed a numeric
// host:port.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

var defaultDialer = &net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}

// Policy compiles the section, refusing any entry that is not a bare hostname,
// IP literal, prefix or CIDR. Invalid entries are a startup error: a wildcard
// or a URL typed into allow_hosts would otherwise sit there looking like a
// permission that does not exist.
func (c Config) Policy() (*Policy, error) {
	p := &Policy{
		allowHosts:     make(map[string]struct{}, len(c.AllowHosts)),
		allowHTTPHosts: make(map[string]struct{}, len(c.AllowHTTPHosts)),
	}
	for _, entry := range c.AllowHosts {
		key, err := hostEntryKey(entry)
		if err != nil {
			return nil, fmt.Errorf("mcp_oauth.allow_hosts: %w", err)
		}
		p.allowHosts[key] = struct{}{}
	}
	for _, entry := range c.AllowCIDRs {
		if entry != strings.TrimSpace(entry) {
			return nil, fmt.Errorf("mcp_oauth.allow_cidrs: %q has surrounding whitespace", entry)
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("mcp_oauth.allow_cidrs: %q is not a CIDR: %w", entry, err)
		}
		p.allowCIDRs = append(p.allowCIDRs, prefix.Masked())
	}
	for _, entry := range c.AllowHTTPHosts {
		key, err := hostEntryKey(entry)
		if err != nil {
			return nil, fmt.Errorf("mcp_oauth.allow_http_hosts: %w", err)
		}
		p.allowHTTPHosts[key] = struct{}{}
	}
	return p, nil
}

// ValidateURL reports whether the daemon may contact u, including that every
// address its host resolves to is permitted. It is the check for a URL the
// daemon is about to fetch, and equally for a URL derived from remote metadata
// that is about to be handed to a browser: an authorization endpoint named by
// discovery is validated here before it leaves the daemon.
//
// The DNS part is a pre-flight check, not the gate: the dial resolves again and
// checks every address it sees, which is what survives a rebinding answer.
func (p *Policy) ValidateURL(ctx context.Context, u *url.URL) error {
	p = p.orDefault()
	if err := checkTarget(u); err != nil {
		return err
	}
	if err := p.checkScheme(u); err != nil {
		return err
	}
	_, err := p.approvedAddr(ctx, u.Hostname())
	return err
}

// ValidateDestination parses raw and applies ValidateURL to it.
func (p *Policy) ValidateDestination(ctx context.Context, raw string) error {
	u, err := parseTarget(raw)
	if err != nil {
		return err
	}
	return p.ValidateURL(ctx, u)
}

// Client returns the client every OAuth leg uses: discovery, dynamic client
// registration, the code exchange, refresh and revocation.
//
// One request through it is bounded to OperationTimeout (body read included),
// its body to MaxResponseBytes, its redirects to MaxRedirects hops, and its
// destination to the policy — checked before the request and again at dial time.
// A redirect hop is its own request, so a chain is bounded to MaxRedirects+1
// such bounds; put a deadline on the context if you want one bound for the
// whole chain. There is no proxy: see transport.
func (p *Policy) Client() *http.Client {
	p = p.orDefault()
	return &http.Client{
		Transport:     &policyTransport{policy: p, base: p.baseTransport(), bounded: true},
		CheckRedirect: p.checkRedirect,
	}
}

// StreamClient returns the client for a long-lived MCP request over direct URL
// entries, where the body is an SSE stream.
//
// The difference from Client is deliberate: no whole-operation deadline, so a
// session lives as long as its caller's context instead of dying 30 seconds
// after it connected; the initial response is still bounded
// (ResponseHeaderTimeout), the destination is still checked before the request
// and at dial time, and the redirect rules are identical. Response bodies are
// NOT size-capped here — an event stream has no natural end — so a caller that
// wants a cap meters the stream itself.
func (p *Policy) StreamClient() *http.Client {
	p = p.orDefault()
	return &http.Client{
		Transport:     &policyTransport{policy: p, base: p.baseTransport()},
		CheckRedirect: p.checkRedirect,
	}
}

// checkScheme enforces the scheme half of the policy: HTTPS always, HTTP only
// for a host the operator listed in allow_http_hosts.
func (p *Policy) checkScheme(u *url.URL) error {
	p = p.orDefault()
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "https":
		return nil
	case "http":
		if _, ok := p.allowHTTPHosts[hostKeyForCompare(u.Hostname())]; !ok {
			return fmt.Errorf("%w: cleartext http://%s: the host is not in mcp_oauth.allow_http_hosts",
				ErrDestinationDenied, u.Host)
		}
		return nil
	default:
		return fmt.Errorf("%w: %q: scheme must be http or https", ErrInvalidURL, scheme)
	}
}

// approvedAddr resolves host and returns the one address to connect to. EVERY
// address the name resolves to must pass: a name that answers both a public and
// a private address is refused outright, because which one a connect would use
// is not ours to decide.
func (p *Policy) approvedAddr(ctx context.Context, host string) (netip.Addr, error) {
	p = p.orDefault()
	allowedHost := p.hostAllowed(host)
	key := hostKeyForCompare(host)
	if addr, err := parseAddr(key); err == nil {
		// An IP literal needs no lookup, and must not get one: resolving
		// "10.0.0.1" would only be a chance for something else to answer.
		if !allowedHost && !p.addrAllowed(addr) {
			return netip.Addr{}, fmt.Errorf("%w: address %s is not public and no allow_hosts or allow_cidrs entry covers it",
				ErrDestinationDenied, addr)
		}
		return addr, nil
	}
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: resolve %s: %v", ErrDestinationDenied, host, err)
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("%w: resolve %s: no addresses", ErrDestinationDenied, host)
	}
	var chosen netip.Addr
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !allowedHost && !p.addrAllowed(addr) {
			return netip.Addr{}, fmt.Errorf("%w: %s resolves to %s, which is not public and no allow_hosts or allow_cidrs entry covers it",
				ErrDestinationDenied, host, addr)
		}
		if !chosen.IsValid() {
			chosen = addr
		}
	}
	return chosen, nil
}

func (p *Policy) hostAllowed(host string) bool {
	if len(p.allowHosts) == 0 {
		return false
	}
	_, ok := p.allowHosts[hostKeyForCompare(host)]
	return ok
}

func (p *Policy) addrAllowed(addr netip.Addr) bool {
	if isPublicAddr(addr) {
		return true
	}
	for _, prefix := range p.allowCIDRs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// isPublicAddr reports whether addr is a destination on the public internet.
//
// IsGlobalUnicast alone is NOT sufficient, and that is measured rather than
// assumed: on Go 1.27 it says true for RFC 1918 space, carrier-grade NAT
// (100.64.0.0/10, which is also where some clouds put instance metadata),
// documentation ranges (192.0.2.0/24, 2001:db8::/32), benchmarking space
// (198.18.0.0/15) and 240.0.0.0/4. So the explicit families below are checked
// first, then the ranges the standard library calls global that are not
// destinations at all.
//
// IPv4-mapped IPv6 needs no special case beyond the Unmap: ::ffff:127.0.0.1 is
// 127.0.0.1 with a different spelling, and treating the two spellings
// differently is exactly the bypass this check exists to close.
func isPublicAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	switch {
	case addr.IsLoopback(),
		addr.IsPrivate(),          // RFC 1918 and IPv6 unique-local
		addr.IsLinkLocalUnicast(), // includes the 169.254.169.254 metadata services
		addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast(),
		addr.IsUnspecified(),
		!addr.IsGlobalUnicast(): // 0.0.0.0/8, the broadcast address, ...
		return false
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// reservedPrefixes are ranges that are not public destinations although the
// standard library's global-unicast test accepts them. The IPv6 entries that
// embed an IPv4 address (6to4, NAT64, Teredo, IPv4-compatible) are here because
// the address check above cannot see the IPv4 address inside them.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 CGNAT, incl. 100.100.100.200
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast (deprecated)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("::/96"),           // IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
}

// transport builds the one Transport both clients share, so a policy has one
// connection pool rather than one per client.
func (p *Policy) transport() *http.Transport {
	return &http.Transport{
		// No proxy, ever. ProxyFromEnvironment (the default) would let an
		// HTTP_PROXY/HTTPS_PROXY in the daemon's environment route the request
		// around every check here: the proxy resolves the name itself, we never
		// see the address it connected to, and the response looks normal. There
		// is no environment variable that can widen this boundary.
		Proxy: nil,
		// DialTLSContext rather than DialContext plus a TLSClientConfig: the
		// handshake has to name the HOST we resolved, not the numeric address we
		// connected to, or certificate verification runs against an IP that no
		// public certificate names. Doing both here is also what keeps "connect
		// to a checked address" and "verify the certificate for the name that
		// address came from" in one place.
		DialContext:           p.dialContext,
		DialTLSContext:        p.dialTLSContext,
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ResponseHeaderTimeout: p.responseHeaderTimeout(),
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// HTTP/2 stays off: the bundled transport only negotiates it with a
		// standard TLS dialer, and the custom one above is the security
		// property. HTTP/1.1 carries MCP's streamable SSE fine.
	}
}

func (p *Policy) baseTransport() http.RoundTripper {
	if p == nil {
		zero := &Policy{}
		return zero.transport()
	}
	p.once.Do(func() { p.base = p.transport() })
	return p.base
}

func (p *Policy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: unparsable dial address %q", ErrDestinationDenied, address)
	}
	addr, err := p.approvedAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	// A numeric address: the dialer has nothing left to resolve, so the address
	// checked above is the address connected to. Do not "improve" this into a
	// host:port dial with a Control hook — that hands the resolution back to the
	// dialer and reopens the rebinding window this closes.
	return p.dialer()(ctx, network, net.JoinHostPort(addr.String(), port))
}

func (p *Policy) dialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: unparsable dial address %q", ErrDestinationDenied, address)
	}
	addr, err := p.approvedAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	conn, err := p.dialer()(ctx, network, net.JoinHostPort(addr.String(), port))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if p != nil && p.tlsConfig != nil {
		cfg = p.tlsConfig.Clone()
		cfg.ServerName = host
	}
	tlsConn := tls.Client(conn, cfg)
	// The connect is bounded by the dialer; the handshake has to be bounded
	// here, since ResponseHeaderTimeout only starts once the request is written.
	hsCtx, cancel := context.WithTimeout(ctx, TLSHandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mcpoauth: TLS handshake with %s: %w", host, err)
	}
	return tlsConn, nil
}

// checkRedirect is the redirect half of both clients: same origin only, never an
// HTTPS downgrade, never more than MaxRedirects hops.
//
// The checks are not "is the target allowed" (a public second origin would pass
// that); they are "is the target the origin we authenticated to". A credential —
// the Authorization header of an MCP request, or a token request's client
// secret — rides the request the client resends, so following a redirect to
// another origin would hand it to whoever answered. The address check for a
// same-origin hop still happens at dial, like every other hop's.
func (p *Policy) checkRedirect(req *http.Request, via []*http.Request) error {
	p = p.orDefault()
	if len(via) > MaxRedirects {
		return fmt.Errorf("%w: stopped after %d redirects", ErrRedirectRefused, MaxRedirects)
	}
	if len(via) == 0 || req.URL == nil {
		return nil
	}
	prev := via[len(via)-1].URL
	next := req.URL
	if prev != nil && strings.EqualFold(prev.Scheme, "https") && strings.EqualFold(next.Scheme, "http") {
		// Checked before the allow_http_hosts gate and independent of it: an
		// operator permitting cleartext somewhere must not turn any redirect
		// into a downgrade.
		return fmt.Errorf("%w: HTTPS downgrade to %s", ErrRedirectRefused, next.Redacted())
	}
	if err := checkTarget(next); err != nil {
		return fmt.Errorf("%w: %w", ErrRedirectRefused, err)
	}
	if !SameOrigin(via[0].URL, next) {
		return fmt.Errorf("%w: cross-origin redirect to %s", ErrRedirectRefused, next.Redacted())
	}
	if err := p.checkScheme(next); err != nil {
		return fmt.Errorf("%w: %w", ErrRedirectRefused, err)
	}
	return nil
}

// policyTransport is the RoundTripper under both clients. Every request it
// carries is checked against the policy BEFORE a connection is opened, which
// gives a typed refusal instead of a dial error and catches an unapproved host
// without a socket; the dial then repeats the address check from its own
// lookup, which is what survives DNS rebinding.
type policyTransport struct {
	policy  *Policy
	base    http.RoundTripper
	bounded bool
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	policy := t.policy.orDefault()
	if req.URL == nil {
		return nil, fmt.Errorf("%w: request has no URL", ErrInvalidURL)
	}
	if err := checkTarget(req.URL); err != nil {
		return nil, err
	}
	if err := policy.checkScheme(req.URL); err != nil {
		return nil, err
	}
	ctx := req.Context()
	cancel := context.CancelFunc(func() {})
	if t.bounded {
		ctx, cancel = context.WithTimeout(ctx, policy.operationTimeout())
	}
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	body := resp.Body
	if t.bounded {
		body = &limitedBody{rc: body}
	}
	resp.Body = &cancelOnCloseBody{rc: body, cancel: cancel}
	return resp, nil
}

// limitedBody fails a body that runs past MaxResponseBytes, reading one byte
// beyond the cap so a body of exactly the cap is still a success.
type limitedBody struct {
	rc   io.ReadCloser
	n    int64
	over bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.over {
		return 0, ErrResponseTooLarge
	}
	remaining := int64(MaxResponseBytes) + 1 - b.n
	if remaining <= 0 {
		b.over = true
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.rc.Read(p)
	b.n += int64(n)
	if b.n > MaxResponseBytes {
		b.over = true
		return 0, ErrResponseTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// cancelOnCloseBody releases the operation's context when the caller is done
// with the response, not when RoundTrip returns: the deadline covers the body
// read.
type cancelOnCloseBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *cancelOnCloseBody) Close() error {
	err := b.rc.Close()
	b.cancel()
	return err
}

func (p *Policy) resolver() ipResolver {
	if p != nil && p.resolve != nil {
		return p.resolve
	}
	return net.DefaultResolver
}

func (p *Policy) dialer() dialFunc {
	if p != nil && p.dial != nil {
		return p.dial
	}
	return defaultDialer.DialContext
}

func (p *Policy) operationTimeout() time.Duration {
	if p != nil && p.opTimeout > 0 {
		return p.opTimeout
	}
	return OperationTimeout
}

func (p *Policy) responseHeaderTimeout() time.Duration {
	if p != nil && p.headerTimeout > 0 {
		return p.headerTimeout
	}
	return ResponseHeaderTimeout
}

// orDefault makes a nil *Policy behave as the zero Policy (public HTTPS only).
func (p *Policy) orDefault() *Policy {
	if p == nil {
		return &Policy{}
	}
	return p
}

// parseAddr parses an IP literal, unmapping an IPv4-mapped IPv6 address so both
// spellings classify identically.
func parseAddr(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap(), nil
}

// hostEntryKey validates one allow_hosts/allow_http_hosts entry and returns the
// comparison key. Entries are exact: a wildcard, a scheme, a port or a path is a
// config error rather than a permission that silently matches nothing.
func hostEntryKey(entry string) (string, error) {
	if entry == "" {
		return "", errors.New("empty entry")
	}
	if entry != strings.TrimSpace(entry) {
		return "", fmt.Errorf("%q has surrounding whitespace", entry)
	}
	if strings.ContainsRune(entry, '*') {
		return "", fmt.Errorf("%q: no wildcards; list each host exactly", entry)
	}
	key := hostKeyForCompare(entry)
	if key == "" {
		return "", fmt.Errorf("%q: empty host", entry)
	}
	if _, err := parseAddr(key); err == nil {
		return key, nil
	}
	if strings.ContainsAny(key, `/\?#@:[]`) {
		return "", fmt.Errorf("%q: want a bare hostname or IP address, without scheme, port or path", entry)
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return "", fmt.Errorf("%q: %q is not a hostname character", entry, r)
		}
	}
	return key, nil
}
