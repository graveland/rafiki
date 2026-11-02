package mcpoauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// scriptedResolver answers for a host from a queue of answers and repeats the
// last one. Successive answers are what model a rebinding name: public on the
// pre-flight lookup, private on the dial's own lookup.
type scriptedResolver struct {
	mu    sync.Mutex
	queue map[string][][]string
	calls map[string]int
}

func newResolver() *scriptedResolver {
	return &scriptedResolver{queue: map[string][][]string{}, calls: map[string]int{}}
}

// answer queues one DNS answer for host: the addresses it returns on the next
// lookup.
func (r *scriptedResolver) answer(host string, ips ...string) *scriptedResolver {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue[host] = append(r.queue[host], ips)
	return r
}

func (r *scriptedResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[host]++
	answers := r.queue[host]
	if len(answers) == 0 {
		return nil, fmt.Errorf("scriptedResolver: no answer for %q", host)
	}
	i := r.calls[host] - 1
	if i >= len(answers) {
		i = len(answers) - 1
	}
	out := make([]netip.Addr, 0, len(answers[i]))
	for _, s := range answers[i] {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, nil
}

func (r *scriptedResolver) asked(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// fixture is a local server standing in for a remote origin. It records the
// requests it received because "the blocked destination saw zero requests" is
// the assertion that separates a check that ran before the socket from one that
// ran after it.
type fixture struct {
	srv      *httptest.Server
	addr     string
	mu       sync.Mutex
	received []string
	sni      []string
}

func newFixture(t *testing.T, h http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{}
	f.start(t, h, nil)
	return f
}

// newTLSFixture serves h over TLS with cert, which the caller also passes to the
// policy as a root so the certificate chain actually verifies.
func newTLSFixture(t *testing.T, cert tls.Certificate, h http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{}
	f.start(t, h, &tls.Config{Certificates: []tls.Certificate{cert}})
	return f
}

func (f *fixture) start(t *testing.T, h http.HandlerFunc, tlsCfg *tls.Config) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.received = append(f.received, r.URL.RequestURI())
		if r.TLS != nil {
			f.sni = append(f.sni, r.TLS.ServerName)
		}
		f.mu.Unlock()
		h(w, r)
	}))
	if tlsCfg != nil {
		srv.TLS = tlsCfg
		srv.StartTLS()
	} else {
		srv.Start()
	}
	f.srv = srv
	f.addr = srv.Listener.Addr().String()
	t.Cleanup(srv.Close)
}

func (f *fixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func (f *fixture) serverNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sni...)
}

// routedDialer is the dial seam: it maps the numeric address the policy decided
// to connect to onto a fixture's real listener, and records what it was asked for
// so a test can prove the dial never saw a hostname (and never saw the blocked
// destination at all).
type routedDialer struct {
	mu     sync.Mutex
	routes map[string]string
	seen   []string
}

func newDialer() *routedDialer { return &routedDialer{routes: map[string]string{}} }

func (d *routedDialer) route(connectTo, fixtureAddr string) *routedDialer {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes[connectTo] = fixtureAddr
	return d
}

func (d *routedDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.seen = append(d.seen, address)
	target, ok := d.routes[address]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("routedDialer: no route for %q", address)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, target)
}

