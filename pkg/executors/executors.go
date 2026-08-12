// Package executors manages the executor registry: the database rows where
// identity and trust labels live, atomic enrollment against one-time tokens,
// and label selectors that decide where a child runs.
package executors

import (
	"context"
	"time"
)

// Executor is a row from the executors table, authoritative for everything
// that gates access. A credential proves only binding to a row, never what
// the row says.
type Executor struct {
	ID            string
	DisplayName   string
	Labels        map[string]string
	SelfReported  map[string]string
	Annotations   map[string]string
	Roots         []string
	Isolation     string
	WorkspaceMode string
	Admits        string
	Enabled       bool
	EnrolledAt    time.Time
	LastSeenAt    time.Time
}

// NewToken carries everything the minter supplies to create an enrollment token.
type NewToken struct {
	Labels        map[string]string
	Roots         []string
	Isolation     string
	WorkspaceMode string
	Admits        string
	MintedBy      string
	ExpiresAt     time.Time
}

// Store is the executor registry's persistence layer. The implementation
// is in postgres.go; a conformance test against the interface lives in
// conformance_test.go.
type Store interface {
	MintToken(ctx context.Context, t NewToken) (plaintext string, err error)
	Enroll(ctx context.Context, token string, self map[string]string) (Executor, string, error)
	Authenticate(ctx context.Context, credential string) (Executor, error)
	Get(ctx context.Context, id string) (Executor, error)
	List(ctx context.Context) ([]Executor, error)
	SetLabels(ctx context.Context, id string, set map[string]string, remove []string) (Executor, error)
	SetEnabled(ctx context.Context, id string, enabled bool) error
	Annotate(ctx context.Context, id string, set map[string]string, remove []string) error
	TouchSeen(ctx context.Context, id string) error
}
