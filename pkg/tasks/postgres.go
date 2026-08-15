package tasks

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPostgresStore returns a Store backed by the conversations.tasks table.
func NewPostgresStore(pool *pgxpool.Pool) Store {
	return &postgresStore{pool: pool}
}

type postgresStore struct {
	pool *pgxpool.Pool
}

func (ps *postgresStore) Add(ctx context.Context, convID, parentHandle string, items []NewTask) ([]Task, error) {
	tx, err := ps.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks add: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve parent ID if a handle was given.
	var parentID *string
	if parentHandle != "" {
		p, err := ps.resolveInTx(ctx, tx, convID, parentHandle)
		if err != nil {
			return nil, err
		}
		parentID = &p.ID
	}

	// Serialize ordinal allocation for this (conversation, parent) partition.
	//
	// A row lock cannot do this job: SELECT … FOR UPDATE over an EMPTY
	// partition locks nothing, so two concurrent first-adds both read
	// MAX(ordinal)=0 and both insert ordinal 1. A transaction-scoped advisory
	// lock keyed on the partition has no such hole and is released on commit
	// or rollback without any explicit unlock.
	partitionKey := convID + "|"
	if parentID != nil {
		partitionKey += *parentID
	}
	if _, lockErr := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		partitionKey,
	); lockErr != nil {
		return nil, fmt.Errorf("tasks add: lock: %w", lockErr)
	}

	// Compute the next ordinal. MUST use MAX, never COUNT.
	var nextOrdinal int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(ordinal), 0) + 1
		   FROM conversations.tasks
		  WHERE conversation_id = $1
		    AND parent_id IS NOT DISTINCT FROM $2`,
		convID, parentID,
	).Scan(&nextOrdinal)
	if err != nil {
		return nil, fmt.Errorf("tasks add: ordinal: %w", err)
	}

	// Insert with sequential ordinals.
	addedIDs := make(map[string]bool, len(items))
	for i, item := range items {
		ord := nextOrdinal + i
		id := uuid.Must(uuid.NewV7()).String()
		addedIDs[id] = true

		md := item.Metadata
		if md == nil {
			md = map[string]string{}
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO conversations.tasks
				(id, conversation_id, parent_id, ordinal, content, active_form, status, metadata)
			 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)`,
			id, convID, parentID, ord, item.Content, item.ActiveForm, md,
		)
		if err != nil {
			return nil, fmt.Errorf("tasks add: insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tasks add: commit: %w", err)
	}

	// Load full tree to compute handles.
	all, err := ps.loadAll(ctx, convID)
	if err != nil {
		return nil, err
	}
	all = AssignHandles(all)

	// Return only added tasks.
	var out []Task
	for _, t := range all {
		if addedIDs[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

func (ps *postgresStore) Update(ctx context.Context, convID string, changes []Change) ([]Task, error) {
	tx, err := ps.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks update: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, ch := range changes {
		if !ch.Status.UserSettable() {
			return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, ch.Status)
		}
		t, err := ps.resolveInTx(ctx, tx, convID, ch.Handle)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx,
			`UPDATE conversations.tasks
			    SET status = $1, updated_at = now()
			  WHERE id = $2`,
			string(ch.Status), t.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("tasks update: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tasks update: commit: %w", err)
	}

	all, err := ps.loadAll(ctx, convID)
	if err != nil {
		return nil, err
	}
	return AssignHandles(all), nil
}

func (ps *postgresStore) Drop(ctx context.Context, convID, handle, reason string) ([]Task, error) {
	if reason == "" {
		return nil, ErrDropReason
	}

	tx, err := ps.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks drop: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t, err := ps.resolveInTx(ctx, tx, convID, handle)
	if err != nil {
		return nil, err
	}

	// Collect subtree IDs.
	subtreeIDs, err := ps.descendantIDs(ctx, tx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("tasks drop: descendants: %w", err)
	}
	allIDs := append([]string{t.ID}, subtreeIDs...)

	// Check for live assignees — under a lock covering EVERY row the UPDATE
	// below will touch.
	//
	// The unlocked version of this check was a TOCTOU: a plain SELECT reads a
	// snapshot, and the UPDATE re-filters on id and status only, never
	// re-reading the assignee. Under READ COMMITTED an Assign committing
	// between the two dropped a task a live agent was holding — precisely what
	// the Store contract says cannot happen.
	//
	// Three things about the shape:
	//
	//   - No `assignee IS NOT NULL` predicate. FOR UPDATE locks only the rows
	//     it returns, so filtering here would lock the rows that already have
	//     an assignee and leave every currently-unassigned row — the ones an
	//     Assign is actually racing for — free. The filtering moves into Go.
	//   - The whole subtree via allIDs, not just the target: the UPDATE
	//     cascades, so a descendant is equally capable of being assigned.
	//   - ORDER BY id gives concurrent Drops over overlapping subtrees a
	//     consistent lock order. Add takes no row locks (it only inserts,
	//     under an advisory lock Drop never touches) and Assign takes exactly
	//     one and waits on nothing while holding it, so neither can complete
	//     a cycle with this.
	lockedRows, err := tx.Query(ctx,
		`SELECT assignee FROM conversations.tasks
		  WHERE id = ANY($1)
		  ORDER BY id
		    FOR UPDATE`,
		allIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks drop: assignee check: %w", err)
	}
	assignees, err := pgx.CollectRows(lockedRows, pgx.RowTo[*string])
	if err != nil {
		return nil, fmt.Errorf("tasks drop: assignee check: %w", err)
	}
	for _, a := range assignees {
		if a != nil && *a != "" {
			return nil, fmt.Errorf("%w: %s", ErrAssigned, *a)
		}
	}

	// Drop non-terminal nodes.
	_, err = tx.Exec(ctx,
		`UPDATE conversations.tasks
		    SET status = 'dropped', drop_reason = $2, updated_at = now()
		  WHERE id = ANY($1)
		    AND status NOT IN ('completed','failed','dropped')`,
		allIDs, reason,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks drop: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tasks drop: commit: %w", err)
	}

	all, err := ps.loadAll(ctx, convID)
	if err != nil {
		return nil, err
	}
	return AssignHandles(all), nil
}

func (ps *postgresStore) List(ctx context.Context, f ListFilter) ([]Task, error) {
	all, err := ps.loadAll(ctx, f.ConversationID)
	if err != nil {
		return nil, err
	}
	return FilterTasks(AssignHandles(all), f), nil
}

func (ps *postgresStore) Assign(ctx context.Context, convID, handle, assignee string) (Task, error) {
	tx, err := ps.pool.Begin(ctx)
	if err != nil {
		return Task{}, fmt.Errorf("tasks assign: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	t, err := ps.resolveInTx(ctx, tx, convID, handle)
	if err != nil {
		return Task{}, err
	}

	_, err = tx.Exec(ctx,
		`UPDATE conversations.tasks
		    SET assignee = $1, updated_at = now()
		  WHERE id = $2`,
		assignee, t.ID,
	)
	if err != nil {
		return Task{}, fmt.Errorf("tasks assign: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Task{}, fmt.Errorf("tasks assign: commit: %w", err)
	}

	t.Assignee = assignee
	return *t, nil
}

func (ps *postgresStore) OrphanAssigned(ctx context.Context, assignee string) (int, error) {
	// Guarded here as well as in the memory store, so the two agree by
	// contract rather than by accident. `assignee = ''` happens to match
	// nothing today only because unassigned rows are NULL; a future NOT NULL
	// DEFAULT '' would silently turn this into the memory store's data loss.
	if assignee == "" {
		return 0, nil
	}

	tag, err := ps.pool.Exec(ctx,
		`UPDATE conversations.tasks
		    SET status = 'orphaned', updated_at = now()
		  WHERE assignee = $1
		    AND status IN ('pending','in_progress','blocked')`,
		assignee,
	)
	if err != nil {
		return 0, fmt.Errorf("tasks orphan: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// loadAll reads every task row for convID, or every task row in the ledger
// when convID is empty (the ctrl_task_list case).
//
// The empty case is a separate statement rather than a guarded predicate:
// conversation_id is UUID NOT NULL, and any expression that casts "" to uuid
// fails at parse time regardless of whether the guard would short-circuit.
func (ps *postgresStore) loadAll(ctx context.Context, convID string) ([]Task, error) {
	const cols = `id, conversation_id, parent_id, ordinal, content,
	              active_form, status, drop_reason, assignee, metadata`

	var rows pgx.Rows
	var err error
	if convID == "" {
		rows, err = ps.pool.Query(ctx,
			`SELECT `+cols+`
			   FROM conversations.tasks
			  ORDER BY conversation_id, ordinal`)
	} else {
		rows, err = ps.pool.Query(ctx,
			`SELECT `+cols+`
			   FROM conversations.tasks
			  WHERE conversation_id = $1
			  ORDER BY ordinal`, convID)
	}
	if err != nil {
		return nil, fmt.Errorf("tasks loadAll: query: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var parentID, dropReason, assignee *string
		if err := rows.Scan(
			&t.ID, &t.ConversationID, &parentID, &t.Ordinal,
			&t.Content, &t.ActiveForm, &t.Status,
			&dropReason, &assignee, &t.Metadata,
		); err != nil {
			return nil, fmt.Errorf("tasks loadAll: scan: %w", err)
		}
		if parentID != nil {
			t.ParentID = *parentID
		}
		if dropReason != nil {
			t.DropReason = *dropReason
		}
		if assignee != nil {
			t.Assignee = *assignee
		}
		if t.Metadata == nil {
			t.Metadata = map[string]string{}
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tasks loadAll: rows: %w", err)
	}
	return out, nil
}

// resolveInTx finds a task by handle within a transaction.
func (ps *postgresStore) resolveInTx(ctx context.Context, tx pgx.Tx, convID, handle string) (*Task, error) {
	all, err := ps.loadAllInTx(ctx, tx, convID)
	if err != nil {
		return nil, err
	}
	all = AssignHandles(all)
	for i := range all {
		if all[i].Handle == handle {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, handle)
}

// loadAllInTx reads all tasks for convID within a transaction.
func (ps *postgresStore) loadAllInTx(ctx context.Context, tx pgx.Tx, convID string) ([]Task, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, conversation_id, parent_id, ordinal, content,
		        active_form, status, drop_reason, assignee, metadata
		   FROM conversations.tasks
		  WHERE conversation_id = $1
		  ORDER BY ordinal`,
		convID,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks loadAllInTx: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		var parentID, dropReason, assignee *string
		if err := rows.Scan(
			&t.ID, &t.ConversationID, &parentID, &t.Ordinal,
			&t.Content, &t.ActiveForm, &t.Status,
			&dropReason, &assignee, &t.Metadata,
		); err != nil {
			return nil, fmt.Errorf("tasks loadAllInTx: scan: %w", err)
		}
		if parentID != nil {
			t.ParentID = *parentID
		}
		if dropReason != nil {
			t.DropReason = *dropReason
		}
		if assignee != nil {
			t.Assignee = *assignee
		}
		if t.Metadata == nil {
			t.Metadata = map[string]string{}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// descendantIDs returns the IDs of all descendant tasks of rootID.
func (ps *postgresStore) descendantIDs(ctx context.Context, tx pgx.Tx, rootID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`WITH RECURSIVE subtree AS (
			SELECT id FROM conversations.tasks WHERE parent_id = $1
			UNION ALL
			SELECT t.id FROM conversations.tasks t
			JOIN subtree s ON t.parent_id = s.id
		 )
		 SELECT id FROM subtree`,
		rootID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
