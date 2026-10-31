// SPDX-License-Identifier: Apache-2.0

package rawtrace_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"

	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// rawTraceTestPool is a scratch database with the schema migrated: the round
// trip under test is JSONB's, so it needs a real JSONB column.
func rawTraceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping DB-backed raw trace test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_rawtrace_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err)
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "migrate")
	return pool
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	ck := assert.NewAborting(t)
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(b)
	ck.NoError(err, "gzip write")
	ck.NoError(w.Close(), "gzip close")
	return buf.Bytes()
}

func zstdBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	ck := assert.NewAborting(t)
	enc, err := zstd.NewWriter(nil)
	ck.NoError(err, "zstd writer")
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// decompress is the transport's rule, mirrored here: the stored body is what
// the compressed wire body decodes to. pkg/llm's tests pin the transport that
// applies it; this test is about what happens to the result in JSONB.
func decompress(t *testing.T, name string, compressed []byte) []byte {
	t.Helper()
	ck := assert.NewAborting(t)
	switch name {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(compressed))
		ck.NoError(err, "gzip reader")
		defer func() { ck.NoError(r.Close(), "gzip close") }()
		out, err := io.ReadAll(r)
		ck.NoError(err, "gzip read")
		return out
	case "zstd":
		dec, err := zstd.NewReader(nil)
		ck.NoError(err, "zstd reader")
		defer dec.Close()
		out, err := dec.DecodeAll(compressed, nil)
		ck.NoError(err, "zstd decode")
		return out
	}
	t.Fatalf("unknown encoding %q", name)
	return nil
}

// canonical normalises a JSON document by value, so a JSONB round trip (which
// reorders keys and normalises whitespace) compares equal.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	ck := assert.NewAborting(t)
	var v any
	ck.NoError(json.Unmarshal(raw, &v), "decode JSON")
	out, err := json.Marshal(v)
	ck.NoError(err, "re-encode JSON")
	return string(out)
}

// TestRawTraceDecompressedBodyDBRoundTrip pins what survives the JSONB column.
// A raw trace's req_body is the request body DECOMPRESSED to the dialect's
// JSON; stored verbatim it must read back as that same JSON object — not as a
// double-encoded JSON string (which is what the store's non-JSON fallback would
// produce for compressed bytes) and not mangled into replacement characters.
func TestRawTraceDecompressedBodyDBRoundTrip(t *testing.T) {
	pool := rawTraceTestPool(t)
	st := rawtrace.NewRawTraceStore(pool)
	ctx := context.Background()

	// Non-ASCII on purpose: a body decoded as the wrong text encoding comes back
	// with U+FFFD, which is exactly what a compressed blob would look like.
	original := []byte(`{"model":"z-ai/glm-5.3-flash","max_tokens":16,` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"héllo — ünïcode ✓"}]}]}`)

	cases := []struct {
		name     string
		compress func(*testing.T, []byte) []byte
	}{
		{"gzip", gzipBytes},
		{"zstd", zstdBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			compressed := tc.compress(t, original)
			c.NotEmpty(compressed, "compressed fixture")
			decompressed := decompress(t, tc.name, compressed)
			c.EqDeep(original, decompressed, "the decompression rule must reproduce the original request JSON")

			// A truncation diagnostic rides the row's Error field, as the capture
			// path records it; it must survive storage unchanged.
			const diagnostic = "request body capture: decompressed body truncated at 8388608 bytes"
			c.NoError(st.Insert(ctx, rawtrace.RawHTTPRequest{
				Source:     "fundi",
				Model:      "z-ai/glm-5.3-flash",
				Upstream:   "openrouter",
				ReqMethod:  "POST",
				ReqPath:    "/v1/messages",
				ReqHeaders: json.RawMessage(`{"Content-Encoding":"` + tc.name + `","Content-Length":"` + fmt.Sprint(len(compressed)) + `"}`),
				ReqBody:    decompressed,
				Error:      diagnostic,
			}), "insert raw trace row")

			var got []byte
			var gotErr *string
			c.Require().NoError(pool.QueryRow(ctx,
				`SELECT req_body, error FROM conversations.raw_http_request
				  WHERE model='z-ai/glm-5.3-flash' ORDER BY created_at DESC LIMIT 1`).Scan(&got, &gotErr),
				"read req_body back")
			c.NotNil(gotErr, "the capture diagnostic must be stored")
			c.Eq(diagnostic, *gotErr, "capture diagnostic")
			c.Require().NotEmpty(got, "req_body")

			// Canonical equality with the original request JSON.
			c.Eq(canonical(t, original), canonical(t, got), "req_body did not round-trip as the original JSON")

			// No replacement characters: a compressed blob stored as text is full
			// of them.
			c.NotStrContains(string(got), "\uFFFD", "req_body contains replacement characters")

			// Not a double-encoded JSON string.
			var asString string
			c.Error(json.Unmarshal(got, &asString), "req_body must be a JSON object, not a JSON string: %s", got)

			var asObject map[string]any
			c.Require().NoError(json.Unmarshal(got, &asObject), "req_body must decode as an object: %s", got)
			c.Eq("z-ai/glm-5.3-flash", asObject["model"], "req_body model")
		})
	}
}
