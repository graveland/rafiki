// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
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
	// OperationTimeout bounds one non-streaming OPERATION end to end: the whole
	// redirect chain and the body read, not one hop of it. It is installed as
	// http.Client.Timeout on the Client, which is what makes a chain of
	// MaxRedirects hops cost one bound rather than one bound each.
	OperationTimeout = 30 * time.Second
	// ConnectTimeout bounds the DNS lookup that produces the address, and the
	// TCP connect to that one already-checked address. The lookup is inside the
	// bound because a dial runs on the request's context and the streaming
	// client's context has no deadline: without it a silent resolver is the only
	// unbounded wait left in the dial.
	ConnectTimeout = 10 * time.Second
	// TLSHandshakeTimeout bounds the handshake, which is what proves the peer
	// is the host we resolved rather than the address we dialled.
	TLSHandshakeTimeout = 10 * time.Second
	// ResponseHeaderTimeout bounds how long a server may take to start
	// answering. It is the only bound a streaming client gets for the initial
	// response: an SSE session may live as long as its caller's context, but it
	// may not hang before the first byte.
	ResponseHeaderTimeout = 30 * time.Second
	// MaxResponseBytes caps a response body, on BOTH clients. The one exception
	// is a text/event-stream reply, and only on the streaming client: the content
	// type is the server's to choose, and no OAuth leg reads an event stream, so
	// honouring it there would let a hostile endpoint opt out of the cap. Reading
	// past it is an ERROR, never a truncated success: a JSON body cut in half
	// parses as garbage a caller cannot tell from a server bug, and a token
	// response that happens to fit would hide the ones that did not.
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
	//
	// A HOSTNAME here is a trust in that name's DNS, entirely and deliberately:
	// the waiver covers every address the name answers with, at the pre-flight
	// check and again at dial time, so a name that answers 169.254.169.254 is
	// reachable and the daemon cannot tell that apart from a legitimate internal
	// address. That is the point of the entry — the operator named the host, and
	// the host owns its own answers — but it means this is the widest of the two
	// opt-ins. An operator who wants the daemon reachable only inside a range
	// lists that range in AllowCIDRs (or the IP literal here) instead, which
	// narrows the permission to addresses rather than to a name's DNS.
	AllowHosts []string `toml:"allow_hosts"`

	// AllowCIDRs are netip prefixes (10.0.0.0/8, fd00::/8) whose addresses are
	// permitted when the host is not in AllowHosts. Host bits are masked. An
	// IPv4-mapped entry (::ffff:10.0.0.0/104) is accepted and converted to the
	// IPv4 prefix it denotes, because candidates are unmapped before comparison
	// and the mapped spelling would otherwise be a permission that matches
	// nothing; see allowCIDRPrefix.
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

	// opTimeout, headerTimeout and lookupTimeout default to OperationTimeout,
	// ResponseHeaderTimeout and ConnectTimeout; tests shorten them instead of
	// sleeping for the real bound.
	opTimeout     time.Duration
	headerTimeout time.Duration
	lookupTimeout time.Duration

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
		prefix, err = allowCIDRPrefix(entry, prefix)
		if err != nil {
			return nil, err
		}
		p.allowCIDRs = append(p.allowCIDRs, prefix)
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

