package tools

import (
	"testing"
)

func TestApplyEditsExactReplace(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")  \n}\n"
	oldStr := "\tfmt.Println(\"world\")  "
	edits := []editPair{{OldString: oldStr, NewString: "\tfmt.Println(\"universe\")"}}

	_, newContent, err := applyEdits(content, edits)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"universe\")\n}\n"
	if newContent != expected {
		t.Fatalf("got %q, want %q", newContent, expected)
	}
}

func TestApplyEditsFuzzyTrailingSpace(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")\n}\n"
	// old_string has trailing space that file doesn't.
	edits := []editPair{{OldString: "\tfmt.Println(\"world\") ", NewString: "\tfmt.Println(\"universe\")"}}

	_, newContent, err := applyEdits(content, edits)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"universe\")\n}\n"
	if newContent != expected {
		t.Fatalf("got %q, want %q", newContent, expected)
	}
}
