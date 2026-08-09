package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebfetchFetchesHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte("hello from http")); err != nil {
			panic(err)
		}
	}))
	defer srv.Close()

	opts := ToolOpts{Web: true, HTTPClient: srv.Client()}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool == nil {
		t.Fatal("Materialize returned nil with Web=true")
	}

	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{
		"url": srv.URL,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "hello from http") {
		t.Fatalf("want hello from http, got: %q", result.Text)
	}
}

func TestWebfetchRejectsNonHTTPScheme(t *testing.T) {
	opts := ToolOpts{Web: true}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	for _, scheme := range []string{"ftp://example.com", "file:///etc/passwd", "gopher://example.com"} {
		result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"url": scheme}))
		if err != nil {
			t.Errorf("Execute(%q) returned error: %v", scheme, err)
			continue
		}
		if !strings.Contains(strings.ToLower(result.Text), "only http") {
			t.Errorf("want 'only http/https' rejection for %q, got: %q", scheme, result.Text)
		}
	}
}

func TestWebfetchCapsBodyAt100KB(t *testing.T) {
	// Serve 200 KB of text — the tool must stop reading at 100 KB.
	payload := strings.Repeat("x", 200*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if _, err := w.Write([]byte(payload)); err != nil {
			panic(err)
		}
	}))
	defer srv.Close()

	opts := ToolOpts{Web: true, HTTPClient: srv.Client()}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"url": srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Text) > 110*1024 { // allow some overhead from ClipBudget envelope
		t.Fatalf("body too long: got %d bytes, cap is 100 KB", len(result.Text))
	}
	if !strings.Contains(result.Text, "x") {
		t.Fatalf("body does not contain expected content: %q", result.Text[:min(len(result.Text), 200)])
	}
}

func TestWebfetchBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("secret")); err != nil {
			panic(err)
		}
	}))
	defer srv.Close()

	opts := ToolOpts{Web: true}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// httptest.NewServer binds to a loopback address.
	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"url": srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "blocked") {
		t.Fatalf("loopback was not blocked, got: %q", result.Text)
	}
}

func TestWebfetchBlocksPrivateRanges(t *testing.T) {
	opts := ToolOpts{Web: true}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	privateURLs := []string{
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"https://[fc00::1]/",
	}
	for _, u := range privateURLs {
		result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"url": u}))
		if err != nil {
			t.Errorf("Execute(%q) returned error: %v", u, err)
			continue
		}
		if !strings.Contains(strings.ToLower(result.Text), "blocked") {
			t.Errorf("private range %q not blocked, got: %q", u, result.Text)
		}
	}
}

func TestWebfetchBlocksLinkLocal(t *testing.T) {
	opts := ToolOpts{Web: true}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// 169.254.169.254 is the cloud metadata endpoint.
	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"url": "http://169.254.169.254/"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "blocked") {
		t.Fatalf("link-local 169.254.169.254 not blocked, got: %q", result.Text)
	}
}

func TestWebfetchGateOffReturnsNil(t *testing.T) {
	opts := ToolOpts{Web: false}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool != nil {
		t.Fatal("expected nil tool when Web gate is off")
	}
}

func TestWebfetchResolvesHostnameToBlockedIP(t *testing.T) {
	// Point a hostname at a private address via a test resolver.
	// We can't override DNS in this test easily, but we can test
	// the IP-checking function directly.
	opts := ToolOpts{Web: true}
	blueprint := &WebfetchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// localhost resolves to 127.0.0.1 / ::1 — must be blocked.
	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"url": "http://localhost:12345/nonexistent"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "blocked") {
		t.Fatalf("localhost was not blocked by its resolved IP, got: %q", result.Text)
	}
}

