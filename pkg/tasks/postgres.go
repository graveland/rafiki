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

	// Lock the (convID, parentID) partition for ordinal allocation.
	// ErrNoRows is fine — no rows to lock means we start at ordinal 1.
	if _, lockErr := tx.Exec(ctx,
		`SELECT 1 FROM conversations.tasks
		  WHERE conversation_id = $1
		    AND parent_id IS NOT DISTINCT FROM $2
		  FOR UPDATE`,
		convID, parentID,
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
		if !ch.Status.Valid() {
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

	// Check for live assignees.
	var assigned string
	err = tx.QueryRow(ctx,
		`SELECT assignee FROM conversations.tasks
		  WHERE id = ANY($1) AND assignee IS NOT NULL AND assignee <> ''
		  LIMIT 1`,
		allIDs,
	).Scan(&assigned)
	if err == nil {
		return nil, fmt.Errorf("%w: %s", ErrAssigned, assigned)
	}
	if err != pgx.ErrNoRows {
		return nil, fmt.Errorf("tasks drop: assignee check: %w", err)
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

// loadAll reads every task row for convID.
func (ps *postgresStore) loadAll(ctx context.Context, convID string) ([]Task, error) {
	rows, err := ps.pool.Query(ctx,
		`SELECT id, conversation_id, parent_id, ordinal, content,
		        active_form, status, drop_reason, assignee, metadata
		   FROM conversations.tasks
		  WHERE conversation_id = $1
		  ORDER BY ordinal`,
		convID,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks loadAll: %w", err)
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
