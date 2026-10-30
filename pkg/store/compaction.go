// SPDX-License-Identifier: Apache-2.0

package store

import (
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/slashcmd"
)

// Kinds written by the compaction boundary path. A compaction appends a
// summary as a new ROOT of the tree, followed by copies of the tail messages
// the summary did not cover, hung under it. They are conv's kinds, so a row
// reader and a tree walk name the same thing.
const (
	KindCompactionSummary = conv.KindCompactionSummary
	KindCompactionTail    = conv.KindCompactionTail
	KindClear             = conv.KindClear
)

// ClearBoundaryText is the content of the synthetic user row a fundi /clear
// opens the fresh working set with. The working set must be non-empty (Continue
// refuses an empty one), and a user turn is what the next prompt merges into at
// request assembly. It lives in slashcmd, a stdlib-only package, so pgx-free
// readers (pkg/recall) can match it.
const ClearBoundaryText = slashcmd.ClearBoundaryText
