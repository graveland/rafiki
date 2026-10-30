// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// codecProofSampleCap bounds how many row ids a category records, so a proof
// over a production database names enough to investigate without holding every
// failing id in memory.
const codecProofSampleCap = 20

// codecProofReport is what a codec proof run observed. Rows counts every row
// streamed; DecodeErrors counts rows the codec could not decode or re-encode;
// Unfaithful counts rows whose canonical content changed across the round trip
// even after Normalize; NormalizedOnly counts rows that only agree once
// Normalize has expanded the transport-only shapes. OpaqueByType counts Opaque
// blocks by their JSON "type", which is what a codec gap looks like.
type codecProofReport struct {
	Rows           int
	DecodeErrors   int
	Unfaithful     int
	NormalizedOnly int
	OpaqueByType   map[string]int
	Samples        map[string][]string
}

// beginReadOnly is the single place a proof transaction is opened. It is
// read-only so a proof over production can never write, and so a caller that
// tries to write is refused by the server rather than by convention.
func beginReadOnly(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	return pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
}

// proveCodec streams every stored message through the anthropic codec and
// reports how faithfully each one survives decode-then-encode. conversationIDs
// nil means every conversation.
func proveCodec(ctx context.Context, pool *pgxpool.Pool, conversationIDs []string) (codecProofReport, error) {
	report := codecProofReport{
		OpaqueByType: make(map[string]int),
		Samples:      make(map[string][]string),
	}

	tx, err := beginReadOnly(ctx, pool)
	if err != nil {
		return report, fmt.Errorf("codec proof: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var filter []string
	if conversationIDs != nil {
		filter = conversationIDs
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, role, content
		  FROM conversations.conversation_message
		 WHERE ($1::uuid[] IS NULL OR conversation_id = ANY($1::uuid[]))`, filter)
	if err != nil {
		return report, fmt.Errorf("codec proof: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id   string
			role string
			raw  []byte
		)
		if err := rows.Scan(&id, &role, &raw); err != nil {
			return report, fmt.Errorf("codec proof: scan: %w", err)
		}
		report.Rows++

		decoded, err := msganthropic.Decode(role, raw)
		if err != nil {
			report.DecodeErrors++
			report.sample("decode", id)
			continue
		}
		encoded, err := msganthropic.EncodeContent(decoded)
		if err != nil {
			report.DecodeErrors++
			report.sample("encode", id)
			continue
		}
		report.countOpaque(decoded.Content)

		unfaithful, normalizedOnly, err := compareCodecRoundTrip(role, raw, encoded)
		if err != nil {
			report.DecodeErrors++
			report.sample("canon", id)
			continue
		}
		switch {
		case unfaithful:
			report.Unfaithful++
			report.sample("unfaithful", id)
		case normalizedOnly:
			report.NormalizedOnly++
			report.sample("normalized-only", id)
		}
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("codec proof: stream: %w", err)
	}
	return report, nil
}

// compareCodecRoundTrip decides whether re-encoding the decoded message
// reproduces the stored content. unfaithful is the hard failure: content
// changed even in the relaxed form. normalizedOnly is the soft note: content
// changed only in a transport-only way Normalize forgives.
func compareCodecRoundTrip(role string, raw, encoded []byte) (unfaithful, normalizedOnly bool, err error) {
	rawNormalized, err := msganthropic.Normalize(raw)
	if err != nil {
		return false, false, fmt.Errorf("normalize stored: %w", err)
	}
	encNormalized, err := msganthropic.Normalize(encoded)
	if err != nil {
		return false, false, fmt.Errorf("normalize encoded: %w", err)
	}
	rawRelaxed, err := convtree.Canon(role, rawNormalized)
	if err != nil {
		return false, false, fmt.Errorf("canon stored normalized: %w", err)
	}
	encRelaxed, err := convtree.Canon(role, encNormalized)
	if err != nil {
		return false, false, fmt.Errorf("canon encoded normalized: %w", err)
	}
	if !bytes.Equal(rawRelaxed, encRelaxed) {
		return true, false, nil
	}
	rawStrict, err := convtree.Canon(role, raw)
	if err != nil {
		return false, false, fmt.Errorf("canon stored: %w", err)
	}
	encStrict, err := convtree.Canon(role, encoded)
	if err != nil {
		return false, false, fmt.Errorf("canon encoded: %w", err)
	}
	return false, !bytes.Equal(rawStrict, encStrict), nil
}

// sample records id under category, up to the cap.
func (r *codecProofReport) sample(category, id string) {
	if len(r.Samples[category]) >= codecProofSampleCap {
		return
	}
	r.Samples[category] = append(r.Samples[category], id)
}

// countOpaque tallies every Opaque block in blocks, descending into tool
// results because a block the codec could not model there is just as much a gap.
func (r *codecProofReport) countOpaque(blocks []msg.Block) {
	for _, b := range blocks {
		switch block := b.(type) {
		case msg.Opaque:
			r.OpaqueByType[opaqueType(block.Raw)]++
		case msg.ToolResult:
			r.countOpaque(block.Content)
		}
	}
}

// opaqueType returns the JSON "type" a raw Opaque block declares, or "" when it
// declares none.
func opaqueType(raw json.RawMessage) string {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ""
	}
	return head.Type
}

// codecProofNode builds an append node carrying raw content, which the tests
// seed through conv.Append exactly as capture would.
func codecProofNode(t *testing.T, role, content string) convpkg.NewNode {
	t.Helper()
	if !json.Valid([]byte(content)) {
		t.Fatalf("seed content is not valid JSON: %s", content)
	}
	message, err := msganthropic.Decode(role, []byte(content))
	if err != nil {
		t.Fatalf("decode seed content: %v", err)
	}
	return convpkg.NewNode{Msg: message, Content: []byte(content)}
}

// TestCodecProofOnSeededRows seeds the shapes the codec is most likely to
// mishandle and proves every one survives a round trip.
func TestCodecProofOnSeededRows(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	// t.Context is cancelled before cleanup runs, so cleanup needs its own.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cctx, `UPDATE conversations.conversation SET head_id = NULL WHERE id = $1::uuid`, conv); err != nil {
			t.Logf("cleanup conversation head %s: %v", conv, err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM conversations.conversation_message WHERE conversation_id = $1::uuid`, conv); err != nil {
			t.Logf("cleanup messages %s: %v", conv, err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM conversations.conversation WHERE id = $1::uuid`, conv); err != nil {
			t.Logf("cleanup conversation %s: %v", conv, err)
		}
	})

	nodes := []convpkg.NewNode{
		codecProofNode(t, "user", `[{"type":"tool_result","tool_use_id":"toolu_01","content":"file written"}]`),
		codecProofNode(t, "user", `"hello"`),
		codecProofNode(t, "assistant", `[{"type":"redacted_thinking","data":"b3BhcXVl"}]`),
	}
	res, err := convpkg.NewStore(NewConvBackend(pool), convpkg.Options{}).Open(conv, convpkg.AsOwner()).
		Append(ctx, convpkg.Append{Root: convtree.RootKindRoot, Nodes: nodes})
	c.NoError(err, "append seed")
	c.Eq(3, res.Inserted, "seed inserted three rows")

	report, err := proveCodec(ctx, pool, []string{conv})
	c.NoError(err, "proveCodec")
	t.Logf("codec proof: %+v", report)

	c.Eq(3, report.Rows, "rows streamed")
	c.Eq(0, report.DecodeErrors, "decode errors")
	c.Eq(0, report.Unfaithful, "unfaithful rows")
	c.Eq(1, report.NormalizedOnly, "rows that only agree once normalized")
	c.Eq(1, report.OpaqueByType["redacted_thinking"], "opaque redacted_thinking blocks")
}

// TestCodecProofProduction runs the proof over a real database named by
// RAFIKI_CODEC_PROOF_DSN. It never writes: the read-only transaction refuses it.
func TestCodecProofProduction(t *testing.T) {
	dsn := os.Getenv("RAFIKI_CODEC_PROOF_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_CODEC_PROOF_DSN unset")
	}
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "pool")
	t.Cleanup(pool.Close)

	report, err := proveCodec(ctx, pool, nil)
	c.NoError(err, "proveCodec")
	t.Logf("codec proof: %+v", report)

	if report.DecodeErrors > 0 || report.Unfaithful > 0 {
		t.Errorf("codec proof over %d rows: %d decode errors, %d unfaithful; samples: %+v",
			report.Rows, report.DecodeErrors, report.Unfaithful, report.Samples)
	}
}

// TestCodecProofTxIsReadOnly proves the proof's transaction actually refuses
// writes, which is what makes running it against production safe.
func TestCodecProofTxIsReadOnly(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()

	tx, err := beginReadOnly(ctx, pool)
	c.NoError(err, "begin read-only tx")
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server')`)
	var pgErr *pgconn.PgError
	isPG := errors.As(err, &pgErr)
	c.True(isPG, "insert in a read-only transaction must fail with a Postgres error, got %v", err)
	c.Eq("25006", pgErr.Code, "SQLSTATE read_only_sql_transaction")
}
