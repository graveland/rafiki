// SPDX-License-Identifier: Apache-2.0

package pymodules

import (
	"reflect"
	"slices"
	"testing"
)

func TestParseRequirementsExtractsMarkerBlock(t *testing.T) {
	code := "import os\n\n# pymodule-requirements:\n# requests>=2.31\n# numpy\n\ndef main():\n    pass\n"
	got := ParseRequirements(code)
	want := []string{"requests>=2.31", "numpy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseRequirements(...) = %#v, want %#v", got, want)
	}
}

func TestParseRequirementsReturnsNilWithoutMarker(t *testing.T) {
	code := "import os\n\ndef main():\n    pass\n"
	if got := ParseRequirements(code); got != nil {
		t.Fatalf("ParseRequirements(...) = %#v, want nil", got)
	}
}

func TestParseRequirementsReturnsNilForEmptyBlock(t *testing.T) {
	t.Run("marker followed by code", func(t *testing.T) {
		code := "# pymodule-requirements:\ndef main():\n    pass\n"
		if got := ParseRequirements(code); got != nil {
			t.Fatalf("ParseRequirements(...) = %#v, want nil", got)
		}
	})
	t.Run("marker at EOF", func(t *testing.T) {
		code := "import os\n# pymodule-requirements:"
		if got := ParseRequirements(code); got != nil {
			t.Fatalf("ParseRequirements(...) = %#v, want nil", got)
		}
	})
}

func TestParseRequirementsStopsAtFirstNonCommentLine(t *testing.T) {
	code := "# pymodule-requirements:\n# requests>=2.31\nimport os\n# not-a-requirement\n"
	got := ParseRequirements(code)
	want := []string{"requests>=2.31"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseRequirements(...) = %#v, want %#v (later comments must not merge in)", got, want)
	}
}

func TestParseRequirementsIgnoresMarkerNotAtLineStart(t *testing.T) {
	code := "x = 1  # pymodule-requirements:\n# requests\n"
	if got := ParseRequirements(code); got != nil {
		t.Fatalf("ParseRequirements(...) = %#v, want nil (mid-line marker text is not the marker)", got)
	}
}

func TestRequirementsHashStableForSameOrder(t *testing.T) {
	reqs := []string{"requests>=2.31", "numpy"}
	if RequirementsHash(reqs) != RequirementsHash(slices.Clone(reqs)) {
		t.Fatalf("same reqs hashed to different digests")
	}
}

func TestRequirementsHashDiffersForDifferentOrder(t *testing.T) {
	if RequirementsHash([]string{"a", "b"}) == RequirementsHash([]string{"b", "a"}) {
		t.Fatalf("reordered reqs hashed the same, want different digests")
	}
}

func TestRequirementsHashDiffersForDifferentContent(t *testing.T) {
	if RequirementsHash([]string{"a"}) == RequirementsHash([]string{"a", "b"}) {
		t.Fatalf("different reqs hashed the same, want different digests")
	}
}
