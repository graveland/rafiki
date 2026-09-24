package prefill

import (
	"fmt"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestParseList(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    []protocol.PrefillRead
		wantErr string
	}{
		{
			name: "plain path",
			text: "main.go\n",
			want: []protocol.PrefillRead{{Path: "main.go"}},
		},
		{
			name: "range with both bounds",
			text: "a.go:10-20",
			want: []protocol.PrefillRead{{Path: "a.go", Start: 10, End: 20}},
		},
		{
			name: "range open to EOF",
			text: "a.go:10-",
			want: []protocol.PrefillRead{{Path: "a.go", Start: 10}},
		},
		{
			name: "range open from start",
			text: "a.go:-5",
			want: []protocol.PrefillRead{{Path: "a.go", End: 5}},
		},
		{
			name: "colon in path, no range",
			text: "weird:name.go",
			want: []protocol.PrefillRead{{Path: "weird:name.go"}},
		},
		{
			name: "colon in path with range",
			text: "weird:name.go:3-4",
			want: []protocol.PrefillRead{{Path: "weird:name.go", Start: 3, End: 4}},
		},
		{
			name: "comments and blank lines skipped",
			text: "\n# only a comment\n\n   \n\tmain.go  \nsub/x.go # trailing comment\n",
			want: []protocol.PrefillRead{{Path: "main.go"}, {Path: "sub/x.go"}},
		},
		{
			name: "glob accepted",
			text: "src/**/*.rs",
			want: []protocol.PrefillRead{{Path: "src/**/*.rs"}},
		},
		{
			name:    "empty range",
			text:    "a.go:-",
			wantErr: `prefill: line 1: empty range ":-"`,
		},
		{
			name:    "zero is not a line number",
			text:    "a.go:0-3",
			wantErr: "prefill: line 1: zero is not a valid line number (ranges are 1-based)",
		},
		{
			name:    "start after end",
			text:    "a.go:5-3",
			wantErr: "prefill: line 1: range start 5 is after end 3",
		},
		{
			name:    "glob cannot carry a range",
			text:    "src/*.go:1-2",
			wantErr: `prefill: line 1: glob "src/*.go" cannot carry a line range`,
		},
		{
			name:    "error carries the 1-based line number",
			text:    "main.go\n# a comment\n\na.go:-\n",
			wantErr: `prefill: line 4: empty range ":-"`,
		},
		{
			name:    "all comments is empty",
			text:    "# one\n\n# two\n",
			wantErr: "prefill: no entries",
		},
		{
			name:    "empty input is empty",
			text:    "",
			wantErr: "prefill: no entries",
		},
		{
			name:    "too many entries",
			text:    strings.Repeat("f.go\n", MaxEntries+1),
			wantErr: fmt.Sprintf("prefill: too many entries: %d (max %d)", MaxEntries+1, MaxEntries),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.text)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want error %q", tt.text, got, tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("Parse(%q) error = %q, want %q", tt.text, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tt.text, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tt.text, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Parse(%q)[%d] = %+v, want %+v", tt.text, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseEntriesMatchesParse(t *testing.T) {
	// ParseEntries must handle comments and blanks exactly as Parse does.
	lines := []string{"", "# header", "a.go:1-2", "  ", "b.go # tail"}
	got, err := ParseEntries(lines)
	if err != nil {
		t.Fatalf("ParseEntries(%q) unexpected error: %v", lines, err)
	}
	want, _ := Parse(strings.Join(lines, "\n"))
	if len(got) != len(want) {
		t.Fatalf("ParseEntries = %v, Parse = %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ParseEntries[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := ParseEntries([]string{"a.go:-"}); err == nil || err.Error() != `prefill: line 1: empty range ":-"` {
		t.Errorf("ParseEntries error = %v, want empty-range error on line 1", err)
	}
}

func TestPrefillValidate(t *testing.T) {
	tests := []struct {
		name    string
		entries []protocol.PrefillRead
		wantErr string
	}{
		{
			name:    "open both bounds is valid",
			entries: []protocol.PrefillRead{{Path: "a.go", Start: 0, End: 0}},
		},
		{
			name:    "start only is valid",
			entries: []protocol.PrefillRead{{Path: "a.go", Start: 5}},
		},
		{
			name:    "end only is valid",
			entries: []protocol.PrefillRead{{Path: "a.go", End: 5}},
		},
		{
			name:    "negative start",
			entries: []protocol.PrefillRead{{Path: "a.go", Start: -1}},
			wantErr: "prefill: entry 1: start -1 is negative",
		},
		{
			name:    "negative end",
			entries: []protocol.PrefillRead{{Path: "a.go", End: -3}},
			wantErr: "prefill: entry 1: end -3 is negative",
		},
		{
			name:    "start after end",
			entries: []protocol.PrefillRead{{Path: "a.go", Start: 5, End: 3}},
			wantErr: "prefill: entry 1: start 5 is after end 3",
		},
		{
			name:    "empty path",
			entries: []protocol.PrefillRead{{}},
			wantErr: "prefill: entry 1: empty path",
		},
		{
			name:    "glob with end set",
			entries: []protocol.PrefillRead{{Path: "src/*.go", End: 5}},
			wantErr: `prefill: entry 1: glob "src/*.go" cannot carry a line range`,
		},
		{
			name:    "glob with start set",
			entries: []protocol.PrefillRead{{Path: "src/*.go", Start: 5}},
			wantErr: `prefill: entry 1: glob "src/*.go" cannot carry a line range`,
		},
		{
			name:    "entry numbers are 1-based",
			entries: []protocol.PrefillRead{{Path: "ok.go"}, {}},
			wantErr: "prefill: entry 2: empty path",
		},
		{
			name:    "nil entries",
			wantErr: "prefill: no entries",
		},
		{
			name:    "too many entries",
			entries: make([]protocol.PrefillRead, MaxEntries+1),
			wantErr: fmt.Sprintf("prefill: too many entries: %d (max %d)", MaxEntries+1, MaxEntries),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.entries)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%v) unexpected error: %v", tt.entries, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%v) = nil, want error %q", tt.entries, tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("Validate(%v) error = %q, want %q", tt.entries, err, tt.wantErr)
			}
		})
	}
}

func TestPrefillIsGlob(t *testing.T) {
	tests := map[string]bool{
		"a.go":               false,
		"weird:name.go":      false,
		"src/**/*.rs":        true,
		"src/*.go":           true,
		"file?.go":           true,
		"file[ab].go":        true,
		"file{a,b}.go":       true,
		"dir/{a,b}/x.go":     true,
		"dir/{a,b}/x.go:1-2": true,
	}
	for path, want := range tests {
		if got := IsGlob(path); got != want {
			t.Errorf("IsGlob(%q) = %v, want %v", path, got, want)
		}
	}
}