// allowCIDRPrefix turns one allow_cidrs entry into the spelling the address
// check compares against.
//
// An IPv4-mapped prefix (::ffff:10.0.0.0/104) is converted to the IPv4 prefix it
// denotes (10.0.0.0/8). Every candidate address is Unmap()ped before it is
// compared, and netip.Prefix.Contains never matches an IPv4 address against an
// IPv6 prefix, so the mapped spelling would parse, pass startup validation and
// then match nothing — the exact silent no-op the validation exists to prevent.
// A mapped prefix shorter than /96 covers more than the IPv4 address it embeds
// and has no IPv4 spelling, so it is refused.
func allowCIDRPrefix(entry string, prefix netip.Prefix) (netip.Prefix, error) {
	if !prefix.Addr().Is4In6() {
		return prefix.Masked(), nil
	}
	bits := prefix.Bits()
	if bits < 96 {
		return netip.Prefix{}, fmt.Errorf("mcp_oauth.allow_cidrs: %q is an IPv4-mapped prefix shorter than /96 and has no IPv4 spelling; write the IPv4 prefix itself", entry)
	}
	return netip.PrefixFrom(prefix.Addr().Unmap(), bits-96).Masked(), nil
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
	_, err := p.approvedAddrs(ctx, u.Hostname())
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
// One request through it is one operation: the whole redirect chain, body read
// included, is bounded to OperationTimeout (http.Client.Timeout, so a chain of
// MaxRedirects hops cannot multiply it), its body to MaxResponseBytes whatever
// content type the server claimed, its redirects to same-origin MaxRedirects
// hops, and its destination to the policy — checked before the request and again
// at dial time. There is no proxy: see transport.
func (p *Policy) Client() *http.Client {
	p = p.orDefault()
	return &http.Client{
		Transport:     &policyTransport{policy: p, base: p.baseTransport()},
		CheckRedirect: p.checkRedirect,
		Timeout:       p.operationTimeout(),
	}
}

// StreamClient returns the client for a long-lived MCP request over direct URL
// entries, where the body is an SSE stream.
//
// The difference from Client is deliberate: no whole-operation deadline, so a
// session lives as long as its caller's context instead of dying 30 seconds
// after it connected; the initial response is still bounded
// (ResponseHeaderTimeout), the destination is still checked before the request
// and at dial time, and the redirect rules are identical. A response body is
// still capped at MaxResponseBytes UNLESS it is text/event-stream, because this
// client also reads ordinary JSON replies and those are finite like any other; an
// event stream has no natural end, so it stays uncapped and its caller meters it.
// That exemption exists HERE and nowhere else: Client() caps every body whatever
// content type the server claims.
func (p *Policy) StreamClient() *http.Client {
	p = p.orDefault()
	return &http.Client{
		Transport:     &policyTransport{policy: p, base: p.baseTransport(), stream: true},
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

// approvedAddrs resolves host and returns EVERY address the policy allows a
// connection to, in the order the resolver answered. Every address the name
// resolves to must pass: a name that answers both a public and a private address
// is refused outright, because which one a connect would use is not ours to
// decide.
//
// The returned list is what a dial may connect to: it tries those addresses in
// this order and re-resolves nothing, so a fallback cannot reach an address that
// a second lookup answered (the rebinding window dialApproved's callers exist to
// close).
func (p *Policy) approvedAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	p = p.orDefault()
	allowedHost := p.hostAllowed(host)
	key := hostKeyForCompare(host)
	if addr, err := parseAddr(key); err == nil {
		// An IP literal needs no lookup, and must not get one: resolving
		// "10.0.0.1" would only be a chance for something else to answer.
		if !allowedHost && !p.addrAllowed(addr) {
			return nil, fmt.Errorf("%w: address %s is not public and no allow_hosts or allow_cidrs entry covers it",
				ErrDestinationDenied, addr)
		}
		return []netip.Addr{addr}, nil
	}
	// The lookup carries its own bound (dnsTimeout). A dial runs on the REQUEST's
	// context, and StreamClient's has no deadline at all (that is the whole point
	// of it), so without this the only limit on a resolver that never answers is
	// the resolver's own — and a dial that hangs is a session that never starts.
	lookupCtx, cancel := context.WithTimeout(ctx, p.dnsTimeout())
	defer cancel()
	addrs, err := p.resolver().LookupNetIP(lookupCtx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s: %v", ErrDestinationDenied, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: resolve %s: no addresses", ErrDestinationDenied, host)
	}
	approved := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !allowedHost && !p.addrAllowed(addr) {
			return nil, fmt.Errorf("%w: %s resolves to %s, which is not public and no allow_hosts or allow_cidrs entry covers it",
				ErrDestinationDenied, host, addr)
		}
		approved = append(approved, addr)
	}
	return approved, nil
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
	// A zone is a local interface scope (%eth0), never part of a remote
	// destination, so it is REFUSED rather than stripped. netip.Prefix.Contains
	// is false for any zoned address, so stripping would be the fix that keeps
	// matching; refusing is the one that cannot silently weaken the check if a
	// later edit reorders these tests. Without this, a zoned address walks past
	// every reservedPrefixes entry: 2002::1%eth0 (6to4, embedding any IPv4) or
	// 64:ff9b::1.2.3.4%eth0 (NAT64) would classify as public.
	if addr.Zone() != "" {
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
//
// The IPv4-MAPPED form (::ffff:0:0/96) needs no entry: such an address is
// unmapped to its IPv4 address before the loop runs, so it is classified as the
// IPv4 address it carries — which is where 10/8 and 169.254.169.254 get caught.
// The IPv4-TRANSLATED form is a DIFFERENT prefix in a different place: RFC 6052
// §2.2 puts ::ffff:0:0:0/96's ffff at bits 64-79 rather than 80-95, so Is4In6 is
// false and Unmap does nothing. It carries an arbitrary IPv4 address in its low
// 32 bits with nothing to inspect it, so it is listed here.
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
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translatable (SIIT), RFC 6052
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64
	netip.MustParsePrefix("100::/64"),        // discard-only
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("2001:2::/48"),     // IPv6 benchmarking (RFC 5180)
	netip.MustParsePrefix("2001:10::/28"),    // ORCHID (RFC 4843)
	netip.MustParsePrefix("2001:20::/28"),    // ORCHIDv2 (RFC 7343)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
	netip.MustParsePrefix("5f00::/16"),       // SRv6 SIDs, not globally routable (RFC 9602)
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
		// HTTP/2 stays off, and not because a custom dialer forbids it: net/http
		// simply does not auto-enable its bundled HTTP/2 support when the
		// transport supplies its own dial or TLS config (ForceAttemptHTTP2 is the
		// opt-in, and it is unset here). Leaving it unset is deliberate, because
		// the ALPN list this transport would need lives in OUR tls.Config inside
		// dialTLSContext, which the h2 support never sees — enabling it would hand
		// protocol negotiation to a path that no longer owns the handshake. MCP's
		// streamable SSE is carried by HTTP/1.1 without losing anything.
	}
}