func (d *routedDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// testPolicy builds a policy from cfg wired to the resolver and dialer seams.
func testPolicy(t *testing.T, cfg Config, resolve *scriptedResolver, dial *routedDialer) *Policy {
	t.Helper()
	p, err := cfg.Policy()
	assert.NewAborting(t).NoError(err)
	if resolve != nil {
		p.resolve = resolve
	}
	if dial != nil {
		p.dial = dial.dial
	}
	return p
}

func get(client *http.Client, raw string) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, string(body), err
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// testCert mints a self-signed certificate for the given SANs (an IP SAN for an
// IP literal, a DNS SAN otherwise) plus the pool that trusts it.
func testCert(t *testing.T, sans ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assert.NewAborting(t).NoError(err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: sans[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, san)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	assert.NewAborting(t).NoError(err)
	leaf, err := x509.ParseCertificate(der)
	assert.NewAborting(t).NoError(err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// TestMcpOAuthNetworkPublicHTTPSAccepted is the default policy's happy path: a
// public name over HTTPS works, the certificate is verified against the name
// that was resolved (not the address that was dialled), and the dial itself gets
// a numeric address.
func TestMcpOAuthNetworkPublicHTTPSAccepted(t *testing.T) {
	cert, pool := testCert(t, "mcp.example")
	remote := newTLSFixture(t, cert, okHandler)
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:443", remote.addr)
	p := testPolicy(t, Config{}, resolve, dial)
	p.tlsConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	resp, body, err := get(p.Client(), "https://mcp.example/mcp")
	assert.NewAborting(t).NoError(err)
	collect := assert.NewCollecting(t)
	collect.Eq(http.StatusOK, resp.StatusCode)
	collect.Eq("ok", body)
	collect.Eq(1, remote.count(), "the fixture is reachable through the approved address")
	collect.EqDeep([]string{"mcp.example"}, remote.serverNames(), "TLS SNI names the host, not the address")
	collect.EqDeep([]string{"93.184.216.34:443"}, dial.addresses(), "the dial gets a checked numeric address")
}

// TestMcpOAuthNetworkTLSCertificateIsVerified proves the custom dialer did not
// buy address control by giving up certificate verification: the same request
// with no trusted root fails.
func TestMcpOAuthNetworkTLSCertificateIsVerified(t *testing.T) {
	cert, _ := testCert(t, "mcp.example")
	remote := newTLSFixture(t, cert, okHandler)
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:443", remote.addr)
	p := testPolicy(t, Config{}, resolve, dial)

	_, _, err := get(p.Client(), "https://mcp.example/mcp")
	collect := assert.NewCollecting(t)
	collect.Error(err, "an untrusted certificate must fail")
	collect.ErrorContains(err, "certificate")
}

// TestMcpOAuthNetworkDeniesNonPublicFamilies is the default-deny table. Every
// entry is a destination family the policy must refuse with no allow-list entry,
// and the fixture behind the seam must receive nothing at all.
func TestMcpOAuthNetworkDeniesNonPublicFamilies(t *testing.T) {
	cases := []struct{ name, ip string }{
		{"IPv4 loopback", "127.0.0.1"},
		{"IPv4 loopback (127/8)", "127.9.9.9"},
		{"IPv6 loopback", "::1"},
		{"IPv4 private 10/8", "10.0.0.5"},
		{"IPv4 private 172.16/12", "172.16.4.4"},
		{"IPv4 private 192.168/16", "192.168.1.9"},
		{"IPv6 unique-local", "fc00::1"},
		{"IPv6 unique-local (fd00)", "fd00:ec2::254"},
		{"IPv4 link-local metadata", "169.254.169.254"},
		{"IPv6 link-local", "fe80::1"},
		{"unspecified IPv4", "0.0.0.0"},
		{"unspecified IPv6", "::"},
		{"this-network 0/8", "0.1.2.3"},
		{"multicast IPv4", "224.0.0.1"},
		{"multicast IPv6", "ff02::1"},
		{"broadcast", "255.255.255.255"},
		{"reserved 240/4", "240.0.0.1"},
		{"carrier-grade NAT", "100.64.0.1"},
		{"cloud metadata in CGNAT", "100.100.100.200"},
		{"benchmarking", "198.18.0.1"},
		{"documentation IPv4", "192.0.2.1"},
		{"documentation IPv6", "2001:db8::1"},
		{"6to4", "2002::1"},
		{"NAT64", "64:ff9b::1.2.3.4"},
		{"Teredo", "2001::1"},
		{"IPv4-mapped loopback", "::ffff:127.0.0.1"},
		{"IPv4-mapped private", "::ffff:10.0.0.1"},
		{"IPv4-mapped link-local", "::ffff:169.254.169.254"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remote := newFixture(t, okHandler)
			resolve := newResolver().answer("deny.example", c.ip)
			dial := newDialer().route(c.ip+":443", remote.addr)
			p := testPolicy(t, Config{}, resolve, dial)

			_, _, err := get(p.Client(), "https://deny.example/mcp")
			collect := assert.NewCollecting(t)
			collect.ErrorIs(err, ErrDestinationDenied, c.ip)
			collect.Eq(0, remote.count(), "a refused destination receives no request")
		})
	}
}

// TestMcpOAuthNetworkDeniesZonedAddresses is the zone rule: an address carrying a
// local interface scope names no remote destination, so it is refused — never
// classified public and never dialled. The zone is the whole point: netip.
// Prefix.Contains is false for ANY zoned address, so without this every one of
// these walks past the reserved families below, and a zoned 6to4 or NAT64
// address embeds an arbitrary IPv4 address (10/8, 169.254.169.254) that the
// address check cannot see. A zoned GLOBAL address is refused too: stripping the
// zone instead of refusing it would be the weaker of the two fixes.
func TestMcpOAuthNetworkDeniesZonedAddresses(t *testing.T) {
	cases := []struct{ name, addr string }{
		{"zoned 6to4", "2002::1%eth0"},
		{"zoned NAT64", "64:ff9b::1.2.3.4%eth0"},
		{"zoned Teredo", "2001::1%eth0"},
		{"zoned documentation", "2001:db8::1%eth0"},
		{"zoned link-local", "fe80::1%eth0"},
		{"zoned unique-local", "fd00::1%eth0"},
		{"zoned global unicast", "2606:4700:4700::1111%eth0"},
	}
	for _, c := range cases {
		t.Run(c.name+" literal", func(t *testing.T) {
			remote := newFixture(t, okHandler)
			dial := newDialer().route(c.addr+":443", remote.addr)
			p := testPolicy(t, Config{}, newResolver(), dial)

			// The zone's '%' is written %25 inside a URL authority.
			literal := strings.ReplaceAll(c.addr, "%", "%25")
			_, _, err := get(p.Client(), "https://["+literal+"]/mcp")
			collect := assert.NewCollecting(t)
			collect.ErrorIs(err, ErrInvalidURL, "a zoned literal names no remote destination")
			collect.ErrorContains(err, "zoned")
			collect.Eq(0, remote.count(), "a zoned destination receives no request")
			collect.Eq(0, len(dial.addresses()), "a zoned address is never dialled")
		})

		t.Run(c.name+" resolver answer", func(t *testing.T) {
			remote := newFixture(t, okHandler)
			resolve := newResolver().answer("zoned.example", c.addr)
			dial := newDialer().route(c.addr+":443", remote.addr)
			p := testPolicy(t, Config{}, resolve, dial)

			_, _, err := get(p.Client(), "https://zoned.example/mcp")
			collect := assert.NewCollecting(t)
			collect.ErrorIs(err, ErrDestinationDenied, "an answer carrying a zone is refused even for a public address")
			collect.Eq(0, remote.count())
			collect.Eq(0, len(dial.addresses()))
		})
	}
}

// TestMcpOAuthNetworkNumericPrivateLiteralDenied covers the literal path: an IP
// in the URL is refused without being handed to the resolver, which would only
// be a chance for something else to answer for it.
func TestMcpOAuthNetworkNumericPrivateLiteralDenied(t *testing.T) {
	remote := newFixture(t, okHandler)
	resolve := newResolver()
	dial := newDialer().route("10.0.0.5:443", remote.addr)
	p := testPolicy(t, Config{}, resolve, dial)

	_, _, err := get(p.Client(), "https://10.0.0.5/mcp")
	collect := assert.NewCollecting(t)
	collect.ErrorIs(err, ErrDestinationDenied)
	collect.Eq(0, resolve.asked("10.0.0.5"), "an IP literal is not resolved")
	collect.Eq(0, remote.count())
}

// TestMcpOAuthNetworkMixedAnswerDenied refuses a name that answers both a
// public and a private address: which one a connect would pick is not ours to
// guess, so neither is used.
func TestMcpOAuthNetworkMixedAnswerDenied(t *testing.T) {
	remote := newFixture(t, okHandler)
	resolve := newResolver().answer("mixed.example", "93.184.216.34", "127.0.0.1")
	dial := newDialer().
		route("93.184.216.34:443", remote.addr).
		route("127.0.0.1:443", remote.addr)
	p := testPolicy(t, Config{}, resolve, dial)

	_, _, err := get(p.Client(), "https://mixed.example/mcp")
	collect := assert.NewCollecting(t)
	collect.ErrorIs(err, ErrDestinationDenied)
	collect.Eq(0, remote.count(), "a mixed answer opens no connection at all")
}

// TestMcpOAuthNetworkRebindingDenied is the DNS-rebinding case: the pre-flight
// lookup answers a public address and passes, then the dial's own lookup answers
// loopback. The dial refuses, and the fixture the rebound address would have
// reached sees nothing.
func TestMcpOAuthNetworkRebindingDenied(t *testing.T) {
	blocked := newFixture(t, okHandler)
	resolve := newResolver().
		answer("rebind.example", "93.184.216.34").
		answer("rebind.example", "127.0.0.1")
	dial := newDialer().
		route("93.184.216.34:443", blocked.addr).
		route("127.0.0.1:443", blocked.addr)
	p := testPolicy(t, Config{}, resolve, dial)

	err := p.ValidateDestination(context.Background(), "https://rebind.example/mcp")
	assert.NewAborting(t).NoError(err, "the first answer is public")

	_, _, err = get(p.Client(), "https://rebind.example/mcp")
	collect := assert.NewCollecting(t)
	collect.ErrorIs(err, ErrDestinationDenied, "the second answer must be refused at dial time")
	collect.Eq(0, blocked.count(), "the rebound destination receives no request")
	collect.Eq(2, resolve.asked("rebind.example"), "the dial resolves again instead of trusting the pre-check")
}

// TestMcpOAuthNetworkDialsOnlyTheApprovedAddress pins the "one checked numeric
// address, no second lookup" rule: of two public addresses the resolver answered,
// exactly the first checked one is dialled, and the dial never sees a hostname.
func TestMcpOAuthNetworkDialsOnlyTheApprovedAddress(t *testing.T) {
	cert, pool := testCert(t, "mcp.example")
	remote := newTLSFixture(t, cert, okHandler)
	resolve := newResolver().answer("mcp.example", "93.184.216.34", "8.8.8.8")
	dial := newDialer().
		route("93.184.216.34:443", remote.addr).
		route("8.8.8.8:443", remote.addr)
	p := testPolicy(t, Config{}, resolve, dial)
	p.tlsConfig = &tls.Config{RootCAs: pool}

	_, _, err := get(p.Client(), "https://mcp.example/mcp")
	collect := assert.NewCollecting(t)
	collect.NoError(err)
	collect.EqDeep([]string{"93.184.216.34:443"}, dial.addresses())
	collect.Eq(1, resolve.asked("mcp.example"), "one dial resolves once")
}

// TestMcpOAuthNetworkMetadataRedirectRefused follows a redirect towards cloud
// instance metadata, the classic SSRF win: the hop is cross-origin, so it is
// refused before any socket, and the metadata fixture sees zero requests.
func TestMcpOAuthNetworkMetadataRedirectRefused(t *testing.T) {
	metadata := newFixture(t, okHandler)
	origin := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/", http.StatusFound)
	})
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().
		route("93.184.216.34:80", origin.addr).
		route("169.254.169.254:80", metadata.addr)
	p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)

	_, _, err := get(p.Client(), "http://mcp.example/token")
	collect := assert.NewCollecting(t)
	collect.ErrorIs(err, ErrRedirectRefused)
	collect.ErrorContains(err, "cross-origin")
	collect.Eq(0, metadata.count(), "the metadata endpoint receives no request")
	for _, addr := range dial.addresses() {
		collect.NotEq("169.254.169.254:80", addr, "the metadata address was never dialled")
	}
}

