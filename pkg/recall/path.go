package recall

import (
	"fmt"
	"strings"
)

// ValidPath checks a memory path: one or more dot-separated labels, each
// 1..256 characters of [A-Za-z0-9_-]. It returns ErrInvalidPath wrapped with
// the offending label.
func ValidPath(p string) error {
	for _, label := range strings.Split(p, ".") {
		if !validLabel(label) {
			return fmt.Errorf("%w: label %q", ErrInvalidPath, label)
		}
	}
	return nil
}

// ValidName checks a memory name: non-empty, at most 256 bytes, no newlines.
func ValidName(n string) error {
	switch {
	case n == "":
		return fmt.Errorf("%w: empty name", ErrInvalidPath)
	case len(n) > 256:
		return fmt.Errorf("%w: name longer than 256 bytes", ErrInvalidPath)
	case strings.ContainsAny(n, "\n\r"):
		return fmt.Errorf("%w: name contains newline", ErrInvalidPath)
	}
	return nil
}

func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
