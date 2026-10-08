package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestDownloadWritesFile(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from http"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	tool, err := DownloadBlueprint{}.Materialize(ToolOpts{Web: true, Cwd: dir, HTTPClient: srv.Client()})
	c.NoError(err, "Materialize")
	c.NotNil(tool, "Materialize returned nil with Web=true")

	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{
		"url": srv.URL, "file_path": "sub/out.txt",
	}))
	c.NoError(err, "Execute")

	// Parent directories are created and the body lands verbatim.
	b, err := os.ReadFile(filepath.Join(dir, "sub", "out.txt"))
	c.NoError(err, "downloaded file missing")
	c.Eq("hello from http", string(b), "file content = %q")
	c.StrContains(result.Text, "out.txt", "result =")
}

func TestDownloadRejectsNonHTTPScheme(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := DownloadBlueprint{}.Materialize(ToolOpts{Web: true, Cwd: t.TempDir()})
	c.Require().NoError(err, "Materialize")

	for _, scheme := range []string{"ftp://example.com/x", "file:///etc/passwd"} {
		result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{
			"url": scheme, "file_path": "x",
		}))
		c.Require().NoError(err, "Execute(%q)", scheme)
		c.StrContains(strings.ToLower(result.Text), "only http", "want scheme rejection for %q, got: %q", scheme, result.Text)
	}
}

func TestDownloadNon2xxWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	tool, err := DownloadBlueprint{}.Materialize(ToolOpts{Web: true, Cwd: dir, HTTPClient: srv.Client()})
	c.NoError(err, "Materialize")

	result, err := tool.Execute(context.Background(), mustMarshal(t, map[string]string{
		"url": srv.URL, "file_path": "out.txt",
	}))
	c.NoError(err, "Execute")
	c.StrContains(result.Text, "404", "want the status in the result, got: %q", result.Text)
	_, statErr := os.Stat(filepath.Join(dir, "out.txt"))
	c.True(os.IsNotExist(statErr), "no file should be written on a non-2xx response")
}

func TestDownloadMaterializeDeclinesWithoutWeb(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := DownloadBlueprint{}.Materialize(ToolOpts{Web: false, Cwd: t.TempDir()})
	c.Require().NoError(err, "Materialize")
	c.Nil(tool, "expected nil tool when Web is false")
}