// TestMcpOAuthNetworkCrossOriginCredentialRedirectRefused is the token-request
// version: a second origin that would answer 200 must not be followed, because
// the request carries a client secret or bearer token. A same-origin redirect is
// still followed.
func TestMcpOAuthNetworkCrossOriginCredentialRedirectRefused(t *testing.T) {
	other := newFixture(t, okHandler)
	t.Run("cross-origin", func(t *testing.T) {
		origin := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://other.example/token", http.StatusTemporaryRedirect)
		})
		resolve := newResolver().answer("mcp.example", "93.184.216.34")
		dial := newDialer().
			route("93.184.216.34:80", origin.addr).
			route("1.1.1.1:80", other.addr)
		p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example", "other.example"}}, resolve, dial)

		_, _, err := get(p.Client(), "http://mcp.example/token")
		collect := assert.NewCollecting(t)
		collect.ErrorIs(err, ErrRedirectRefused)
		collect.Eq(0, other.count(), "the credential must not reach the second origin")
	})

	t.Run("same-origin", func(t *testing.T) {
		origin := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				http.Redirect(w, r, "/token/v2", http.StatusTemporaryRedirect)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "followed")
		})
		resolve := newResolver().answer("mcp.example", "93.184.216.34")
		dial := newDialer().route("93.184.216.34:80", origin.addr)
		p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)

		resp, body, err := get(p.Client(), "http://mcp.example/token")
		collect := assert.NewCollecting(t)
		collect.NoError(err)
		collect.Eq(http.StatusOK, resp.StatusCode)
		collect.Eq("followed", body)
		collect.Eq(2, origin.count())
	})
}

