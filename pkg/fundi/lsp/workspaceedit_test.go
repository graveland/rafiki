package lsp

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestWorkspaceEditDecodesDocumentChanges is the regression test for rename
// being a guaranteed no-op. Only `changes` was decoded, but modern gopls
// always answers with `documentChanges` — so the map was always nil, the
// apply loop never ran, and lsp_rename returned "no files were modified" as
// a SUCCESS string while the files sat untouched.
//
// The payload below is the real shape gopls v0.20.0 returns for a rename.
func TestWorkspaceEditDecodesDocumentChanges(t *testing.T) {
	c := assert.NewAborting(t)
	const raw = `{
	  "documentChanges": [
	    {
	      "textDocument": {"uri": "file:///repo/main.go", "version": 0},
	      "edits": [
	        {"range": {"start":{"line":3,"character":5},"end":{"line":3,"character":8}}, "newText": "Sum"},
	        {"range": {"start":{"line":9,"character":1},"end":{"line":9,"character":4}}, "newText": "Sum"}
	      ]
	    },
	    {
	      "textDocument": {"uri": "file:///repo/other.go", "version": 0},
	      "edits": [
	        {"range": {"start":{"line":2,"character":7},"end":{"line":2,"character":10}}, "newText": "Sum"}
	      ]
	    }
	  ]
	}`

	var we WorkspaceEdit
	c.NoError(json.Unmarshal([]byte(raw), &we))
	byFile, err := we.FileEdits()
	c.NoError(err)
	c.Len(byFile, 2, "got %d files, want 2 — documentChanges was not decoded", len(byFile))
	c.Eq(2, len(byFile["/repo/main.go"]), "main.go: got")
	c.Eq(1, len(byFile["/repo/other.go"]), "other.go: got")
}

// TestWorkspaceEditDecodesLegacyChanges keeps the older shape working, since
// not every server emits documentChanges.
func TestWorkspaceEditDecodesLegacyChanges(t *testing.T) {
	c := assert.NewAborting(t)
	const raw = `{"changes": {"file:///repo/a.go": [
		{"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}},"newText":"x"}
	]}}`
	var we WorkspaceEdit
	c.NoError(json.Unmarshal([]byte(raw), &we))
	byFile, err := we.FileEdits()
	c.NoError(err)
	c.Len(byFile["/repo/a.go"], 1, "legacy changes shape not decoded: %#v", byFile)
}

// TestWorkspaceEditRefusesResourceOperations: silently dropping a file
// create/rename/delete would leave a half-finished refactor on disk with no
// indication anything was skipped.
func TestWorkspaceEditRefusesResourceOperations(t *testing.T) {
	c := assert.NewAborting(t)
	const raw = `{"documentChanges":[
		{"kind":"rename","oldUri":"file:///repo/a.go","newUri":"file:///repo/b.go"}
	]}`
	var we WorkspaceEdit
	c.NoError(json.Unmarshal([]byte(raw), &we))
	_, err := we.FileEdits()
	c.Error(err, "a resource operation must be refused, not dropped")
}

// TestURIRoundTripEscapes: a workspace path containing a space produced an
// invalid URI outbound, and the server's properly-escaped reply came back
// with a literal %20 in the path, so os.ReadFile failed with ENOENT — in the
// middle of a multi-file rename, after other files had been written.
func TestURIRoundTripEscapes(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, path := range []string{
		"/Users/me/My Projects/app/main.go",
		"/tmp/plain/main.go",
		"/tmp/uni/日本語/main.go",
		"/tmp/pct/100%/main.go",
	} {
		uri := PathToURI(path)
		got, err := URIToPath(uri)
		c.Require().NoError(err, "URIToPath(%q) from %q", uri, path)
		c.Eq(path, got, "round trip lost data: %q -> %q ->", path, uri)
	}
}

func TestURIToPathRejectsNonFileScheme(t *testing.T) {
	_, err := URIToPath("https://example.com/x.go")
	assert.NewAborting(t).Error(err, "a non-file URI must be an error, not a mangled path")
}

// TestPositionToOffsetUTF16 pins the conversion the whole edit path depends
// on. `x := "日本語" + foo`: each CJK rune is 1 UTF-16 unit but 3 bytes, so a
// byte-indexed reading of column 13 lands 6 bytes early, mid-rune.
func TestPositionToOffsetUTF16(t *testing.T) {
	text := "x := \"日本語\" + foo\n"
	off := PositionToOffset(text, Position{Line: 0, Character: 13}, PositionEncodingUTF16)
	got := text[off : off+3]
	assert.NewAborting(t).Eq("foo", got, "offset %d points at %q, want \"foo\"", off, got)
}

func TestPositionToOffsetClampsColumnPastEOL(t *testing.T) {
	text := "ab\ncd\n"
	// Column far past the end of line 0 must clamp to end-of-line, not run
	// into line 1.
	off := PositionToOffset(text, Position{Line: 0, Character: 99}, PositionEncodingUTF16)
	assert.NewAborting(t).Eq(2, off, "offset")
}

func TestPositionToOffsetSecondLine(t *testing.T) {
	text := "ab\ncd\n"
	off := PositionToOffset(text, Position{Line: 1, Character: 1}, PositionEncodingUTF16)
	assert.NewAborting(t).Eq('d', text[off], "offset %d points at %q, want 'd'", off, text[off])
}

func TestEffectivePositionEncodingDefaultsToUTF16(t *testing.T) {
	c := assert.NewAborting(t)
	// An absent value means utf-16 per spec. Reading "" as utf-8 is what
	// made the byte-indexed code look correct.
	c.Eq(PositionEncodingUTF16, (ServerCapabilities{}).EffectivePositionEncoding(), "empty positionEncoding =>")
	c.Eq(PositionEncodingUTF8, (ServerCapabilities{PositionEncoding: "utf-8"}).EffectivePositionEncoding(), "utf-8 =>")
}