// isBlockedIP is tested directly to ensure the range checks are correct.
func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		// Blocked: loopback
		{"127.0.0.1", true},
		{"::1", true},
		// Blocked: link-local
		{"169.254.169.254", true},
		{"169.254.1.1", true},
		// Blocked: private
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		// Blocked: unique local
		{"fc00::1", true},
		{"fd00::1", true},
		// Allowed: public
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false}, // example.com
		{"2001:4860:4860::8888", false},
	}
	for _, tc := range tests {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("invalid test IP: %q", tc.ip)
		}
		if got := isBlockedIP(ip); got != tc.blocked {
			t.Errorf("isBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
		}
	}
}

func mustMarshal(t *testing.T, v any) ToolInput {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return ToolInput(b)
}

// TestIsBlockedIPRanges pins the address ranges. Each of these was either
// reachable before the hardening or is a well-known filter bypass.
func TestIsBlockedIPRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",              // loopback
		"::1",                    // loopback v6
		"169.254.169.254",        // cloud instance metadata
		"10.1.2.3",               // RFC1918
		"192.168.1.1",            // RFC1918
		"172.16.0.1",             // RFC1918
		"0.0.0.0",                // unspecified: connect() remaps to loopback
		"::",                     // unspecified v6
		"0.1.2.3",                // 0.0.0.0/8
		"100.64.1.1",             // CGNAT / tailnet
		"100.127.255.254",        // CGNAT upper edge
		"::ffff:127.0.0.1",       // IPv4-mapped loopback, a classic bypass
		"::ffff:169.254.169.254", // IPv4-mapped metadata
		"fd00::1",                // IPv6 unique local
		"fe80::1",                // IPv6 link-local
		"224.0.0.1",              // multicast
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: %q is not a valid IP", s)
		}
		if !isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = false, want true", s)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:2800:220:1::1"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = true, want false (public address)", s)
		}
	}
}

// TestWebfetchBlocksRedirectToMetadata is the regression test for the
// headline SSRF hole: the address check used to be a pre-flight DNS lookup
// on the URL the model typed, so only the first hop was validated. A server
// answering "302 Location: http://169.254.169.254/…" walked straight into
// cloud instance metadata and put the returned credentials into the model's
// context. The check now lives in the dialer's Control hook, which runs on
// every connection including redirect hops.
//
// This test drives the real production client (no injected transport) with
// only the loopback exemption needed to reach httptest.
func TestWebfetchBlocksRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/security-credentials/", http.StatusFound)
	}))
	defer srv.Close()

	tool, err := (&WebfetchBlueprint{}).Materialize(ToolOpts{Web: true})
	if err != nil {
		t.Fatal(err)
	}
	wt, ok := tool.(*webfetchTool)
	if !ok {
		t.Fatalf("expected *webfetchTool, got %T", tool)
	}
	// Allow loopback so the first hop reaches httptest; everything else
	// keeps the production predicate, so the redirect target stays blocked.
	wt.blocked = func(ip net.IP) bool { return !ip.IsLoopback() && isBlockedIP(ip) }

	res, err := wt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err != nil {
		t.Fatalf("expected a tool result, got a hard error: %v", err)
	}
	if strings.Contains(res.Text, "SecretAccessKey") || strings.Contains(res.Text, "AccessKeyId") {
		t.Fatalf("metadata content reached the model: %q", res.Text)
	}
	if !strings.Contains(res.Text, "blocked") {
		t.Fatalf("expected the redirect to be refused as a blocked address, got: %q", res.Text)
	}
}

// TestWebfetchGuardedClientAllowsNormalFetch proves the guard does not break
// an ordinary fetch — without this, a guard that blocked everything would
// pass the test above for the wrong reason.
func TestWebfetchGuardedClientAllowsNormalFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>hello from the server</p></body></html>")
	}))
	defer srv.Close()

	tool, err := (&WebfetchBlueprint{}).Materialize(ToolOpts{Web: true})
	if err != nil {
		t.Fatal(err)
	}
	wt := tool.(*webfetchTool)
	wt.blocked = func(ip net.IP) bool { return !ip.IsLoopback() && isBlockedIP(ip) }

	res, err := wt.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "hello from the server") {
		t.Fatalf("guarded client broke a normal fetch, got: %q", res.Text)
	}
}