// TestMcpOAuthNetworkHTTPSDowngradeRefused refuses an HTTPS→HTTP redirect on the
// same host even when that host is explicitly allowed cleartext: downgrading is
// not a policy decision an allow_http_hosts entry can make.
func TestMcpOAuthNetworkHTTPSDowngradeRefused(t *testing.T) {
	cleartext := newFixture(t, okHandler)
	cert, pool := testCert(t, "secure.example")
	origin := newTLSFixture(t, cert, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://secure.example/plain", http.StatusFound)
	})
	resolve := newResolver().answer("secure.example", "93.184.216.34")
	dial := newDialer().
		route("93.184.216.34:443", origin.addr).
		route("93.184.216.34:80", cleartext.addr)
	p := testPolicy(t, Config{
		AllowHosts:     []string{"secure.example"},
		AllowHTTPHosts: []string{"secure.example"},
	}, resolve, dial)
	p.tlsConfig = &tls.Config{RootCAs: pool}

	_, _, err := get(p.Client(), "https://secure.example/token")
	collect := assert.NewCollecting(t)
	collect.ErrorIs(err, ErrRedirectRefused)
	collect.ErrorContains(err, "downgrade")
	collect.Eq(0, cleartext.count(), "the downgraded target receives no request")
}

// TestMcpOAuthNetworkProxyEnvCannotBypass proves the transport ignores the
// environment: with every proxy variable pointing at a fixture, the request
// still goes to the approved destination and the proxy sees nothing. A proxy
// resolves the name itself, so honouring one would hand the address decision to
// whoever runs it.
func TestMcpOAuthNetworkProxyEnvCannotBypass(t *testing.T) {
	proxy := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("proxy received %s", r.URL)
		http.Error(w, "must not be used", http.StatusBadGateway)
	})
	t.Setenv("HTTP_PROXY", proxy.srv.URL)
	t.Setenv("HTTPS_PROXY", proxy.srv.URL)
	t.Setenv("ALL_PROXY", proxy.srv.URL)
	t.Setenv("NO_PROXY", "")

	cert, pool := testCert(t, "mcp.example")
	remote := newTLSFixture(t, cert, okHandler)
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:443", remote.addr)
	p := testPolicy(t, Config{}, resolve, dial)
	p.tlsConfig = &tls.Config{RootCAs: pool}

	resp, body, err := get(p.Client(), "https://mcp.example/mcp")
	collect := assert.NewCollecting(t)
	collect.NoError(err)
	collect.Eq(http.StatusOK, resp.StatusCode)
	collect.Eq("ok", body)
	collect.Eq(0, proxy.count(), "the proxy is never consulted")
	collect.Eq(1, remote.count())
}

