package recall

import (
	"errors"
	"strings"
	"testing"
)

func TestValidPath(t *testing.T) {
	for _, p := range []string{"a", "a.b-c.d_1", "sub.notes"} {
		if err := ValidPath(p); err != nil {
			t.Fatalf("ValidPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"", "a..b", "a.b c", ".a", "a.", strings.Repeat("a", 257), "a/"} {
		err := ValidPath(p)
		if err == nil {
			t.Fatalf("ValidPath(%q) accepted", p)
		}
		if !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("ValidPath(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"n", "my note", strings.Repeat("n", 256)} {
		if err := ValidName(n); err != nil {
			t.Fatalf("ValidName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range []string{"", strings.Repeat("n", 257), "two\nlines"} {
		if err := ValidName(n); err == nil {
			t.Fatalf("ValidName(%q) accepted", n)
		}
	}
}
