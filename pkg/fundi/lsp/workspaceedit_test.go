package lsp

import (
	"encoding/json"
	"testing"
)

// TestWorkspaceEditDecodesDocumentChanges is the regression test for rename
// being a guaranteed no-op. Only `changes` was decoded, but modern gopls
// always answers with `documentChanges` — so the map was always nil, the
// apply loop never ran, and lsp_rename returned "no files were modified" as
// a SUCCESS string while the files sat untouched.
//
// The payload below is the real shape gopls v0.20.0 returns for a rename.
func TestWorkspaceEditDecodesDocumentChanges(t *testing.T) {
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
	if err := json.Unmarshal([]byte(raw), &we); err != nil {
		t.Fatal(err)
	}
	byFile, err := we.FileEdits()
	if err != nil {
		t.Fatal(err)
	}
	if len(byFile) != 2 {
		t.Fatalf("got %d files, want 2 — documentChanges was not decoded", len(byFile))
	}
	if n := len(byFile["/repo/main.go"]); n != 2 {
		t.Fatalf("main.go: got %d edits, want 2", n)
	}
	if n := len(byFile["/repo/other.go"]); n != 1 {
		t.Fatalf("other.go: got %d edits, want 1", n)
	}
}

// TestWorkspaceEditDecodesLegacyChanges keeps the older shape working, since
// not every server emits documentChanges.
func TestWorkspaceEditDecodesLegacyChanges(t *testing.T) {
	const raw = `{"changes": {"file:///repo/a.go": [
		{"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":3}},"newText":"x"}
	]}}`
	var we WorkspaceEdit
	if err := json.Unmarshal([]byte(raw), &we); err != nil {
		t.Fatal(err)
	}
	byFile, err := we.FileEdits()
	if err != nil {
		t.Fatal(err)
	}
	if len(byFile["/repo/a.go"]) != 1 {
		t.Fatalf("legacy changes shape not decoded: %#v", byFile)
	}
}

// TestWorkspaceEditRefusesResourceOperations: silently dropping a file
// create/rename/delete would leave a half-finished refactor on disk with no
// indication anything was skipped.
func TestWorkspaceEditRefusesResourceOperations(t *testing.T) {
	const raw = `{"documentChanges":[
		{"kind":"rename","oldUri":"file:///repo/a.go","newUri":"file:///repo/b.go"}
	]}`
	var we WorkspaceEdit
	if err := json.Unmarshal([]byte(raw), &we); err != nil {
		t.Fatal(err)
	}
	if _, err := we.FileEdits(); err == nil {
		t.Fatal("a resource operation must be refused, not dropped")
	}
}

// TestURIRoundTripEscapes: a workspace path containing a space produced an
// invalid URI outbound, and the server's properly-escaped reply came back
// with a literal %20 in the path, so os.ReadFile failed with ENOENT — in the
// middle of a multi-file rename, after other files had been written.
func TestURIRoundTripEscapes(t *testing.T) {
	for _, path := range []string{
		"/Users/me/My Projects/app/main.go",
		"/tmp/plain/main.go",
		"/tmp/uni/日本語/main.go",
		"/tmp/pct/100%/main.go",
	} {
		uri := PathToURI(path)
		got, err := URIToPath(uri)
		if err != nil {
			t.Fatalf("URIToPath(%q) from %q: %v", uri, path, err)
		}
		if got != path {
			t.Errorf("round trip lost data: %q -> %q -> %q", path, uri, got)
		}
	}
}

func TestURIToPathRejectsNonFileScheme(t *testing.T) {
	if _, err := URIToPath("https://example.com/x.go"); err == nil {
		t.Fatal("a non-file URI must be an error, not a mangled path")
	}
}

// TestPositionToOffsetUTF16 pins the conversion the whole edit path depends
// on. `x := "日本語" + foo`: each CJK rune is 1 UTF-16 unit but 3 bytes, so a
// byte-indexed reading of column 13 lands 6 bytes early, mid-rune.
func TestPositionToOffsetUTF16(t *testing.T) {
	text := "x := \"日本語\" + foo\n"
	off := PositionToOffset(text, Position{Line: 0, Character: 13}, PositionEncodingUTF16)
	if got := text[off : off+3]; got != "foo" {
		t.Fatalf("offset %d points at %q, want \"foo\"", off, got)
	}
}

func TestPositionToOffsetClampsColumnPastEOL(t *testing.T) {
	text := "ab\ncd\n"
	// Column far past the end of line 0 must clamp to end-of-line, not run
	// into line 1.
	off := PositionToOffset(text, Position{Line: 0, Character: 99}, PositionEncodingUTF16)
	if off != 2 {
		t.Fatalf("offset %d, want 2 (end of line 0)", off)
	}
}

func TestPositionToOffsetSecondLine(t *testing.T) {
	text := "ab\ncd\n"
	off := PositionToOffset(text, Position{Line: 1, Character: 1}, PositionEncodingUTF16)
	if text[off] != 'd' {
		t.Fatalf("offset %d points at %q, want 'd'", off, text[off])
	}
}

func TestEffectivePositionEncodingDefaultsToUTF16(t *testing.T) {
	// An absent value means utf-16 per spec. Reading "" as utf-8 is what
	// made the byte-indexed code look correct.
	if got := (ServerCapabilities{}).EffectivePositionEncoding(); got != PositionEncodingUTF16 {
		t.Fatalf("empty positionEncoding => %q, want %q", got, PositionEncodingUTF16)
	}
	if got := (ServerCapabilities{PositionEncoding: "utf-8"}).EffectivePositionEncoding(); got != PositionEncodingUTF8 {
		t.Fatalf("utf-8 => %q", got)
	}
}