// TestMcpOAuthNetworkApprovedPrivateHTTPSWorks covers the two opt-ins that make
// a private destination reachable: an exact host, and a CIDR.
func TestMcpOAuthNetworkApprovedPrivateHTTPSWorks(t *testing.T) {
	t.Run("allow_hosts", func(t *testing.T) {
		cert, pool := testCert(t, "internal.example")
		remote := newTLSFixture(t, cert, okHandler)
		resolve := newResolver().answer("internal.example", "10.1.2.3")
		dial := newDialer().route("10.1.2.3:443", remote.addr)
		p := testPolicy(t, Config{AllowHosts: []string{"internal.example"}}, resolve, dial)
		p.tlsConfig = &tls.Config{RootCAs: pool}

		_, _, err := get(p.Client(), "https://internal.example/mcp")
		collect := assert.NewCollecting(t)
		collect.NoError(err)
		collect.Eq(1, remote.count())
	})

	t.Run("allow_cidrs", func(t *testing.T) {
		cert, pool := testCert(t, "10.1.2.3")
		remote := newTLSFixture(t, cert, okHandler)
		dial := newDialer().route("10.1.2.3:443", remote.addr)
		p := testPolicy(t, Config{AllowCIDRs: []string{"10.1.0.0/16"}}, newResolver(), dial)
		p.tlsConfig = &tls.Config{RootCAs: pool}

		_, _, err := get(p.Client(), "https://10.1.2.3/mcp")
		collect := assert.NewCollecting(t)
		collect.NoError(err)
		collect.EqDeep([]string{"10.1.2.3:443"}, dial.addresses())
	})

	t.Run("an allowed host is not dodged by a mapped spelling", func(t *testing.T) {
		cert, pool := testCert(t, "10.0.0.7")
		remote := newTLSFixture(t, cert, okHandler)
		dial := newDialer().route("10.0.0.7:443", remote.addr)
		p := testPolicy(t, Config{AllowHosts: []string{"10.0.0.7"}}, newResolver(), dial)
		p.tlsConfig = &tls.Config{RootCAs: pool}

		_, _, err := get(p.Client(), "https://[::ffff:10.0.0.7]/mcp")
		assert.NewCollecting(t).NoError(err)
	})
}

