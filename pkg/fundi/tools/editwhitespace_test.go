package tools

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestNormalizeWS(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("a b c", normalizeWS("a  b\t\tc"), "got")
	c.Eq("hello", normalizeWS("  hello  "), "got")
	c.Eq("", normalizeWS("   "), "got")
}

func TestVisualizeWS(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("····code", visualizeWS("    code"), "got")
	c.Eq("→code", visualizeWS("\tcode"), "got")
	c.Eq("→→code", visualizeWS("\t\tcode"), "got")
	c.Eq("code  more", visualizeWS("code  more"), "got")
}

func TestDetectIndentUnit(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("\t", detectIndentUnit(strings.Split("func x() {\n\tfoo()\n}", "\n")), "tab file")
	c.Eq("    ", detectIndentUnit(strings.Split("func x() {\n    foo()\n}", "\n")), "4-space file")
	c.Eq("", detectIndentUnit(strings.Split("package main\n\nfunc main() {}", "\n")), "unindented")
}

func TestWhitespaceInsensitiveReplaceTabsVsSpaces(t *testing.T) {
	c := assert.NewAborting(t)
	// File is tab-indented; the model emits spaces at the same nesting level.
	content := "func main() {\n\tfmt.Println(\"hello\")\n}\n"
	old := "func main() {\n    fmt.Println(\"hello\")\n}"
	got, ok := whitespaceInsensitiveReplace(content, old, "func main() {\n    fmt.Println(\"bye\")\n}", false)
	c.True(ok, "expected a whitespace-insensitive match")
	want := "func main() {\n\tfmt.Println(\"bye\")\n}\n"
	c.Eq(want, got, "new lines should be re-indented to tabs")
}

func TestWhitespaceInsensitiveReplaceWrongIndentDepth(t *testing.T) {
	c := assert.NewAborting(t)
	// The model emits the block at the wrong nesting level (one tab too few).
	content := "func main() {\n\tif x {\n\t\tfmt.Println(\"deep\")\n\t}\n}\n"
	old := "if x {\n\tfmt.Println(\"deep\")\n}"
	got, ok := whitespaceInsensitiveReplace(content, old, "if y {\n\tfmt.Println(\"deep\")\n}", false)
	c.True(ok, "expected a whitespace-insensitive match")
	want := "func main() {\n\tif y {\n\t\tfmt.Println(\"deep\")\n\t}\n}\n"
	c.Eq(want, got, "replacement should land at the matched nesting depth")
}

func TestWhitespaceInsensitiveReplaceAmbiguousReturnsFalse(t *testing.T) {
	c := assert.NewAborting(t)
	content := "a\nb()\n\na\nb()\n"
	_, ok := whitespaceInsensitiveReplace(content, "a\nb()", "x", false)
	c.False(ok, "an ambiguous match must not be applied")
}

func TestDiagnoseMismatch(t *testing.T) {
	c := assert.NewAborting(t)

	t.Run("tabs vs spaces", func(t *testing.T) {
		content := "func main() {\n\tfmt.Println(\"hello\")\n}\n"
		old := "func main() {\n    fmt.Println(\"hello\")\n}"
		hint := diagnoseMismatch(content, old)
		c.StrContains(hint, "whitespace-normalized match", "hint =")
		c.StrContains(hint, "→", "hint =")
		c.StrContains(hint, "lines 1-3", "hint =")
	})

	t.Run("wrong indent depth", func(t *testing.T) {
		content := "func main() {\n\tif x {\n\t\tfmt.Println(\"deep\")\n\t}\n}\n"
		old := "if x {\n\tfmt.Println(\"deep\")\n}"
		hint := diagnoseMismatch(content, old)
		c.StrContains(hint, "→", "hint =")
	})

	t.Run("completely different text", func(t *testing.T) {
		content := "package main\n\nfunc main() {}\n"
		old := "this text does not exist anywhere in the file at all"
		c.Eq("", diagnoseMismatch(content, old), "hint should be empty")
	})

	t.Run("partial line match", func(t *testing.T) {
		content := "func foo() {\n\tbar()\n\tbaz()\n}\n"
		old := "func foo() {\n\tbar()\n\tqux()\n}"
		hint := diagnoseMismatch(content, old)
		c.StrContains(hint, "Closest match", "hint =")
	})

	t.Run("empty old string", func(t *testing.T) {
		c.Eq("", diagnoseMismatch("some content", ""), "hint should be empty")
	})
}
