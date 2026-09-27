package recall

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestValidPath(t *testing.T) {
	c := assert.NewAborting(t)
	for _, p := range []string{"a", "a.b-c.d_1", "sub.notes"} {
		err := ValidPath(p)
		c.NoError(err, "ValidPath(%q) = %v, want nil", p, err)
	}
	for _, p := range []string{"", "a..b", "a.b c", ".a", "a.", strings.Repeat("a", 257), "a/"} {
		err := ValidPath(p)
		c.Error(err, "ValidPath(%q) accepted", p)
		c.ErrorIs(err, ErrInvalidPath, "ValidPath(%q) = %v, want ErrInvalidPath", p, err)
	}
}

func TestValidName(t *testing.T) {
	c := assert.NewAborting(t)
	for _, n := range []string{"n", "my note", strings.Repeat("n", 256)} {
		err := ValidName(n)
		c.NoError(err, "ValidName(%q) = %v, want nil", n, err)
	}
	for _, n := range []string{"", strings.Repeat("n", 257), "two\nlines"} {
		c.Error(ValidName(n), "ValidName(%q) accepted", n)
	}
}