// TestMcpOAuthNetworkHTTPNeedsItsOwnAllowance pins the two independent gates:
// cleartext needs its exact host listed, and that listing does not make a
// private address reachable.
func TestMcpOAuthNetworkHTTPNeedsItsOwnAllowance(t *testing.T) {
	t.Run("cleartext without its own entry is refused", func(t *testing.T) {
		remote := newFixture(t, okHandler)
		resolve := newResolver().answer("internal.example", "10.0.0.7")
		dial := newDialer().route("10.0.0.7:80", remote.addr)
		p := testPolicy(t, Config{AllowHosts: []string{"internal.example"}}, resolve, dial)

		_, _, err := get(p.Client(), "http://internal.example/mcp")
		collect := assert.NewCollecting(t)
		collect.ErrorIs(err, ErrDestinationDenied)
		collect.ErrorContains(err, "allow_http_hosts")
		collect.Eq(0, remote.count())
	})

	t.Run("cleartext with its own entry is allowed", func(t *testing.T) {
		remote := newFixture(t, okHandler)
		resolve := newResolver().answer("internal.example", "10.0.0.7")
		dial := newDialer().route("10.0.0.7:80", remote.addr)
		p := testPolicy(t, Config{
			AllowHosts:     []string{"internal.example"},
			AllowHTTPHosts: []string{"internal.example"},
		}, resolve, dial)

		_, _, err := get(p.Client(), "http://internal.example/mcp")
		collect := assert.NewCollecting(t)
		collect.NoError(err)
		collect.Eq(1, remote.count())
	})

	t.Run("a cleartext entry does not permit a private address", func(t *testing.T) {
		remote := newFixture(t, okHandler)
		resolve := newResolver().answer("internal.example", "10.0.0.7")
		dial := newDialer().route("10.0.0.7:80", remote.addr)
		p := testPolicy(t, Config{AllowHTTPHosts: []string{"internal.example"}}, resolve, dial)

		_, _, err := get(p.Client(), "http://internal.example/mcp")
		collect := assert.NewCollecting(t)
		collect.ErrorIs(err, ErrDestinationDenied)
		collect.ErrorContains(err, "not public")
		collect.Eq(0, remote.count(), "an HTTP allowance is not an address allowance")
	})

	t.Run("https needs no entry", func(t *testing.T) {
		cert, pool := testCert(t, "internal.example")
		remote := newTLSFixture(t, cert, okHandler)
		resolve := newResolver().answer("internal.example", "10.0.0.7")
		dial := newDialer().route("10.0.0.7:443", remote.addr)
		p := testPolicy(t, Config{AllowHosts: []string{"internal.example"}}, resolve, dial)
		p.tlsConfig = &tls.Config{RootCAs: pool}

		_, _, err := get(p.Client(), "https://internal.example/mcp")
		assert.NewCollecting(t).NoError(err)
	})
}

// TestMcpOAuthNetworkBoundsResponseBody pins the body cap, which both clients
// carry: exactly the cap is a success, one byte more is an explicit error rather
// than a body that decodes into nothing useful, and the only exemption is an
// event stream, which has no natural end.
func TestMcpOAuthNetworkBoundsResponseBody(t *testing.T) {
	serve := func(n int, contentType string) *fixture {
		body := strings.Repeat("x", n)
		return newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = io.WriteString(w, body)
		})
	}
	build := func(remote *fixture, stream bool) *http.Client {
		resolve := newResolver().answer("mcp.example", "93.184.216.34")
		dial := newDialer().route("93.184.216.34:80", remote.addr)
		p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)
		if stream {
			return p.StreamClient()
		}
		return p.Client()
	}

	t.Run("exactly the cap succeeds", func(t *testing.T) {
		remote := serve(MaxResponseBytes, "application/json")
		_, body, err := get(build(remote, false), "http://mcp.example/mcp")
		collect := assert.NewCollecting(t)
		collect.NoError(err)
		collect.Eq(MaxResponseBytes, len(body))
	})

	t.Run("one byte past the cap is an error", func(t *testing.T) {
		remote := serve(MaxResponseBytes+1, "application/json")
		_, _, err := get(build(remote, false), "http://mcp.example/mcp")
		assert.NewCollecting(t).ErrorIs(err, ErrResponseTooLarge)
	})

	t.Run("a JSON reply on the stream client is capped too", func(t *testing.T) {
		// The SDK reads ordinary JSON POST replies through this client, so an
		// uncapped body here is a hostile server's way to grow the daemon's heap.
		remote := serve(MaxResponseBytes+1, "application/json")
		_, _, err := get(build(remote, true), "http://mcp.example/mcp")
		collect := assert.NewCollecting(t)
		collect.ErrorIs(err, ErrResponseTooLarge)
		collect.ErrorContains(err, "response body exceeds")
	})

	t.Run("a JSON reply of exactly the cap on the stream client succeeds", func(t *testing.T) {
		remote := serve(MaxResponseBytes, "application/json")
		_, body, err := get(build(remote, true), "http://mcp.example/mcp")
		collect := assert.NewCollecting(t)
		collect.NoError(err)
		collect.Eq(MaxResponseBytes, len(body))
	})

	t.Run("an event stream is not capped", func(t *testing.T) {
		for _, contentType := range []string{"text/event-stream", "text/event-stream; charset=utf-8"} {
			remote := serve(MaxResponseBytes+1, contentType)
			_, body, err := get(build(remote, true), "http://mcp.example/mcp")
			collect := assert.NewCollecting(t)
			collect.NoError(err, contentType)
			collect.Eq(MaxResponseBytes+1, len(body), "an event stream has no natural end: %s", contentType)
		}
	})
}

