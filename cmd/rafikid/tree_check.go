// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/store"
)

// newTreeCmd is the conversation tree's operator surface. Today it holds one
// verb, the invariant check the tree cutover is judged by.
func newTreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "tree",
		Short:         "Inspect the conversation tree",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newTreeCheckCmd())
	return cmd
}

func newTreeCheckCmd() *cobra.Command {
	var dbDSN string

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify the conversation tree's invariants against the stored history",
		Long: `Checks every conversation's tree against the history it was built from: every
row that is neither a replay's copy nor a duplicate of a root has a path hash,
every node's hash is the hash its position implies, every child sits one below
its parent, every conversation with messages has a head, the ordinal allocator is
ahead of the rows, and every conversation's working path carries the same
messages as the working set it replaced.

duplicate_rows and working_set_loose_match are informational: they count rows
and conversations that are the same message in a different representation, and
neither fails the check.

Exits non-zero when any check fails, so it can gate a cutover.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbDSN == "" {
				return errors.New("--db (or RAFIKI_DB) is required")
			}
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()

			report, err := store.CheckTree(ctx, pool)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "conversations: %d\n", report.Conversations)
			fmt.Fprintf(out, "missing_hash: %d\n", report.MissingHash)
			fmt.Fprintf(out, "duplicate_rows: %d\n", report.DuplicateRows)
			fmt.Fprintf(out, "bad_hash: %d\n", report.BadHash)
			fmt.Fprintf(out, "bad_depth: %d\n", report.BadDepth)
			fmt.Fprintf(out, "missing_head: %d\n", report.MissingHead)
			fmt.Fprintf(out, "ordinal_counter_behind: %d\n", report.OrdinalCounterBehind)
			fmt.Fprintf(out, "working_set_mismatch: %d\n", len(report.WorkingSetMismatch))
			for _, id := range report.WorkingSetMismatch {
				fmt.Fprintf(out, "  mismatch %s\n", id)
			}
			fmt.Fprintf(out, "working_set_loose_match: %d\n", len(report.WorkingSetLooseMatch))
			for _, id := range report.WorkingSetLooseMatch {
				fmt.Fprintf(out, "  loose match %s\n", id)
			}
			if !report.OK() {
				return errors.New("conversation tree check failed")
			}
			return nil
		},
	}

	def := os.Getenv("RAFIKI_DB")
	if def == "" {
		def = os.Getenv("RAFIKI_TEST_DSN")
	}
	cmd.Flags().StringVar(&dbDSN, "db", def, "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")

	return cmd
}
