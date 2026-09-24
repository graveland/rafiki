package recalldb

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"go.graveland.dev/rafiki/pkg/recall"
)

// ExtractCursors lists the conversations the extractor must (re)visit:
// messages beyond the last window's ordinal_to, conversations with messages
// but no windows, or any window still on an older extractor version. The
// stale-version windows are deleted here (derived data, rebuilt wholesale),
// so those cursors come back with Tail=nil and extract from scratch.
func (s *Store) ExtractCursors(ctx context.Context, excluded []string, limit int) ([]recall.ExtractCursor, error) {
	// Delete every window of a conversation that carries any window with an
	// older extractor version. Excluded conversations are never touched.
	if _, err := s.pool.Exec(ctx, `DELETE FROM conversations.conversation_window w
		USING conversations.conversation c
		WHERE w.conversation_id = c.id
		  AND c.origin_entrypoint <> ALL($1)
		  AND EXISTS (SELECT 1 FROM conversations.conversation_window v
		              WHERE v.conversation_id = w.conversation_id AND v.extractor_version < $2)`,
		excludedArg(excluded), recall.ExtractorVersion); err != nil {
		return nil, err
	}

	var b sqlb
	b.WriteString(`SELECT c.id::text, coalesce(c.owner_user_id::text, ''), coalesce(c.name, ''), ` + repoBaseSQL + `, c.origin_entrypoint,
			c.created_at, c.updated_at,
			(SELECT max(m.ordinal) FROM conversations.conversation_message m WHERE m.conversation_id = c.id),
			` + lastActivitySQL + `,
			coalesce(` + stoppedSQL + `, false),
			lw.id::text, coalesce(lw.owner_user_id::text, ''), lw.seq, lw.ordinal_from, lw.ordinal_to, lw.text, lw.sealed, lw.extractor_version
		FROM conversations.conversation c
		LEFT JOIN LATERAL (SELECT * FROM conversations.conversation_window w
		                   WHERE w.conversation_id = c.id ORDER BY w.seq DESC LIMIT 1) lw ON TRUE
		WHERE c.origin_entrypoint <> ALL(` + b.arg(excludedArg(excluded)) + `)
		  AND (
		      (lw.id IS NULL AND EXISTS (SELECT 1 FROM conversations.conversation_message m2
		                                 WHERE m2.conversation_id = c.id))
		      OR lw.ordinal_to < (SELECT max(m3.ordinal) FROM conversations.conversation_message m3
		                          WHERE m3.conversation_id = c.id)
		      OR EXISTS (SELECT 1 FROM conversations.conversation_window w2
		                 WHERE w2.conversation_id = c.id AND w2.extractor_version < ` + b.arg(recall.ExtractorVersion) + `)
		  )
		ORDER BY c.updated_at DESC
		LIMIT ` + strconv.Itoa(limit))
	rows, err := s.pool.Query(ctx, b.String(), b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.ExtractCursor
	for rows.Next() {
		var c recall.ExtractCursor
		var repo, entrypoint string
		var maxOrdinal sql.NullInt64
		var updatedAt time.Time
		var winID, winText sql.NullString
		var winOwner string
		var winSeq, winFrom, winTo, winVersion sql.NullInt64
		var sealed sql.NullBool
		if err := rows.Scan(&c.Conversation.ID, &c.Conversation.OwnerUserID, &c.Conversation.Name, &repo, &entrypoint,
			&c.Conversation.CreatedAt, &updatedAt,
			&maxOrdinal, &c.Conversation.LastAt, &c.Conversation.Stopped,
			&winID, &winOwner, &winSeq, &winFrom, &winTo, &winText, &sealed, &winVersion); err != nil {
			return nil, err
		}
		c.Conversation.Repo = repo
		c.Conversation.Kind = kindOf(entrypoint)
		c.Conversation.MaxOrdinal = int(maxOrdinal.Int64)
		if winID.Valid {
			c.Tail = &recall.Window{
				ID:               winID.String,
				ConversationID:   c.Conversation.ID,
				OwnerUserID:      winOwner,
				Seq:              int(winSeq.Int64),
				OrdinalFrom:      int(winFrom.Int64),
				OrdinalTo:        int(winTo.Int64),
				Text:             winText.String,
				Sealed:           sealed.Bool,
				ExtractorVersion: int(winVersion.Int64),
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// excludedArg normalizes an exclusion list for `<> ALL(...)`: a nil slice
// would bind as an untyped NULL and null out the whole predicate.
func excludedArg(excluded []string) []string {
	if excluded == nil {
		return []string{}
	}
	return excluded
}

// MessagesFrom reads a conversation's messages from an ordinal, oldest first.
func (s *Store) MessagesFrom(ctx context.Context, conversationID string, fromOrdinal int) ([]recall.Message, error) {
	uid, ok := parseID(conversationID)
	if !ok {
		return nil, recall.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT m.ordinal, m.role, coalesce(m.kind, ''), m.content, m.created_at
		FROM conversations.conversation_message m
		WHERE m.conversation_id = $1::uuid AND m.ordinal >= $2
		ORDER BY m.ordinal`, uid, fromOrdinal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// WriteWindows upserts one conversation's windows in one transaction, keyed
// on (conversation_id, seq). On update the embedding columns clear only when
// the text changed; sealed, ordinals and updated_at are always stamped.
func (s *Store) WriteWindows(ctx context.Context, conversationID string, ws []recall.Window) error {
	uid, ok := parseID(conversationID)
	if !ok {
		return recall.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, w := range ws {
		if _, err := tx.Exec(ctx, `INSERT INTO conversations.conversation_window
				(conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (conversation_id, seq) DO UPDATE SET
				text = EXCLUDED.text,
				sealed = EXCLUDED.sealed,
				ordinal_from = EXCLUDED.ordinal_from,
				ordinal_to = EXCLUDED.ordinal_to,
				extractor_version = EXCLUDED.extractor_version,
				embedding = CASE WHEN conversation_window.text IS DISTINCT FROM EXCLUDED.text
				                 THEN NULL ELSE conversation_window.embedding END,
				embedding_model = CASE WHEN conversation_window.text IS DISTINCT FROM EXCLUDED.text
				                       THEN NULL ELSE conversation_window.embedding_model END,
				updated_at = now()`,
			uid, nullUUID(w.OwnerUserID), w.Seq, w.OrdinalFrom, w.OrdinalTo, w.Text, w.Sealed, w.ExtractorVersion); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// PendingEmbeds collects rows missing a vector for model: memories (live),
// summaries, and windows that are sealed or whose conversation is stopped.
// Conversation-derived rows exclude the daemon's own machine conversations.
// Each source gets an even share of limit, so one table cannot starve the
// others; sources order their rows oldest first.
func (s *Store) PendingEmbeds(ctx context.Context, model string, limit int) ([]recall.EmbedItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	per := (limit + 2) / 3

	memories, err := s.pendingMemoryEmbeds(ctx, model, per)
	if err != nil {
		return nil, err
	}
	summaries, err := s.pendingSummaryEmbeds(ctx, model, per)
	if err != nil {
		return nil, err
	}
	windows, err := s.pendingWindowEmbeds(ctx, model, per)
	if err != nil {
		return nil, err
	}
	out := make([]recall.EmbedItem, 0, len(memories)+len(summaries)+len(windows))
	out = append(out, memories...)
	out = append(out, summaries...)
	out = append(out, windows...)
	return out, nil
}

func (s *Store) pendingMemoryEmbeds(ctx context.Context, model string, per int) ([]recall.EmbedItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.id::text, m.path::text, m.name, m.body
		FROM conversations.memory m
		WHERE (m.embedding IS NULL OR m.embedding_model IS DISTINCT FROM $1) AND m.deleted_at IS NULL
		ORDER BY m.updated_at
		LIMIT $2`, model, per)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.EmbedItem
	for rows.Next() {
		var it recall.EmbedItem
		var path, name, body string
		if err := rows.Scan(&it.ID, &path, &name, &body); err != nil {
			return nil, err
		}
		it.Source = recall.SourceMemory
		it.Text = path + "/" + name + "\n\n" + body
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) pendingSummaryEmbeds(ctx context.Context, model string, per int) ([]recall.EmbedItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id::text, s.title, s.summary, c.name, `+repoBaseSQL+`, c.origin_entrypoint, c.created_at
		FROM conversations.conversation_summary s
		JOIN conversations.conversation c ON c.id = s.conversation_id
		WHERE (s.embedding IS NULL OR s.embedding_model IS DISTINCT FROM $1)
		  AND c.origin_entrypoint <> ALL($2)
		ORDER BY s.created_at
		LIMIT $3`, model, recall.ExcludedEntrypoints, per)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.EmbedItem
	for rows.Next() {
		var it recall.EmbedItem
		var title, summary, repo, entrypoint string
		var meta recall.ConversationMeta
		if err := rows.Scan(&it.ID, &title, &summary, &meta.Name, &repo, &entrypoint, &meta.CreatedAt); err != nil {
			return nil, err
		}
		meta.Repo = repo
		meta.Kind = kindOf(entrypoint)
		it.Source = recall.SourceSummary
		it.Text = recall.EmbedHeader(meta) + title + "\n\n" + summary
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) pendingWindowEmbeds(ctx context.Context, model string, per int) ([]recall.EmbedItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT w.id::text, w.text, c.name, `+repoBaseSQL+`, c.origin_entrypoint, c.created_at
		FROM conversations.conversation_window w
		JOIN conversations.conversation c ON c.id = w.conversation_id
		WHERE (w.embedding IS NULL OR w.embedding_model IS DISTINCT FROM $1)
		  AND c.origin_entrypoint <> ALL($2)
		  AND (w.sealed OR coalesce(`+stoppedSQL+`, false))
		ORDER BY w.created_at
		LIMIT $3`, model, recall.ExcludedEntrypoints, per)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recall.EmbedItem
	for rows.Next() {
		var it recall.EmbedItem
		var text, repo, entrypoint string
		var meta recall.ConversationMeta
		if err := rows.Scan(&it.ID, &text, &meta.Name, &repo, &entrypoint, &meta.CreatedAt); err != nil {
			return nil, err
		}
		meta.Repo = repo
		meta.Kind = kindOf(entrypoint)
		it.Source = recall.SourceWindow
		it.Text = recall.EmbedHeader(meta) + text
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetEmbeddings writes one transaction of (row, vector) pairs, table chosen
// per item's source. The write is not guarded against concurrent tail
// updates: a window re-extracted mid-embed would get its fresh embedding
// overwritten with a vector of the slightly older text, and stays that way
// until its next text change clears the embedding again. updated_at is not
// comparable here (the embed batch read it before the change), so the race is
// accepted.
func (s *Store) SetEmbeddings(ctx context.Context, model string, items []recall.EmbedItem, vecs [][]float32) error {
	if len(items) != len(vecs) {
		return errors.New("recalldb: embed items and vectors differ in length")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i, it := range items {
		table := ""
		switch it.Source {
		case recall.SourceMemory:
			table = "conversations.memory"
		case recall.SourceWindow:
			table = "conversations.conversation_window"
		case recall.SourceSummary:
			table = "conversations.conversation_summary"
		default:
			return errors.New("recalldb: unknown embed source " + strconv.Quote(string(it.Source)))
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET embedding = $2::vector, embedding_model = $3
			WHERE id = $1::uuid`, it.ID, vecText(vecs[i]), model); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