// TestMcpOAuthNetworkStreamsUncappedAndUndeadlined is the streaming client's
// other half: an event stream is exempt from BOTH bounds. It runs past
// MaxResponseBytes and takes longer than the operation timeout the non-streaming
// client is held to, and the whole body still arrives — so no whole-session
// deadline may be added to StreamClient.
func TestMcpOAuthNetworkStreamsUncappedAndUndeadlined(t *testing.T) {
	const chunk = 64 << 10
	chunks := MaxResponseBytes/chunk + 3
	line := []byte("data: " + strings.Repeat("x", chunk-8) + "\n\n")
	remote := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(line); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:80", remote.addr)
	p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)
	p.opTimeout = 100 * time.Millisecond

	start := time.Now()
	_, body, err := get(p.StreamClient(), "http://mcp.example/mcp")
	elapsed := time.Since(start)
	collect := assert.NewCollecting(t)
	collect.NoError(err)
	collect.Eq(len(line)*chunks, len(body), "an event stream is not capped")
	collect.True(len(body) > MaxResponseBytes, "the stream exceeded the cap and still flowed")
	collect.True(elapsed > p.opTimeout, "the stream outlived the operation timeout (%s)", elapsed)
}

// TestMcpOAuthNetworkBoundsOperation bounds one whole operation, body read
// included: a server that answers headers and then stalls is still cut off. The
// bound is the client's Timeout, so the error is a timeout of some spelling
// rather than one particular one.
func TestMcpOAuthNetworkBoundsOperation(t *testing.T) {
	remote := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "late")
	})
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:80", remote.addr)
	p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)
	p.opTimeout = 100 * time.Millisecond

	start := time.Now()
	_, _, err := get(p.Client(), "http://mcp.example/mcp")
	var netErr net.Error
	collect := assert.NewCollecting(t)
	collect.Error(err, "the stalled body must be cut off")
	collect.True(errors.As(err, &netErr) && netErr.Timeout(), "want a timeout, got %v", err)
	collect.True(time.Since(start) < 3*time.Second, "the bound is not the fixture's own delay")
}

// TestMcpOAuthNetworkBoundsTheWholeRedirectChain is the whole-chain rule: the
// operation timeout covers ONE operation including every redirect, not each hop
// separately. Every hop here is well inside the bound and the chain as a whole is
// well past it, so a per-hop bound would let this request finish and a whole-chain
// bound refuses it.
func TestMcpOAuthNetworkBoundsTheWholeRedirectChain(t *testing.T) {
	const (
		hops  = 5
		delay = 80 * time.Millisecond
	)
	remote := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.URL.Path, "/%d", &n)
		time.Sleep(delay)
		if n < hops {
			http.Redirect(w, r, fmt.Sprintf("/%d", n+1), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "end")
	})
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:80", remote.addr)
	p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)
	p.opTimeout = 150 * time.Millisecond

	start := time.Now()
	_, _, err := get(p.Client(), "http://mcp.example/0")
	elapsed := time.Since(start)
	var netErr net.Error
	collect := assert.NewCollecting(t)
	collect.Error(err, "the chain as a whole runs past the operation bound")
	collect.True(errors.As(err, &netErr) && netErr.Timeout(), "want a timeout, got %v", err)
	collect.True(elapsed < hops*delay, "refused by the whole-chain bound (%s), not after the last hop", elapsed)
	collect.True(remote.count() > 1, "the chain really was followed before the bound fired")
}

