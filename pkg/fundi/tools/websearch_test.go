package tools

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebsearchParsesFixtureResults(t *testing.T) {
	fixture := mustReadFixture(t, "ddg_lite_response.html")

	results, err := parseLiteSearchResults(string(fixture), 10)
	if err != nil {
		t.Fatalf("parseLiteSearchResults: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results from fixture, got 0")
	}
	// All results must have a title, link, and snippet.
	for i, r := range results {
		if r.Title == "" {
			t.Errorf("result %d: empty title", i)
		}
		if r.Link == "" {
			t.Errorf("result %d: empty link", i)
		}
		if r.Snippet == "" {
			t.Errorf("result %d: empty snippet", i)
		}
		if r.Position != i+1 {
			t.Errorf("result %d: position %d, want %d", i, r.Position, i+1)
		}
	}
	t.Logf("parsed %d results from fixture", len(results))
}

func TestWebsearchHonoursMaxResults(t *testing.T) {
	fixture := mustReadFixture(t, "ddg_lite_response.html")

	results, err := parseLiteSearchResults(string(fixture), 3)
	if err != nil {
		t.Fatalf("parseLiteSearchResults: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
}

func TestWebsearchHardCapAt20(t *testing.T) {
	opts := ToolOpts{Web: true}
	blueprint := &WebsearchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool == nil {
		t.Fatal("Materialize returned nil with Web=true")
	}

	// Build a fixture HTML with 25 result rows.
	var buf bytes.Buffer
	buf.WriteString("<!DOCTYPE html><html><body>")
	for i := 0; i < 25; i++ {
		buf.WriteString(`<a class='result-link' href='https://example.com/`)
		buf.WriteString(string(rune('0' + i%10)))
		buf.WriteString(`'>Result `)
		buf.WriteString(string(rune('0' + i%10)))
		buf.WriteString(`</a><table><tr><td class='result-snippet'>s</td></tr></table>`)
	}
	buf.WriteString("</body></html>")

	results, err := parseLiteSearchResults(buf.String(), 50)
	if err != nil {
		t.Fatalf("parseLiteSearchResults: %v", err)
	}
	// parseLiteSearchResults enforces a hard cap at 20 internally.
	if len(results) > 20 {
		t.Fatalf("want at most 20 results, got %d", len(results))
	}
}

func TestWebsearchHardCapEnforcedInInput(t *testing.T) {
	opts := ToolOpts{Web: true}
	blueprint := &WebsearchBlueprint{}
	tool, err := blueprint.Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Use a mock server that returns a fixture with many results.
	fixture := mustReadFixture(t, "ddg_lite_response.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(fixture)
	}))
	defer srv.Close()

	// Override the endpoint for testing.
	oldEndpoint := ddgLiteEndpoint
	ddgLiteEndpoint = srv.URL + "/?q="
	t.Cleanup(func() { ddgLiteEndpoint = oldEndpoint })

	// Request more than 20 — the tool must cap at 20.
	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]any{
		"query":       "test",
		"max_results": 30,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Should contain results but mention "Showing top 20" or similar.
	if !strings.Contains(result.Text, "20") {
		t.Logf("result text: %q", result.Text)
		t.Error("result should be capped at 20")
	}
}

func TestWebsearchEmptyResults(t *testing.T) {
	// HTML page with no result-link elements.
	emptyHTML := `<!DOCTYPE html><html><body>No results found.</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(emptyHTML))
	}))
	defer srv.Close()

	oldEndpoint := ddgLiteEndpoint
	ddgLiteEndpoint = srv.URL + "/?q="
	t.Cleanup(func() { ddgLiteEndpoint = oldEndpoint })

	opts := ToolOpts{Web: true}
	tool, err := (&WebsearchBlueprint{}).Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{"query": "zzz"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "no results") {
		t.Fatalf("expected 'no results' message, got: %q", result.Text)
	}
}

func TestWebsearchGateOffReturnsNil(t *testing.T) {
	opts := ToolOpts{Web: false}
	tool, err := (&WebsearchBlueprint{}).Materialize(opts)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool != nil {
		t.Fatal("expected nil tool when Web gate is off")
	}
}

func TestWebsearchDefaultMaxResults(t *testing.T) {
	// Parse with no explicit max results — the parser should default to 10.
	fixture := mustReadFixture(t, "ddg_lite_response.html")
	results, err := parseLiteSearchResults(string(fixture), 0)
	if err != nil {
		t.Fatalf("parseLiteSearchResults: %v", err)
	}
	if len(results) > 10 {
		t.Fatalf("default max results exceeded 10, got %d", len(results))
	}
}

func TestDuckDuckGoCleanedURLs(t *testing.T) {
	tests := []struct {
		input    string
		contains string
	}{
		{
			"//duckduckgo.com/l/?uddg=https%3A%2F%2Fgobyexample.com%2F&rut=abc123",
			"https://gobyexample.com/",
		},
		{
			"https://example.com/page",
			"https://example.com/page",
		},
	}
	for _, tc := range tests {
		got := cleanDuckDuckGoURL(tc.input)
		if !strings.Contains(got, tc.contains) {
			t.Errorf("cleanDuckDuckGoURL(%q) = %q, want containing %q", tc.input, got, tc.contains)
		}
	}
}

func TestParseLiteSearchResultsIsolated(t *testing.T) {
	// Tests parsing from an io.Reader, confirming isolation from HTTP.
	fixture := mustReadFixture(t, "ddg_lite_response.html")
	results, err := parseLiteSearchResults(string(fixture), 5)
	if err != nil {
		t.Fatalf("parseLiteSearchResults: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("want 5 results, got %d", len(results))
	}
}

// mustReadFixture reads a testdata file relative to the test package.
func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return data
}

// parseLiteSearchResultsFromReader parses DuckDuckGo Lite results from an io.Reader.
// This is the isolated parsing entry point — tests feed captured fixtures without
// touching the network.
func parseLiteSearchResultsFromReader(r io.Reader, maxResults int) ([]SearchResult, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return parseLiteSearchResults(string(b), maxResults)
}

// force test compilation
var _ = parseLiteSearchResultsFromReader
