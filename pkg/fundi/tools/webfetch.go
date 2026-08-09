package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
)

func init() { DefaultBlueprint.Register(&WebfetchBlueprint{}) }

const webfetchUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// webfetchCapBytes is the maximum body size webfetch reads.
const webfetchCapBytes = 100 * 1024

const webfetchDescription = "Fetch a URL and return its content as text. " +
	"Only http and https schemes are accepted. " +
	"The result is capped at 100 KB; use grep/read tools on the spill file for larger pages."

// WebfetchBlueprint is the static metadata for the webfetch tool.
// It implements Materializer because the tool needs runtime state (egress gate).
type WebfetchBlueprint struct{}

func (WebfetchBlueprint) Name() string        { return "webfetch" }
func (WebfetchBlueprint) Description() string { return webfetchDescription }
func (WebfetchBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "url", Type: "string", Description: "URL to fetch. Only http and https schemes are accepted."},
			{Name: "format", Type: "string", Description: "Output format: text (default), markdown, or html"},
		},
		Required: []string{"url"},
	}
}
func (WebfetchBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (WebfetchBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if !opts.Web {
		return nil, nil
	}
	t := &webfetchTool{
		WebfetchBlueprint: WebfetchBlueprint{},
		p:                 opts.OutputPolicy,
		blocked:           isBlockedIP,
	}
	t.client = opts.HTTPClient
	if t.client == nil {
		t.client = newGuardedClient(t.guard, webfetchTimeout)
	}
	return t, nil
}

type webfetchTool struct {
	WebfetchBlueprint
	p      OutputPolicy
	client *http.Client
	// blocked decides which resolved IPs are refused at connect time. It is
	// a field so a test can point the real transport at an httptest server
	// on loopback while still exercising the production dial path;
	// production always gets isBlockedIP.
	blocked func(net.IP) bool
}

// guard indirects through the field so a test can swap the predicate after
// Materialize has already built the client.
func (t *webfetchTool) guard(ip net.IP) bool { return t.blocked(ip) }

const (
	webfetchTimeout      = 30 * time.Second
	webfetchMaxRedirects = 5
)

// newGuardedClient builds webfetch's HTTP client with the address check in
// the dialer's Control hook.
//
// Doing it here rather than as a pre-flight LookupIPAddr is what makes the
// check actually hold. A pre-flight lookup is defeated two ways: the
// transport re-resolves the name independently, so a DNS record with a
// short TTL can answer "1.2.3.4" to the check and "169.254.169.254" to the
// dial (rebinding); and Go follows redirects by default, so only the first
// hop was ever validated — one "302 Location: http://169.254.169.254/…"
// reached cloud instance metadata and put the credentials it returned into
// the model's context. Control runs after resolution, on every connection,
// including every redirect hop, so both holes close by construction rather
// than by bookkeeping.
func newGuardedClient(blocked func(net.IP) bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("unparsable dial address %q", address)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("unresolvable dial address %q", host)
		}
		if blocked(ip) {
			return fmt.Errorf("address %s is in a blocked range", ip)
		}
		return nil
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext, ForceAttemptHTTP2: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// The Control hook covers the address on every hop; a scheme is
			// not an address, so it is re-checked here. Without this a
			// redirect could leave http(s) behind entirely.
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to %q scheme", req.URL.Scheme)
			}
			if len(via) >= webfetchMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", webfetchMaxRedirects)
			}
			return nil
		},
	}
}

type webfetchInput struct {
	URL    string `json:"url"`
	Format string `json:"format"`
}

func (t *webfetchTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in webfetchInput
	if err := input.Unmarshal(&in); err != nil {
		return NewErrorResult(fmt.Errorf("webfetch: invalid input: %w", err)), nil
	}
	if in.URL == "" {
		return NewTextResult("webfetch: url is required"), nil
	}

	parsed, err := url.Parse(in.URL)
	if err != nil {
		return NewTextResult(fmt.Sprintf("webfetch: invalid url: %s", err)), nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return NewTextResult("webfetch: only http and https schemes are allowed"), nil
	}

	// No pre-flight LookupIPAddr: the address check lives in the client's
	// dial Control hook (see newGuardedClient), which is the only place it
	// can survive DNS rebinding and redirects.
	client := t.client
	req, err := http.NewRequestWithContext(ctx, "GET", in.URL, nil)
	if err != nil {
		return NewErrorResult(fmt.Errorf("webfetch: request: %w", err)), nil
	}
	req.Header.Set("User-Agent", webfetchUserAgent)
	req.Header.Set("Accept", "text/html,text/plain,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return NewErrorResult(fmt.Errorf("webfetch: %w", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return NewTextResult(fmt.Sprintf("webfetch: server returned %d", resp.StatusCode)), nil
	}

	// Cap while reading, not after.
	limited := io.LimitReader(resp.Body, webfetchCapBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return NewErrorResult(fmt.Errorf("webfetch: read body: %w", err)), nil
	}

	content := string(body)
	contentType := resp.Header.Get("Content-Type")

	format := strings.ToLower(in.Format)
	if format == "" {
		format = "text"
	}

	switch format {
	case "html":
		// Return raw HTML as-is.
	case "markdown":
		if strings.Contains(contentType, "text/html") {
			content = htmlToText(content)
		}
	case "text":
		if strings.Contains(contentType, "text/html") {
			content = htmlToText(content)
		}
	}

	content = t.p.ClipBudget(content, "webfetch", Budget{MaxBytes: webfetchCapBytes})
	return NewTextResult(content), nil
}

// htmlToText extracts plain text from HTML using golang.org/x/net/html.
// This is a minimal tag-stripper — no new dependency needed.
func htmlToText(htmlContent string) string {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return htmlContent // fallback: return raw
	}
	var sb strings.Builder
	var extract func(*html.Node)
	extract = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				switch c.Data {
				case "script", "style", "noscript", "iframe", "svg":
					continue // skip noisy elements
				case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
					sb.WriteString("\n")
				}
			}
			extract(c)
		}
	}
	extract(doc)

	// Collapse whitespace.
	lines := strings.Split(sb.String(), "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n")
}

// cgnatNet is RFC 6598 carrier-grade NAT space. It is called out
// explicitly because Tailscale allocates from it, and this repo is
// tailnet-aware: without this, webfetch reaches every other host on the
// operator's tailnet — internal dashboards, admin UIs, other rafiki nodes.
var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isBlockedIP reports whether ip is in a range that should not be fetched.
//
// IPv4-mapped IPv6 (::ffff:127.0.0.1, a classic bypass) needs no special
// case: net.IP's IsLoopback/IsPrivate call To4() first, so the mapped form
// is classified as the IPv4 address it encodes.
func isBlockedIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip.IsPrivate():
		return true
	// "0.0.0.0" and "::" are not loopback, not link-local and not private,
	// so they used to sail through — but connect() to the unspecified
	// address is remapped to loopback, which put the daemon's own control
	// plane (RAFIKI_CONTROL_LISTEN) one fetch away.
	case ip.IsUnspecified():
		return true
	case cgnatNet.Contains(ip):
		return true
	}
	// 0.0.0.0/8 ("this network") beyond the unspecified address itself.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return true
	}
	// IsPrivate covers fc00::/7 for IPv6 already (it tests ip[0]&0xfe==0xfc
	// on the 16-byte form), so no separate unique-local branch is needed.
	return false
}
