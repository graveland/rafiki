package tools

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestApplyEditsExactReplace(t *testing.T) {
	c := assert.NewAborting(t)
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")  \n}\n"
	oldStr := "\tfmt.Println(\"world\")  "
	edits := []editPair{{OldString: oldStr, NewString: "\tfmt.Println(\"universe\")"}}

	_, newContent, err := applyEdits(content, edits)
	c.NoError(err, "unexpected error")
	expected := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"universe\")\n}\n"
	c.Eq(expected, newContent, "got")
}

func TestApplyEditsFuzzyTrailingSpace(t *testing.T) {
	c := assert.NewAborting(t)
	content := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"world\")\n}\n"
	// old_string has trailing space that file doesn't.
	edits := []editPair{{OldString: "\tfmt.Println(\"world\") ", NewString: "\tfmt.Println(\"universe\")"}}

	_, newContent, err := applyEdits(content, edits)
	c.NoError(err, "unexpected error")
	expected := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tfmt.Println(\"universe\")\n}\n"
	c.Eq(expected, newContent, "got")
}
