// Package tasks defines the task ledger: a durable, hierarchical record of
// what an agent is doing, readable across agent boundaries.
//
// It exists so a coordinator can answer "what is this subagent actually
// doing" with one indexed query instead of replaying a transcript. Rows
// survive a daemon restart, and a dying child's unfinished work is swept to
// StatusOrphaned rather than lost.
//
// This file declares the contract only. Implementations live in memory.go
// (used when no database is configured) and postgres.go; both must satisfy
// the same conformance suite, because a divergence between them is how a
// DB-less unit test passes while production breaks.
package tasks

import (
	"context"
	"errors"
)

// Status is a task's lifecycle state.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	// StatusOrphaned marks work whose owning agent died. The assignee is
	// RETAINED — "c_abc died holding this" is exactly what a coordinator
	// needs, and nulling it destroys that.
	StatusOrphaned Status = "orphaned"
	// StatusDropped marks work its owner decided not to do. Always carries a
	// reason: "decomposed wrong", "turned out unnecessary" and "superseded"
	// are legitimate, quietly giving up is not — and that last case is
	// StatusFailed, not this.
	StatusDropped Status = "dropped"
)

// Terminal reports whether s is an end state a task cannot leave.
//
// StatusOrphaned is deliberately NOT terminal: it exists precisely to be
// reassigned. StatusDropped IS terminal — an agent that changes its mind
// adds a new row, so the dropped row's reason survives as a record of the
// earlier decision.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusDropped
}

// Valid reports whether s is a recognised status.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusInProgress, StatusBlocked,
		StatusCompleted, StatusFailed, StatusOrphaned, StatusDropped:
		return true
	}
	return false
}

// Task is one row of the ledger.
type Task struct {
	ID             string
	ConversationID string
	ParentID       string // "" for a top-level task
	// Ordinal is monotonic per (ConversationID, ParentID) and is NEVER
	// derived from a count of live siblings. A handle freed by a dropped
	// task and then reused would make a coordinator's stale reference
	// silently address a different task.
	Ordinal    int
	Content    string
	ActiveForm string
	Status     Status
	DropReason string
	// Assignee is a childID, not a UUID and not a foreign key:
	// Controller.Spawn returns a childID before the child's engine creates
	// its conversation row, so a FK would make delegation race startup.
	Assignee string
	// Metadata is agent-authored and selectable, never consulted for
	// authority — the same rule executor annotations follow. A label that
	// gates access cannot be asserted by the thing it gates.
	Metadata map[string]string
	// Handle is the dotted ordinal path ("2.1"). Computed on read, never
	// persisted. Tools address rows by handle so the model never carries a
	// UUID across turns.
	Handle string
}

// NewTask is an item being added to the ledger.
type NewTask struct {
	Content    string
	ActiveForm string
	Metadata   map[string]string
}

// Change is a single status transition, addressed by handle.
type Change struct {
	Handle string
	Status Status
}

// ListFilter selects rows. A zero filter matches everything except dropped
// rows, which stay hidden unless IncludeDropped is set.
type ListFilter struct {
	ConversationID string
	Assignee       string
	Status         Status
	Metadata       map[string]string
	IncludeDropped bool
}

// Store is the ledger. Every mutation names its target: there is no
// replace-all and no delete, which is what makes it safe for a coordinator
// and a subagent to write to the same tree.
type Store interface {
	// Add appends items under parentHandle ("" for top level).
	Add(ctx context.Context, convID, parentHandle string, items []NewTask) ([]Task, error)
	// Update applies changes, touching only the handles named.
	Update(ctx context.Context, convID string, changes []Change) ([]Task, error)
	// Drop abandons handle and its subtree. Refuses with ErrAssigned when
	// the task or any descendant has a live assignee, changing nothing.
	Drop(ctx context.Context, convID, handle, reason string) ([]Task, error)
	// List returns matching rows with Handle populated.
	List(ctx context.Context, f ListFilter) ([]Task, error)
	// Assign binds a task to an agent by childID.
	Assign(ctx context.Context, convID, handle, assignee string) (Task, error)
	// OrphanAssigned moves an agent's unfinished work to StatusOrphaned,
	// retaining the assignee. Returns the number of rows affected.
	OrphanAssigned(ctx context.Context, assignee string) (int, error)
}

var (
	ErrNotFound      = errors.New("tasks: no such handle")
	ErrAssigned      = errors.New("tasks: task is assigned to a live agent")
	ErrInvalidStatus = errors.New("tasks: invalid status")
	ErrDropReason    = errors.New("tasks: a drop requires a reason")
)
