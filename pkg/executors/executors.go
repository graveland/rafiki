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

	// Connected is a view field: true when the executor currently has a live
	// connection in the pool. It is NOT persisted — the store leaves it false
	// and the controller's ExecutorList marks it from execPool.Live(). It is
	// what lets a client wait for its own session executor to come up before
	// spawning, which is otherwise unobservable from the row alone.
	Connected bool `json:"connected,omitempty"`
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
	// Create mints a row and its durable credential in one step, with no
	// enrollment handshake. It is the STATELESS path: the operator injects the
	// returned credential from a secret store and the executor writes nothing to
	// disk, which is what an immutable or rescheduled deployment needs — an
	// enrolled executor that loses its credential file cannot rejoin, because
	// its enrollment token was consumed.
	//
	// The trade is deliberate and runs the other way from Enroll. Here the
	// operator handles a long-lived secret, and a theft is silent rather than
	// announcing itself by consuming a one-time token. Prefer Enroll where the
	// machine can keep a file.
	Create(ctx context.Context, t NewToken) (Executor, string, error)
	Authenticate(ctx context.Context, credential string) (Executor, error)
	Get(ctx context.Context, id string) (Executor, error)
	List(ctx context.Context) ([]Executor, error)
	SetLabels(ctx context.Context, id string, set map[string]string, remove []string) (Executor, error)
	SetEnabled(ctx context.Context, id string, enabled bool) error
	// Delete permanently removes an executor row. Unlike SetEnabled, this
	// cannot be undone — there is no tombstone. Safe because nothing else
	// keys off an executor id for historical resolution: the enrollment
	// token table's executor_id is ON DELETE SET NULL, and no conversation
	// or child record carries a foreign key to this table.
	Delete(ctx context.Context, id string) error
	Annotate(ctx context.Context, id string, set map[string]string, remove []string) error
	TouchSeen(ctx context.Context, id string) error
}
