// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// newUserCmd is the direct-DSN identity CLI: the recovery path for minting a
// user when no daemon is reachable to run `rafiki user create` against (a
// fresh database, or an operator with DB access but no running daemon).
func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "user",
		Short:        "Manage users directly against the database, bypassing a running daemon",
		SilenceUsage: true,
	}
	cmd.AddCommand(newUserCreateCLICmd())
	return cmd
}

func newUserCreateCLICmd() *cobra.Command {
	var dbDSN string
	var admin bool

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a user and print its token once",
		Long: `Creates a user by opening the DSN directly, the same way 'rafikid migrate'
does — no running daemon is contacted. This is the recovery path for a fresh
database or an unreachable daemon; 'rafiki user create' is the normal path
once a daemon is up.

--admin is never inferred from an empty user table: pass it explicitly, or
the created user is an ordinary non-admin user regardless of how many users
already exist.

The token is printed exactly once and cannot be recovered afterward.`,
		Args:          cobra.ExactArgs(1),
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
			return runUserCreateCLI(ctx, cmd.OutOrStdout(), usersdb.NewPostgresStore(pool), args[0], admin)
		},
	}

	def := os.Getenv("RAFIKI_DB")
	if def == "" {
		def = os.Getenv("RAFIKI_TEST_DSN")
	}
	cmd.Flags().StringVar(&dbDSN, "db", def, "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")
	cmd.Flags().BoolVar(&admin, "admin", false, "grant admin (never inferred; must be given explicitly)")

	return cmd
}

// runUserCreateCLI is the DSN-agnostic core of `rafikid user create`: an
// already-opened users.Store, rather than a DSN string, so tests can drive it
// against a scratch database without a --db flag round trip through a fresh
// pgxpool.New. The token is written to out and nowhere else — it must never
// be logged, since it authenticates both faces of the daemon exactly like any
// other user token.
func runUserCreateCLI(ctx context.Context, out io.Writer, store users.Store, name string, admin bool) error {
	u, token, err := store.Create(ctx, users.NewUser{Username: name, IsAdmin: admin, MintToken: true})
	if err != nil {
		if errors.Is(err, users.ErrUsernameTaken) {
			return fmt.Errorf("user %q already exists", name)
		}
		return err
	}
	fmt.Fprintf(out, "username: %s\ntoken: %s\n", u.Username, token)
	return nil
}