// TestMcpOAuthNetworkBoundsInitialResponse bounds the wait for headers on both
// clients, which is what keeps a streaming session from hanging before its first
// byte without imposing a whole-session deadline on it.
func TestMcpOAuthNetworkBoundsInitialResponse(t *testing.T) {
	remote := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "late")
	})
	resolve := newResolver().answer("mcp.example", "93.184.216.34")
	dial := newDialer().route("93.184.216.34:80", remote.addr)
	p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)
	p.headerTimeout = 100 * time.Millisecond

	for name, client := range map[string]*http.Client{"client": p.Client(), "stream": p.StreamClient()} {
		t.Run(name, func(t *testing.T) {
			_, _, err := get(client, "http://mcp.example/mcp")
			var netErr net.Error
			collect := assert.NewCollecting(t)
			collect.Error(err)
			collect.True(errors.As(err, &netErr) && netErr.Timeout(), "want a timeout, got %v", err)
		})
	}
}

// TestMcpOAuthNetworkBoundsRedirects pins the hop count: five are followed, six
// are refused.
func TestMcpOAuthNetworkBoundsRedirects(t *testing.T) {
	chain := func(hops int) *fixture {
		return newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/%d", &n)
			if n < hops {
				http.Redirect(w, r, fmt.Sprintf("/%d", n+1), http.StatusFound)
				return
			}
			_, _ = io.WriteString(w, "end")
		})
	}
	for _, c := range []struct {
		hops    int
		wantErr bool
	}{
		{hops: MaxRedirects, wantErr: false},
		{hops: MaxRedirects + 1, wantErr: true},
	} {
		name := fmt.Sprintf("%d hops", c.hops)
		t.Run(name, func(t *testing.T) {
			remote := chain(c.hops)
			resolve := newResolver().answer("mcp.example", "93.184.216.34")
			dial := newDialer().route("93.184.216.34:80", remote.addr)
			p := testPolicy(t, Config{AllowHTTPHosts: []string{"mcp.example"}}, resolve, dial)

			resp, body, err := get(p.Client(), "http://mcp.example/0")
			collect := assert.NewCollecting(t)
			if c.wantErr {
				collect.ErrorIs(err, ErrRedirectRefused)
				collect.ErrorContains(err, "redirects")
				return
			}
			collect.NoError(err)
			collect.Eq(http.StatusOK, resp.StatusCode)
			collect.Eq("end", body)
		})
	}
}

// TestMcpOAuthNetworkValidatesDerivedAuthorizeURL is the check for a URL that
// came out of remote metadata: an authorization endpoint has to pass here before
// it is handed back to a browser, DNS included.
func TestMcpOAuthNetworkValidatesDerivedAuthorizeURL(t *testing.T) {
	ctx := context.Background()
	resolve := newResolver().
		answer("as.example", "93.184.216.34").
		answer("private.example", "10.0.0.7")
	p := testPolicy(t, Config{AllowHTTPHosts: []string{"as.example"}}, resolve, nil)

	cases := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{name: "public https", raw: "https://as.example/authorize?client_id=x&resource=y"},
		{name: "private https", raw: "https://private.example/authorize", wantErr: ErrDestinationDenied},
		{name: "private literal", raw: "https://10.0.0.7/token", wantErr: ErrDestinationDenied},
		{name: "cleartext not allowed", raw: "http://example.com/authorize", wantErr: ErrDestinationDenied},
		{name: "unresolvable", raw: "https://unknown.example/authorize", wantErr: ErrDestinationDenied},
		{name: "not https/http", raw: "ftp://as.example/authorize", wantErr: ErrInvalidURL},
		{name: "fragment", raw: "https://as.example/authorize#x", wantErr: ErrInvalidURL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := p.ValidateDestination(ctx, c.raw)
			collect := assert.NewCollecting(t)
			if c.wantErr == nil {
				collect.NoError(err)
				return
			}
			collect.ErrorIs(err, c.wantErr, c.raw)
		})
	}
}

// TestMcpOAuthNetworkNilPolicyIsDefaultDeny pins the safe degradation: an
// unwired policy denies what the default denies instead of panicking or
// allowing.
func TestMcpOAuthNetworkNilPolicyIsDefaultDeny(t *testing.T) {
	var p *Policy
	private, err := url.Parse("https://10.0.0.7/mcp")
	assert.NewAborting(t).NoError(err)
	public, err := url.Parse("https://93.184.216.34/mcp")
	assert.NewAborting(t).NoError(err)

	collect := assert.NewCollecting(t)
	collect.ErrorIs(p.ValidateURL(context.Background(), private), ErrDestinationDenied)
	collect.NoError(p.ValidateURL(context.Background(), public))
	collect.NotEq(nil, p.Client())
	collect.NotEq(nil, p.StreamClient())
}
