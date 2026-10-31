// SPDX-License-Identifier: Apache-2.0

// Package tagsdb is the Postgres store for tags. It owns every SQL statement
// against conversations.agent_tag: a create inserts a row, a move supersedes
// the current row and appends the next, and an untag tombstones the current
// row. Rows are never deleted.
package tagsdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/tags"
)

// ErrNameTaken reports that a live tag already holds the name in that scope.
var ErrNameTaken = errors.New("tagsdb: name taken in that scope")

// ErrNotFound reports that no current live tag matches, including for a
// foreign owner and for an id that never existed.
var ErrNotFound = errors.New("tagsdb: tag not found")

// ErrMoved reports that the tag no longer points at the node the caller
// expected, so a move against a stale view is refused rather than silently
// forking from the wrong node.
type ErrMoved struct {
	Current string
}

func (e ErrMoved) Error() string { return "tagsdb: tag moved; it points at " + e.Current }

// Store is the Postgres tag store.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// querier is the shared surface of a pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const tagColumns = `
       t.tag_id::text, COALESCE(t.owner_user_id::text, ''), t.name, t.scope, t.scope_root,
       t.source_child_id, t.conversation_id::text, t.node_id::text, m.ordinal,
       COALESCE(t.creator_child_id, ''), COALESCE(t.actor_child_id, ''), COALESCE(t.note, ''),
       t.created_at, t.superseded_at, t.deleted_at`

// tagFrom joins the node's message row, which is where the ordinal lives.
const tagFrom = `
  FROM conversations.agent_tag t
  JOIN conversations.conversation_message m ON m.id = t.node_id`

const currentLiveSQL = `
 WHERE t.tag_id = $1::uuid
   AND t.owner_user_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid
   AND t.superseded_at IS NULL AND t.deleted_at IS NULL`

const insertSQL = `
INSERT INTO conversations.agent_tag
    (tag_id, owner_user_id, name, scope, scope_root, source_child_id,
     conversation_id, node_id, creator_child_id, actor_child_id, note)
