package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	return &webfetchTool{
		WebfetchBlueprint: WebfetchBlueprint{},
		p:                 opts.OutputPolicy,
		client:            opts.HTTPClient,
	}, nil
}

type webfetchTool struct {
	WebfetchBlueprint
	p      OutputPolicy
	client *http.Client
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

	// When a custom client is provided (tests), trust the caller and skip SSRF.
	// Otherwise, resolve and check every IP.
	if t.client == nil {
		// SSRF: resolve and check the IP.
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
		if err != nil {
			return NewErrorResult(fmt.Errorf("webfetch: dns lookup failed for %s: %w", parsed.Hostname(), err)), nil
		}
		for _, addr := range addrs {
			if isBlockedIP(addr.IP) {
				return NewTextResult(fmt.Sprintf("webfetch: url %q resolves to a blocked IP range (%s); request blocked", in.URL, addr.IP)), nil
			}
		}
	}

	client := t.client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
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

// isBlockedIP reports whether ip is in a range that should not be fetched.
// Blocks: loopback, link-local (169.254.0.0/16), RFC1918 private, and
// unique local addresses (fc00::/7).
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	// IsPrivate does not cover fc00::/7 (unique local).
	if ip.To4() == nil {
		// IPv6 unique local: fc00::/7
		if len(ip) == net.IPv6len && ip[0] >= 0xfc && ip[0] <= 0xfd {
			return true
		}
	}
	return false
}
