//go:build tools
// +build tools

// Package tools tracks build/runtime dependencies that aren't yet
// imported by application code. This keeps them out of `go mod tidy`'s
// crosshairs during bootstrap until later tasks import them directly.
package tools

import (
	_ "github.com/oklog/ulid/v2"
	_ "github.com/puzpuzpuz/xsync/v4"
)