VALUES ($1::uuid, NULLIF($2, '')::uuid, $3, $4, $5, $6,
        $7::uuid, $8::uuid, NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''))`

// tombstoneLiveNameSQL is Create's replace: the live holder of the name leaves
// agent_tag_live_name's partial index so the replacement can take it.
const tombstoneLiveNameSQL = `
UPDATE conversations.agent_tag
   SET deleted_at = now()
 WHERE owner_user_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
   AND scope = $2 AND scope_root = $3 AND name = $4
   AND superseded_at IS NULL AND deleted_at IS NULL`

// lockCurrentSQL reads the current row and holds it until the transaction
// ends, so two concurrent moves serialize and the loser sees the winner's
// node.
const lockCurrentSQL = `
SELECT node_id::text, name, scope, scope_root,
       COALESCE(creator_child_id, ''), COALESCE(note, '')
  FROM conversations.agent_tag
 WHERE tag_id = $1::uuid
   AND owner_user_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid
   AND superseded_at IS NULL AND deleted_at IS NULL
 FOR UPDATE`

// currentNodeSQL answers where the tag points now, without a lock. It is Move's
// fallback when the locking read found no current row: a concurrent move that
// committed while we waited has superseded the row we meant to lock, so the
// caller must be told the tag moved rather than that it vanished.
const currentNodeSQL = `
SELECT node_id::text
  FROM conversations.agent_tag
 WHERE tag_id = $1::uuid
   AND owner_user_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid
   AND superseded_at IS NULL AND deleted_at IS NULL`

const supersedeSQL = `
UPDATE conversations.agent_tag
   SET superseded_at = now()
 WHERE tag_id = $1::uuid AND superseded_at IS NULL AND deleted_at IS NULL`

const untagSQL = `
UPDATE conversations.agent_tag
   SET deleted_at = now(), actor_child_id = NULLIF($3, '')
 WHERE tag_id = $1::uuid
   AND owner_user_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid
   AND superseded_at IS NULL AND deleted_at IS NULL`

const byNameSQL = `
SELECT` + tagColumns + tagFrom + `
 WHERE t.owner_user_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
   AND t.name = $2
   AND t.superseded_at IS NULL AND t.deleted_at IS NULL
 ORDER BY t.created_at, t.tag_id`

const listSQL = `
SELECT` + tagColumns + tagFrom + `
 WHERE t.owner_user_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
   AND starts_with(t.name, $2)
   AND t.superseded_at IS NULL AND t.deleted_at IS NULL
 ORDER BY t.name, t.tag_id
 LIMIT 2000`

const historySQL = `
SELECT` + tagColumns + tagFrom + `
 WHERE t.owner_user_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
   AND t.tag_id = $2::uuid
 ORDER BY t.created_at, t.id`

// Create mints a tag id and inserts t. With replace, the live holder of the
// same (owner, scope, scope_root, name) is tombstoned first, in the same
// transaction.
func (s *Store) Create(ctx context.Context, t tags.Tag, replace bool) (tags.Tag, error) {
	tagID := uuid.Must(uuid.NewV7()).String()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if replace {
		if _, err := tx.Exec(ctx, tombstoneLiveNameSQL, t.OwnerUserID, t.Scope.String(), t.ScopeRoot, t.Name); err != nil {
			return tags.Tag{}, fmt.Errorf("tagsdb: create %s: %w", t.Name, err)
		}
	}
	if _, err := tx.Exec(ctx, insertSQL, tagID, t.OwnerUserID, t.Name, t.Scope.String(), t.ScopeRoot,
		t.SourceChildID, t.ConversationID, t.NodeID, t.CreatorChildID, t.ActorChildID, t.Note); err != nil {
		if isLiveNameViolation(err) {
			return tags.Tag{}, fmt.Errorf("tagsdb: create %s: %w", t.Name, ErrNameTaken)
		}
		return tags.Tag{}, fmt.Errorf("tagsdb: create %s: %w", t.Name, err)
	}
	got, err := scanCurrent(ctx, tx, t.OwnerUserID, tagID)
	if err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: create %s: %w", t.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: create %s: %w", t.Name, err)
	}
	return got, nil
}

// Move points the tag at a new node, appending a row and superseding the
// current one. expectNodeID is the node the caller believes the tag is on; a
// different current node is ErrMoved, so a stale view never forks from the
// wrong node.
func (s *Store) Move(ctx context.Context, ownerUserID, tagID, expectNodeID string, to tags.Tag) (tags.Tag, error) {
	if !isUUID(tagID) {
		return tags.Tag{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		currentNodeID string
		name, scope   string
		scopeRoot     string
		creatorID     string
		note          string
	)
	err = tx.QueryRow(ctx, lockCurrentSQL, tagID, ownerUserID).Scan(
		&currentNodeID, &name, &scope, &scopeRoot, &creatorID, &note)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the tag is gone, or a move that committed while we waited
		// superseded the row we tried to lock. Tell the caller which.
		var current string
		switch err := tx.QueryRow(ctx, currentNodeSQL, tagID, ownerUserID).Scan(&current); {
		case errors.Is(err, pgx.ErrNoRows):
			return tags.Tag{}, ErrNotFound
		case err != nil:
			return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
		default:
			return tags.Tag{}, ErrMoved{Current: current}
		}
	}
	if err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
	}
	if currentNodeID != expectNodeID {
		return tags.Tag{}, ErrMoved{Current: currentNodeID}
	}
	if _, err := tx.Exec(ctx, supersedeSQL, tagID); err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
	}
	if _, err := tx.Exec(ctx, insertSQL, tagID, ownerUserID, name, scope, scopeRoot,
		to.SourceChildID, to.ConversationID, to.NodeID, creatorID, to.ActorChildID, note); err != nil {
		if isLiveNameViolation(err) {
			return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, ErrNameTaken)
		}
		return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
	}
	got, err := scanCurrent(ctx, tx, ownerUserID, tagID)
	if err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return tags.Tag{}, fmt.Errorf("tagsdb: move %s: %w", tagID, err)
	}
	return got, nil
}

// Untag tombstones the current row. A missing, superseded, deleted or
// foreign-owned tag is ErrNotFound.
func (s *Store) Untag(ctx context.Context, ownerUserID, tagID, actorChildID string) error {
	if !isUUID(tagID) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, untagSQL, tagID, ownerUserID, actorChildID)
	if err != nil {
		return fmt.Errorf("tagsdb: untag %s: %w", tagID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Get returns the current live tag, or ok=false when there is none.
func (s *Store) Get(ctx context.Context, ownerUserID, tagID string) (tags.Tag, bool, error) {
	if !isUUID(tagID) {
		return tags.Tag{}, false, nil
	}
	got, err := scanCurrent(ctx, s.pool, ownerUserID, tagID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tags.Tag{}, false, nil
	}
	if err != nil {
		return tags.Tag{}, false, fmt.Errorf("tagsdb: get %s: %w", tagID, err)
	}
	return got, true, nil
}

// ByName returns every live tag with that name in any scope.
func (s *Store) ByName(ctx context.Context, ownerUserID, name string) ([]tags.Tag, error) {
	rows, err := s.pool.Query(ctx, byNameSQL, ownerUserID, name)
	if err != nil {
		return nil, fmt.Errorf("tagsdb: by name %s: %w", name, err)
	}
	defer rows.Close()
	out, err := scanTags(rows)
	if err != nil {
		return nil, fmt.Errorf("tagsdb: by name %s: %w", name, err)
	}
	return out, nil
}

// List returns live tags whose name starts with namePrefix, in name order.
func (s *Store) List(ctx context.Context, ownerUserID, namePrefix string) ([]tags.Tag, error) {
	rows, err := s.pool.Query(ctx, listSQL, ownerUserID, namePrefix)
	if err != nil {
		return nil, fmt.Errorf("tagsdb: list %q: %w", namePrefix, err)
	}
	defer rows.Close()
	out, err := scanTags(rows)
	if err != nil {
		return nil, fmt.Errorf("tagsdb: list %q: %w", namePrefix, err)
	}
	return out, nil
}

// History returns every version of one tag, oldest first — superseded and
// deleted rows included.
func (s *Store) History(ctx context.Context, ownerUserID, tagID string) ([]tags.Tag, error) {
	if !isUUID(tagID) {
		return []tags.Tag{}, nil
	}
	rows, err := s.pool.Query(ctx, historySQL, ownerUserID, tagID)
	if err != nil {
		return nil, fmt.Errorf("tagsdb: history %s: %w", tagID, err)
	}
	defer rows.Close()
	out, err := scanTags(rows)
	if err != nil {
		return nil, fmt.Errorf("tagsdb: history %s: %w", tagID, err)
	}
	return out, nil
}

func scanCurrent(ctx context.Context, q querier, ownerUserID, tagID string) (tags.Tag, error) {
	row := q.QueryRow(ctx, `SELECT`+tagColumns+tagFrom+currentLiveSQL, tagID, ownerUserID)
	return scanTag(row)
}

func scanTags(rows pgx.Rows) ([]tags.Tag, error) {
	out := make([]tags.Tag, 0, 8)
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTag(row pgx.Row) (tags.Tag, error) {
	var (
		t     tags.Tag
		scope string
	)
	err := row.Scan(&t.TagID, &t.OwnerUserID, &t.Name, &scope, &t.ScopeRoot,
		&t.SourceChildID, &t.ConversationID, &t.NodeID, &t.Ordinal,
		&t.CreatorChildID, &t.ActorChildID, &t.Note,
		&t.CreatedAt, &t.SupersededAt, &t.DeletedAt)
	if err != nil {
		return tags.Tag{}, err
	}
	t.Scope, err = tags.ParseScope(scope)
	if err != nil {
		return tags.Tag{}, err
	}
	return t, nil
}

// isLiveNameViolation reports whether err is the agent_tag_live_name unique
// violation and nothing else. Matching the constraint name as well as the
// SQLSTATE keeps the caller-visible meaning (pick another name) separate from
// every other 23505 on this table, which is a bug.
func isLiveNameViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "agent_tag_live_name"
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