func (p *Policy) baseTransport() http.RoundTripper {
	// orDefault first: a nil *Policy gets the one package-level default, so the
	// nil path shares one Transport (and one connection pool) rather than building
	// a fresh one on every Client() call.
	p = p.orDefault()
	p.once.Do(func() { p.base = p.transport() })
	return p.base
}

func (p *Policy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: unparsable dial address %q", ErrDestinationDenied, address)
	}
	addrs, err := p.approvedAddrs(ctx, host)
	if err != nil {
		return nil, err
	}
	return p.dialApproved(ctx, network, port, addrs)
}

func (p *Policy) dialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: unparsable dial address %q", ErrDestinationDenied, address)
	}
	addrs, err := p.approvedAddrs(ctx, host)
	if err != nil {
		return nil, err
	}
	conn, err := p.dialApproved(ctx, network, port, addrs)
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

// dialApproved connects to the first of addrs that accepts, in the order the
// resolver answered them.
//
// The list is the one approvedAddrs just validated, passed in and never
// re-resolved here: a fallback may try a second address only because the SAME
// lookup checked it, so this cannot outrun the address check or reopen the
// rebinding window. Only a failed CONNECT falls through to the next address;
// once a connection is handed back, the TLS handshake is one address's business.
// Each attempt is bounded by the dialer's own ConnectTimeout and the whole
// sequence by the client's OperationTimeout, so trying every address cannot make
// a request unbounded.
//
// Every attempt dials a NUMERIC address. Do not "improve" this into a host:port
// dial with a Control hook — that hands the resolution back to the dialer.
func (p *Policy) dialApproved(ctx context.Context, network, port string, addrs []netip.Addr) (net.Conn, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: no approved address", ErrDestinationDenied)
	}
	dial := p.dialer()
	var lastErr error
	for _, addr := range addrs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// checkRedirect is the redirect half of both clients: same origin only, never a
