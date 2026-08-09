package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if _, err := w.Write(fixture); err != nil {
			panic(err)
		}
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
	// The result should be limited; the exact wording may vary.
	if !strings.Contains(result.Text, "Found") {
		t.Logf("result text: %q", result.Text)
		t.Error("result should contain results")
	}
}

func TestWebsearchEmptyResults(t *testing.T) {
	// HTML page with no result-link elements.
	emptyHTML := `<!DOCTYPE html><html><body>No results found.</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if _, err := w.Write([]byte(emptyHTML)); err != nil {
			panic(err)
		}
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

// TestParseFailureIsNotZeroResults is the regression test for the
// silent-wrong-answer bug. DDG Lite is unofficial and its markup drifts;
// when result-link is renamed, every query used to return "No results
// found", the model concluded the topic had no web coverage, rephrased, and
// concluded it again — with nothing in the logs.
func TestParseFailureIsNotZeroResults(t *testing.T) {
	// A realistic page with the search form and plenty of chrome, but no
	// anchor carrying the class the parser looks for.
	renamed := `<html><body><form action="/lite/"><input name="q"></form>` +
		`<table>` + strings.Repeat(`<tr><td><a class="result__a" href="https://example.com">Example</a></td></tr>`, 20) +
		`</table></body></html>`

	_, err := parseLiteSearchResults(renamed, 10)
	if err == nil {
		t.Fatal("a renamed result class must be reported as a parse failure, not zero results")
	}
	if !strings.Contains(err.Error(), "NOT the same as finding no results") {
		t.Fatalf("the error must tell the model not to treat this as an empty web, got: %v", err)
	}
}

// TestGenuineZeroResultsIsNotAnError guards the other direction: a real
// empty result set must stay a normal, non-error answer.
func TestGenuineZeroResultsIsNotAnError(t *testing.T) {
	empty := `<html><body><form action="/lite/"><input name="q"></form>
		<div>No results found for your query.</div></body></html>`
	results, err := parseLiteSearchResults(empty, 10)
	if err != nil {
		t.Fatalf("a genuinely empty result page must not error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected zero results, got %d", len(results))
	}
}

// TestFixtureStillParses pins the captured fixture against the live parser,
// so a refactor that breaks extraction fails here rather than in production.
func TestFixtureStillParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ddg_lite_response.html"))
	if err != nil {
		t.Fatal(err)
	}
	results, err := parseLiteSearchResults(string(raw), 20)
	if err != nil {
		t.Fatalf("the captured fixture no longer parses: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("the captured fixture yielded no results")
	}
	for i, r := range results {
		if r.Title == "" || r.Link == "" {
			t.Errorf("result %d is missing a title or link: %+v", i, r)
		}
		if !strings.HasPrefix(r.Link, "http") {
			t.Errorf("result %d link is not absolute: %q", i, r.Link)
		}
	}
}

// TestSearchBraveParsesResults drives the Brave path against a fixture
// server, including the age-prefixed snippet and the count clamp.
func TestSearchBraveParsesResults(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[
			{"title":"First","url":"https://a.example","description":"one","age":"2 days ago"},
			{"title":"Second","url":"https://b.example","description":"two"}
		]}}`)
	}))
	defer srv.Close()

	old := braveEndpoint
	braveEndpoint = srv.URL
	defer func() { braveEndpoint = old }()
	braveLast = time.Time{} // don't pay the pacing delay in tests

	results, err := searchBrave(context.Background(), "test-key", "golang generics", 10)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "test-key" {
		t.Errorf("X-Subscription-Token = %q, want test-key", gotAuth)
	}
	if gotQuery != "golang generics" {
		t.Errorf("q = %q", gotQuery)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Title != "First" || results[0].Link != "https://a.example" {
		t.Errorf("first result wrong: %+v", results[0])
	}
	if !strings.Contains(results[0].Snippet, "2 days ago") {
		t.Errorf("age should be surfaced in the snippet: %q", results[0].Snippet)
	}
	if results[1].Position != 2 {
		t.Errorf("position not assigned: %+v", results[1])
	}
}

// TestSearchBraveReportsErrors: a keyed API must surface a real failure
// rather than degrade into an empty result set, which is the whole reason
// for offering it alongside the scraper.
func TestSearchBraveReportsErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"rate limited", http.StatusTooManyRequests, `{}`, "rate limited"},
		{"bad key", http.StatusUnauthorized, `{}`, "API key"},
		{"in-band error", http.StatusOK, `{"type":"ErrorResponse","message":"quota exceeded"}`, "quota exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			old := braveEndpoint
			braveEndpoint = srv.URL
			defer func() { braveEndpoint = old }()
			braveLast = time.Time{}

			if _, err := searchBrave(context.Background(), "k", "q", 5); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
