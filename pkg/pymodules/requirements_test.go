// SPDX-License-Identifier: Apache-2.0

package pymodules

import (
	"slices"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestParseRequirementsExtractsMarkerBlock(t *testing.T) {
	code := "import os\n\n# pymodule-requirements:\n# requests>=2.31\n# numpy\n\ndef main():\n    pass\n"
	got := ParseRequirements(code)
	want := []string{"requests>=2.31", "numpy"}
	assert.NewAborting(t).EqDiff(want, got, "ParseRequirements(...)")
}

func TestParseRequirementsReturnsNilWithoutMarker(t *testing.T) {
	code := "import os\n\ndef main():\n    pass\n"
	assert.NewAborting(t).Nil(ParseRequirements(code), "ParseRequirements(...)")
}

func TestParseRequirementsReturnsNilForEmptyBlock(t *testing.T) {
	t.Run("marker followed by code", func(t *testing.T) {
		code := "# pymodule-requirements:\ndef main():\n    pass\n"
		assert.NewAborting(t).Nil(ParseRequirements(code), "ParseRequirements(...)")
	})
	t.Run("marker at EOF", func(t *testing.T) {
		code := "import os\n# pymodule-requirements:"
		assert.NewAborting(t).Nil(ParseRequirements(code), "ParseRequirements(...)")
	})
}

func TestParseRequirementsStopsAtFirstNonCommentLine(t *testing.T) {
	code := "# pymodule-requirements:\n# requests>=2.31\nimport os\n# not-a-requirement\n"
	got := ParseRequirements(code)
	want := []string{"requests>=2.31"}
	assert.NewAborting(t).EqDiff(want, got, "ParseRequirements(...)")
}

func TestParseRequirementsIgnoresMarkerNotAtLineStart(t *testing.T) {
	code := "x = 1  # pymodule-requirements:\n# requests\n"
	assert.NewAborting(t).Nil(ParseRequirements(code), "ParseRequirements(...)")
}

func TestRequirementsHashStableForSameOrder(t *testing.T) {
	reqs := []string{"requests>=2.31", "numpy"}
	assert.NewAborting(t).Eq(RequirementsHash(slices.Clone(reqs)), RequirementsHash(reqs), "same reqs hashed to different digests")
}

func TestRequirementsHashDiffersForDifferentOrder(t *testing.T) {
	assert.NewAborting(t).NotEq(RequirementsHash([]string{"b", "a"}), RequirementsHash([]string{"a", "b"}), "reordered reqs hashed the same, want different digests")
}

func TestRequirementsHashDiffersForDifferentContent(t *testing.T) {
	assert.NewAborting(t).NotEq(RequirementsHash([]string{"a", "b"}), RequirementsHash([]string{"a"}), "different reqs hashed the same, want different digests")
}