// scheme change, never more than MaxRedirects hops.
//
// The checks are not "is the target allowed" (a public second origin would pass
// that); they are "is the target the origin we authenticated to". A credential —
// the Authorization header of an MCP request, or a token request's client
// secret — rides the request the client resends, so following a redirect to
// another origin would hand it to whoever answered. The address check for a
// same-origin hop still happens at dial, like every other hop's.
//
// A same-host http:// → https:// upgrade is refused as well, and that is
// deliberate rather than an accident of comparing origins: a scheme change also
// changes the port an origin means (80 → 443), so following it hands the request
// to a server the configuration never named — the same reason a downgrade is
// refused, one direction over. It is the safe direction, so an app that requires
// TLS at its token endpoint must be configured with https:// from the start
// instead of being upgraded into it. Configure the URL you mean; no redirect
// re-decides the scheme.
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
		return fmt.Errorf("%w: HTTPS downgrade to %s", ErrRedirectRefused, redactTarget(next))
	}
	if err := checkTarget(next); err != nil {
		return fmt.Errorf("%w: %w", ErrRedirectRefused, err)
	}
	if prev != nil && !strings.EqualFold(prev.Scheme, next.Scheme) {
		// The downgrade above is the dangerous direction and gets its own
		// message; this catches the rest, which after checkTarget means an
		// http:// → https:// upgrade on the same host. See the doc comment: a
		// scheme change moves the port, so it is not the same origin.
		return fmt.Errorf("%w: scheme change from %s to %s to %s", ErrRedirectRefused,
			strings.ToLower(prev.Scheme), strings.ToLower(next.Scheme), redactTarget(next))
	}
	if !SameOrigin(via[0].URL, next) {
		return fmt.Errorf("%w: cross-origin redirect to %s", ErrRedirectRefused, redactTarget(next))
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
//
// It also owns the response-body cap for both clients: an uncapped body is a
// hostile server's way to grow the daemon's heap, and only an event stream has
// no natural end. The whole-operation deadline is the Client's own Timeout, so a
// redirect chain is bounded once rather than once per hop.
type policyTransport struct {
	policy *Policy
	base   http.RoundTripper
	// stream marks the transport StreamClient built. Only that one exempts a
	// text/event-stream body from the cap: on Client() the content type is the
	// server's to choose and the OAuth legs never read an event stream, so an
	// exemption there is a hostile endpoint opting out of the cap.
	stream bool
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
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if !t.exemptFromCap(resp.Header.Get("Content-Type")) {
		resp.Body = &limitedBody{rc: resp.Body}
	}
	return resp, nil
}

// exemptFromCap reports whether this transport leaves the body uncapped, which is
// true only for an event stream fetched through the streaming client.
func (t *policyTransport) exemptFromCap(contentType string) bool {
	return t.stream && isEventStream(contentType)
}

// isEventStream reports whether ct is an SSE content type. It decides the
// exemption only for the streaming client; every other reply — a JSON-RPC answer,
// an OAuth error document, a redirect body — is finite and capped like any other.
func isEventStream(ct string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "text/event-stream")
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

// cancelOnCloseBody releases a request's context when the caller is done with the
// response rather than when RoundTrip returns, so a deadline or an abort also
// covers the body read. The login flow's recorder uses it too.
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

func (p *Policy) dnsTimeout() time.Duration {
	if p != nil && p.lookupTimeout > 0 {
		return p.lookupTimeout
	}
	return ConnectTimeout
}

// defaultPolicy is what a nil *Policy behaves as: the zero Policy, i.e. public
// HTTPS only. It is ONE package-level value rather than a fresh &Policy{} per
// call because the base transport is built once per Policy — a per-call default
// would build a Transport, and its connection pool, on every Client() call a
// nil-policy caller makes, leaking idle pools one request at a time.
var defaultPolicy = &Policy{}

// orDefault makes a nil *Policy behave as the zero Policy (public HTTPS only).
func (p *Policy) orDefault() *Policy {
	if p == nil {
		return defaultPolicy
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
