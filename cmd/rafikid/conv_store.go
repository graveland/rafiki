// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/store"
)

// convCacheBytesDefault is the budget an unset RAFIKI_CONV_CACHE_BYTES gets:
// 256 MiB, the same default conv.NewStore applies to a zero CacheBytes.
const convCacheBytesDefault = 256 << 20

// convCacheBytesEnv names the budget. The name lives here rather than in
// pkg/paths because it is the daemon's alone: no child process, no client and
// no library reads it.
const convCacheBytesEnv = "RAFIKI_CONV_CACHE_BYTES"

// convCacheBytes reads the per-daemon conversation node cache budget. Unset (or
// empty) is the default. Anything else must be a positive integer: a typo in a
// memory budget refuses startup instead of silently becoming the default, which
// is the only way the operator ever learns the setting did nothing.
func convCacheBytes() (int64, error) {
	v := os.Getenv(convCacheBytesEnv)
	if v == "" {
		return convCacheBytesDefault, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", convCacheBytesEnv, v)
	}
	return n, nil
}

// newConvStore builds the daemon's ONE conversation store: the node cache every
// in-daemon reader and writer of conversation history shares. Without it each
// llm.Client, each capture store and each engine would build a private cache
// over the same rows, so a node one face appended is a miss for the next.
func newConvStore(pool *pgxpool.Pool) (*conv.Store, error) {
	n, err := convCacheBytes()
	if err != nil {
		return nil, err
	}
	return conv.NewStore(store.NewConvBackend(pool), conv.Options{CacheBytes: n}), nil
}
