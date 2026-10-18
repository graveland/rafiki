// SPDX-License-Identifier: Apache-2.0

// Package skillsdb is the Postgres implementation of skills.Store. It is the
// only package that may touch pgx on the skills path — see pkg/skills.
package skillsdb

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/skills"
)

// NewPostgresStore creates a skills.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) skills.Store { return &pgStore{pool: pool} }

type pgStore struct{ pool *pgxpool.Pool }

// skillsWriteLockKey serialises every skill-set write. The blob sweep is what
// needs it: two concurrent sets can each decide a hash is referenced only by
// rows it is about to delete, and both delete it while the other is inserting a
// skill_file row that points at it. A transaction-scoped lock is released by
// commit or rollback, so a crash cannot leave it held.
const skillsWriteLockKey int64 = 0x736b696c6c73 // "skills"

const selectCols = `s.id::text, s.namespace, s.name, s.description, s.body, s.source,
	COALESCE(s.owner_user_id::text, ''),
	EXISTS (SELECT 1 FROM conversations.skill_file f WHERE f.skill_id = s.id),
	s.enabled, s.created_at, s.updated_at`

func scanRecord(row pgx.Row) (skills.Record, error) {
	var r skills.Record
	err := row.Scan(&r.ID, &r.Namespace, &r.Name, &r.Description, &r.Body,
		&r.Source, &r.OwnerUserID, &r.HasFiles, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (s *pgStore) List(ctx context.Context, enabledOnly bool) ([]skills.Record, error) {
	q := `SELECT ` + selectCols + ` FROM conversations.skills s`
	if enabledOnly {
		q += ` WHERE s.enabled`
	}
	q += ` ORDER BY s.namespace, s.name`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	defer rows.Close()
	var out []skills.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan skill: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get takes an id rather than a name because a name is only unique within a
// layer: core, global and user rows may all carry (rafiki, model-selection), so
// the caller resolves which one it means (skills.Resolve) and asks by id.
//
// The id comparisons in this file are `id::text = $1` rather than `id = $1::uuid`
// so that an id that is not a UUID is ErrNotFound, the answer the caller can act
// on, instead of a driver-level syntax error.
func (s *pgStore) Get(ctx context.Context, id string) (skills.Record, error) {
	r, err := scanRecord(s.pool.QueryRow(ctx,
		`SELECT `+selectCols+` FROM conversations.skills s WHERE s.id::text = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return skills.Record{}, skills.ErrNotFound
	}
	if err != nil {
		return skills.Record{}, fmt.Errorf("get skill: %w", err)
	}
	files, err := s.Files(ctx, []string{id})
	if err != nil {
		return skills.Record{}, err
	}
	r.Files = files[id]
	return r, nil
}

func (s *pgStore) Files(ctx context.Context, ids []string) (map[string][]skills.File, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT f.skill_id::text, f.path, f.mode, b.content
		   FROM conversations.skill_file f
		   JOIN conversations.skill_blob b ON b.sha256 = f.sha256
		  WHERE f.skill_id = ANY($1::uuid[])
		  ORDER BY f.skill_id, f.path`, ids)
	if err != nil {
		return nil, fmt.Errorf("list skill files: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]skills.File, len(ids))
	for rows.Next() {
		var id, path string
		var mode int16
		var content []byte
		if err := rows.Scan(&id, &path, &mode, &content); err != nil {
			return nil, fmt.Errorf("scan skill file: %w", err)
		}
		out[id] = append(out[id], skills.File{Path: path, Mode: uint32(mode), Content: content})
	}
	return out, rows.Err()
}

// fileRef is a stored file's identity without its bytes: what a set comparison
// needs, and all that a rewrite needs to decide whether to happen.
type fileRef struct {
	path string
	mode uint32
	hash [32]byte
}

// storedSkill is one row of the set being replaced, as the comparison sees it.
type storedSkill struct {
	namespace   string
	name        string
	description string
	body        string
	files       []fileRef
}

// setKey is the identity of one skill within a set: the same string the prune
// builds in SQL, so a row is never matched by one and missed by the other.
func setKey(namespace, name string) string { return namespace + ":" + name }

// PutSet writes one set in a single transaction, in the order that keeps the
// content-addressed tables consistent at every intermediate point: blobs first
// (skill_file's foreign key), then rows, then the prune, then the sweep.
func (s *pgStore) PutSet(ctx context.Context, key skills.SetKey, want []skills.Record) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin skill set write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, skillsWriteLockKey); err != nil {
		return false, fmt.Errorf("lock skill set write: %w", err)
	}

	// Validate everything before touching a row: a rejected set must leave the
	// corpus exactly as it was, not half of the new content.
	wantKeys := make([]string, 0, len(want))
	seen := make(map[string]bool, len(want))
	for i := range want {
		w := &want[i]
		if err := skills.ValidateFiles(w.Files); err != nil {
			return false, fmt.Errorf("skill %s: %w", setKey(w.Namespace, w.Name), err)
		}
		// Sorted here so the comparison against the stored list (which the
		// query orders by path) is a straight elementwise walk.
		sort.Slice(w.Files, func(a, b int) bool { return w.Files[a].Path < w.Files[b].Path })
		k := setKey(w.Namespace, w.Name)
		if seen[k] {
			return false, fmt.Errorf("skill %s appears twice in one set", k)
		}
		seen[k] = true
		wantKeys = append(wantKeys, k)
	}

	// An empty OwnerUserID is the global layer, which is a NULL owner in SQL.
	var ownerArg any
	if key.OwnerUserID != "" {
		ownerArg = key.OwnerUserID
	}

	// A namespace belongs to one source per layer. Taking it from another
	// source would silently mix two corpora into a namespace neither owns, so
	// the write is refused with the holder's name — the fact the operator needs
	// to act on.
	namespaces := make([]string, 0, len(want))
	nsSeen := make(map[string]bool, len(want))
	for _, w := range want {
		if !nsSeen[w.Namespace] {
			nsSeen[w.Namespace] = true
			namespaces = append(namespaces, w.Namespace)
		}
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		var holder string
		err := tx.QueryRow(ctx,
			`SELECT source FROM conversations.skills
			  WHERE namespace = $1
			    AND owner_user_id IS NOT DISTINCT FROM $2::uuid
			    AND (source = 'rafiki-core') = ($3 = 'rafiki-core')
			    AND source <> $3
			  ORDER BY source
			  LIMIT 1`, ns, ownerArg, key.Source).Scan(&holder)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("check namespace %s: %w", ns, err)
		}
		return false, &skills.NamespaceHeldError{Namespace: ns, Holder: holder}
	}

	current, err := loadSet(ctx, tx, key, ownerArg)
	if err != nil {
		return false, err
	}
	if sameSet(current, want) {
		// Nothing to write, and nothing for the caller to push: this is what
		// makes a refresh of an unchanged repo free.
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit skill set write: %w", err)
		}
		return false, nil
	}

	for _, w := range want {
		for _, f := range w.Files {
			h := skills.BlobHash(f.Content)
			if _, err := tx.Exec(ctx,
				`INSERT INTO conversations.skill_blob (sha256, content) VALUES ($1, $2)
				 ON CONFLICT DO NOTHING`, h[:], f.Content); err != nil {
				return false, fmt.Errorf("store blob %x: %w", h, err)
			}
		}
	}

	for _, w := range want {
		cur, existed := current[setKey(w.Namespace, w.Name)]
		// A row identical in every stored field is left completely alone: the
		// upsert would rewrite the same values and bump updated_at, so changing
		// one skill in a set would make the whole set look freshly written. The
		// file list is compared here rather than in a SQL WHERE because the
		// WHERE cannot see it, and a file-only change must still be written.
		if existed && cur.description == w.Description && cur.body == w.Body &&
			sameFiles(cur.files, w.Files) {
			continue
		}
		var id string
		// `enabled` is absent from the SET list on purpose: re-syncing content
		// is not permission to turn a skill an operator switched off back on.
		if err := tx.QueryRow(ctx,
			`INSERT INTO conversations.skills
			   (namespace, name, description, body, source, owner_user_id, enabled)
			 VALUES ($1, $2, $3, $4, $5, $6::uuid, TRUE)
			 ON CONFLICT (namespace, name, owner_user_id, (source = 'rafiki-core')) DO UPDATE SET
			   description = EXCLUDED.description,
			   body = EXCLUDED.body,
			   updated_at = now()
			 RETURNING id::text`,
			w.Namespace, w.Name, w.Description, w.Body, key.Source, ownerArg).Scan(&id); err != nil {
			return false, fmt.Errorf("write skill %s: %w", setKey(w.Namespace, w.Name), err)
		}
		if existed && sameFiles(cur.files, w.Files) {
			continue
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM conversations.skill_file WHERE skill_id = $1::uuid`, id); err != nil {
			return false, fmt.Errorf("clear files of skill %s: %w", setKey(w.Namespace, w.Name), err)
		}
		for _, f := range w.Files {
			h := skills.BlobHash(f.Content)
			if _, err := tx.Exec(ctx,
				`INSERT INTO conversations.skill_file (skill_id, path, sha256, mode)
				 VALUES ($1::uuid, $2, $3, $4)`,
				id, f.Path, h[:], int16(f.Mode)); err != nil {
				return false, fmt.Errorf("write file %s of skill %s: %w",
					f.Path, setKey(w.Namespace, w.Name), err)
			}
		}
	}

	// The set's own rows only: another source's rows in the same namespace are
	// a collision caught above, and another owner's are a different set.
	if _, err := tx.Exec(ctx,
		`DELETE FROM conversations.skills
		  WHERE owner_user_id IS NOT DISTINCT FROM $1::uuid
		    AND source = $2
		    AND NOT (namespace || ':' || name = ANY($3))`,
		ownerArg, key.Source, wantKeys); err != nil {
		return false, fmt.Errorf("prune skill set: %w", err)
	}

	// Sweep only the hashes THIS write stopped referencing: a blob another
	// skill still uses survives, and a hash this set never held is not this
	// write's business.
	if stale := staleHashes(current, want); len(stale) > 0 {
		hashes := make([][]byte, 0, len(stale))
		for _, h := range stale {
			hashes = append(hashes, h[:])
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM conversations.skill_blob b
			  WHERE b.sha256 = ANY($1::bytea[])
			    AND NOT EXISTS (SELECT 1 FROM conversations.skill_file f WHERE f.sha256 = b.sha256)`,
			hashes); err != nil {
			return false, fmt.Errorf("sweep skill blobs: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit skill set write: %w", err)
	}
	return true, nil
}

func (s *pgStore) SetEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.skills SET enabled = $2, updated_at = now() WHERE id::text = $1`,
		id, enabled)
	if err != nil {
		return fmt.Errorf("set skill enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return skills.ErrNotFound
	}
	return nil
}

// Delete removes one row and sweeps the blobs that row's files were the last
// reference to. Both halves are one transaction, because the row's skill_file
// rows go by cascade: once the row is gone the hashes are unrecoverable, so
// they are captured first.
//
// It takes the same advisory lock PutSet does. Without it, a delete's sweep and
// a concurrent set write's re-insert of the same hash can interleave — the
// sweep does not see the other transaction's uncommitted skill_file row, so it
// deletes a blob the other is about to reference (a foreign-key failure), or
// strands a blob forever when nothing sweeps it.
func (s *pgStore) Delete(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin skill delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, skillsWriteLockKey); err != nil {
		return fmt.Errorf("lock skill delete: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT sha256 FROM conversations.skill_file WHERE skill_id::text = $1`, id)
	if err != nil {
		return fmt.Errorf("load skill file hashes: %w", err)
	}
	var hashes [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("scan skill file hash: %w", err)
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM conversations.skills WHERE id::text = $1`, id)
	if err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return skills.ErrNotFound
	}

	// The same restricted sweep PutSet runs: only the hashes this skill named,
	// and only those no remaining skill references.
	if len(hashes) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM conversations.skill_blob b
			  WHERE b.sha256 = ANY($1::bytea[])
			    AND NOT EXISTS (SELECT 1 FROM conversations.skill_file f WHERE f.sha256 = b.sha256)`,
			hashes); err != nil {
			return fmt.Errorf("sweep skill blobs: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit skill delete: %w", err)
	}
	return nil
}

// loadSet reads the set's current rows and their (path, mode, hash) lists, the
// "before" side of both the no-op comparison and the blob sweep.
func loadSet(ctx context.Context, tx pgx.Tx, key skills.SetKey, ownerArg any) (map[string]storedSkill, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, namespace, name, description, body
		   FROM conversations.skills
		  WHERE owner_user_id IS NOT DISTINCT FROM $1::uuid AND source = $2`,
		ownerArg, key.Source)
	if err != nil {
		return nil, fmt.Errorf("load skill set: %w", err)
	}
	defer rows.Close()

	out := make(map[string]storedSkill)
	keyByID := make(map[string]string)
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		var st storedSkill
		if err := rows.Scan(&id, &st.namespace, &st.name, &st.description, &st.body); err != nil {
			return nil, fmt.Errorf("scan skill set: %w", err)
		}
		k := setKey(st.namespace, st.name)
		out[k] = st
		keyByID[id] = k
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}

	frows, err := tx.Query(ctx,
		`SELECT f.skill_id::text, f.path, f.mode, f.sha256
		   FROM conversations.skill_file f
		  WHERE f.skill_id = ANY($1::uuid[])
		  ORDER BY f.skill_id, f.path`, ids)
	if err != nil {
		return nil, fmt.Errorf("load skill set files: %w", err)
	}
	defer frows.Close()
	for frows.Next() {
		var id, path string
		var mode int16
		var sha []byte
		if err := frows.Scan(&id, &path, &mode, &sha); err != nil {
			return nil, fmt.Errorf("scan skill set file: %w", err)
		}
		k, ok := keyByID[id]
		if !ok {
			continue
		}
		st := out[k]
		var hash [32]byte
		copy(hash[:], sha)
		st.files = append(st.files, fileRef{path: path, mode: uint32(mode), hash: hash})
		out[k] = st
	}
	return out, frows.Err()
}

// sameSet reports whether want would leave the corpus byte-identical: the same
// names, the same description and body, and the same file path/mode/hash lists.
func sameSet(current map[string]storedSkill, want []skills.Record) bool {
	if len(current) != len(want) {
		return false
	}
	for _, w := range want {
		cur, ok := current[setKey(w.Namespace, w.Name)]
		if !ok {
			return false
		}
		if cur.description != w.Description || cur.body != w.Body || !sameFiles(cur.files, w.Files) {
			return false
		}
	}
	return true
}

// sameFiles compares a stored list against a want list; both are ordered by
// path (the query's ORDER BY, and PutSet's sort).
func sameFiles(stored []fileRef, want []skills.File) bool {
	if len(stored) != len(want) {
		return false
	}
	for i, f := range want {
		if stored[i].path != f.Path || stored[i].mode != f.Mode || stored[i].hash != skills.BlobHash(f.Content) {
			return false
		}
	}
	return true
}

// staleHashes is the hashes the set referenced before this write and no longer
// references.
func staleHashes(current map[string]storedSkill, want []skills.Record) [][32]byte {
	after := make(map[[32]byte]bool)
	for _, w := range want {
		for _, f := range w.Files {
			after[skills.BlobHash(f.Content)] = true
		}
	}
	seen := make(map[[32]byte]bool)
	var out [][32]byte
	for _, cur := range current {
		for _, f := range cur.files {
			if after[f.hash] || seen[f.hash] {
				continue
			}
			seen[f.hash] = true
			out = append(out, f.hash)
		}
	}
	return out
}
