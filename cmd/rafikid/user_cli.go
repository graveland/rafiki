// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/table"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// newUserCmd is the direct-DSN identity CLI: the recovery path for identity
// work when no daemon is reachable — a fresh database, or an operator with DB
// access but no running daemon. It opens the DSN itself (the same way
// 'rafikid migrate' does) and talks to Postgres directly, so it cannot see
// the daemon's oidc.toml and never guesses what the daemon would do.
func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "user",
		Short:        "Manage users directly against the database, bypassing a running daemon",
		SilenceUsage: true,
	}
	// Every verb opens the DSN itself, so one persistent flag covers the
	// whole group.
	var dbDSN string
	def := os.Getenv("RAFIKI_DB")
	if def == "" {
		def = os.Getenv("RAFIKI_TEST_DSN")
	}
	cmd.PersistentFlags().StringVar(&dbDSN, "db", def, "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")
	cmd.AddCommand(newUserCreateCLICmd(&dbDSN), newUserUpdateCLICmd(&dbDSN), newUserTokenCmd(&dbDSN))
	return cmd
}

// openUserStore opens the DSN directly and returns the pool (the caller
// closes it) with the users store over it.
func openUserStore(ctx context.Context, dsn string) (*pgxpool.Pool, users.Store, error) {
	if dsn == "" {
		return nil, nil, errors.New("--db (or RAFIKI_DB) is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return pool, usersdb.NewPostgresStore(pool), nil
}

func newUserCreateCLICmd(dbDSN *string) *cobra.Command {
	var admin bool
	var email string
	var token bool

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a user; mint an initial token only with --token",
		Long: `Creates a user by opening the DSN directly, the same way 'rafikid migrate'
does — no running daemon is contacted. This is the recovery path for a fresh
database or an unreachable daemon; 'rafiki user create' is the normal path
once a daemon is up.

--admin is never inferred from an empty user table: pass it explicitly, or
the created user is an ordinary non-admin user regardless of how many users
already exist.

Without --token the user has no rafiki credential: if the daemon has OIDC
configured it can authenticate with its IdP, otherwise mint a token later
with 'rafikid user token mint'. With --token, the token is printed exactly
once and cannot be recovered afterward.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			pool, store, err := openUserStore(ctx, *dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()
			return runUserCreateCLI(ctx, cmd.OutOrStdout(), store, users.NewUser{
				Username:  args[0],
				Email:     email,
				IsAdmin:   admin,
				MintToken: token,
			})
		},
	}

	cmd.Flags().BoolVar(&admin, "admin", false, "grant admin (never inferred; must be given explicitly)")
	cmd.Flags().StringVar(&email, "email", "", "email address (normalized, unique among active users)")
	cmd.Flags().BoolVar(&token, "token", false, "mint an initial service token and print it once")

	return cmd
}

// runUserCreateCLI is the DSN-agnostic core of `rafikid user create`: an
// already-opened users.Store, rather than a DSN string, so tests can drive it
// against a scratch database without a --db flag round trip through a fresh
// pgxpool.New. A minted token is written to out and nowhere else — it must
// never be logged, since it authenticates both faces of the daemon exactly
// like any other user token.
func runUserCreateCLI(ctx context.Context, out io.Writer, store users.Store, u users.NewUser) error {
	created, token, err := store.Create(ctx, u)
	if err != nil {
		return userCreateErr(err, u.Username, u.Email)
	}
	if token != "" {
		fmt.Fprintf(out, "username: %s\ntoken: %s\n", created.Username, token)
		return nil
	}
	if created.Email != "" {
		fmt.Fprintf(out, "username: %s\nemail: %s\n", created.Username, created.Email)
	} else {
		fmt.Fprintf(out, "username: %s\n", created.Username)
	}
	fmt.Fprintf(out,
		"no token minted; if the daemon has OIDC configured, %s can 'rafiki login'; otherwise run 'rafikid user token mint %s'\n",
		created.Username, created.Username)
	return nil
}

// userCreateErr turns a store refusal into a message naming the offending
// input. A sentinel is an ANSWER — the caller gave bad input; anything else
// is an outage and passes through untouched.
func userCreateErr(err error, username, email string) error {
	switch {
	case errors.Is(err, users.ErrUsernameTaken):
		return fmt.Errorf("user %q already exists", username)
	case errors.Is(err, users.ErrEmailTaken):
		return fmt.Errorf("email %q is already in use by another user", email)
	default:
		return err
	}
}

func newUserUpdateCLICmd(dbDSN *string) *cobra.Command {
	var email string

	cmd := &cobra.Command{
		Use:   "update <name> --email <address>",
		Short: "Set or clear a user's email directly against the database",
		Long: `Updates a user by opening the DSN directly — no running daemon is
contacted. Pass --email '' to clear the address.

There is no --admin here: the admin bit is set only at create, with
'rafikid user create <name> --admin'.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			pool, store, err := openUserStore(ctx, *dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()
			return runUserUpdateCLI(ctx, cmd.OutOrStdout(), store, args[0], email)
		},
	}

	cmd.Flags().StringVar(&email, "email", "", "email address to store (empty clears it)")

	return cmd
}

// runUserUpdateCLI is the DSN-agnostic core of `rafikid user update`.
func runUserUpdateCLI(ctx context.Context, out io.Writer, store users.Store, username, email string) error {
	u, err := store.SetEmail(ctx, username, email)
	if err != nil {
		switch {
		case errors.Is(err, users.ErrNotFound):
			return fmt.Errorf("no user named %q", username)
		case errors.Is(err, users.ErrEmailTaken):
			return fmt.Errorf("email %q is already in use by another user", email)
		default:
			return err
		}
	}
	addr := u.Email
	if addr == "" {
		addr = "(none)"
	}
	fmt.Fprintf(out, "username: %s\nemail: %s\n", u.Username, addr)
	return nil
}

func newUserTokenCmd(dbDSN *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "token",
		Short:        "Mint, list, or revoke a user's service tokens directly against the database",
		SilenceUsage: true,
	}
	cmd.AddCommand(newUserTokenMintCLICmd(dbDSN), newUserTokenListCLICmd(dbDSN), newUserTokenRevokeCLICmd(dbDSN))
	return cmd
}

func newUserTokenMintCLICmd(dbDSN *string) *cobra.Command {
	var name string
	var ttl string

	cmd := &cobra.Command{
		Use:   "mint <name>",
		Short: "Mint a new service token for an existing user and print it once",
		Long: `Mints a service token by opening the DSN directly — no running daemon is
contacted. The plaintext is printed exactly once and cannot be recovered
afterward.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := tokenTTL(ttl)
			if err != nil {
				return err
			}
			if name == "" {
				name = hostTokenName()
			}
			ctx := context.Background()
			pool, store, err := openUserStore(ctx, *dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()
			return runUserTokenMintCLI(ctx, cmd.OutOrStdout(), store, args[0], name, d)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "token name (default \"host <hostname>\")")
	cmd.Flags().StringVar(&ttl, "ttl", "", "token lifetime as a Go duration (e.g. 720h); default never expires")

	return cmd
}

// tokenTTL parses --ttl. Empty means never expires: MintToken's TTL 0 is no
// expiry, so the flag's zero is the store's meaning, not an unset marker.
func tokenTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --ttl %q: %w", s, err)
	}
	return d, nil
}

// hostTokenName is the default token name: which host minted this credential,
// so 'rafikid user token ls' can tell the operator's laptop token from a CI
// one.
func hostTokenName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "host"
	}
	return "host " + h
}

// runUserTokenMintCLI is the DSN-agnostic core of `rafikid user token mint`.
// The plaintext is written to out and nowhere else — it must never be logged.
func runUserTokenMintCLI(ctx context.Context, out io.Writer, store users.Store, username, name string, ttl time.Duration) error {
	id, err := store.LookupUsername(ctx, username)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return fmt.Errorf("no user named %q", username)
		}
		return err
	}
	_, token, err := store.MintToken(ctx, id, users.NewToken{Name: name, Origin: users.OriginService, TTL: ttl})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "token: %s\n", token)
	return nil
}

func newUserTokenListCLICmd(dbDSN *string) *cobra.Command {
	var revoked bool

	cmd := &cobra.Command{
		Use:   "ls [name]",
		Short: "List service tokens — every user's, or one user's",
		Long: `Lists service tokens by opening the DSN directly — no running daemon is
contacted. With a name, only that user's tokens; without, every user's.
Revoked tokens are hidden unless --revoked is passed — revocation is a
filter, never a deletion.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			ctx := context.Background()
			pool, store, err := openUserStore(ctx, *dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()
			return runUserTokenListCLI(ctx, cmd.OutOrStdout(), store, name, revoked)
		},
	}

	cmd.Flags().BoolVar(&revoked, "revoked", false, "include revoked tokens")

	return cmd
}

// runUserTokenListCLI is the DSN-agnostic core of `rafikid user token ls`.
// An empty username means every user's tokens.
func runUserTokenListCLI(ctx context.Context, out io.Writer, store users.Store, username string, includeRevoked bool) error {
	userID := ""
	if username != "" {
		var err error
		userID, err = store.LookupUsername(ctx, username)
		if err != nil {
			if errors.Is(err, users.ErrNotFound) {
				return fmt.Errorf("no user named %q", username)
			}
			return err
		}
	}
	tokens, err := store.ListTokens(ctx, userID, includeRevoked)
	if err != nil {
		return err
	}
	tb := table.New(out, table.Options{})
	tb.Header("ID", "USER", "NAME", "ORIGIN", "CREATED", "EXPIRES", "REVOKED")
	for _, t := range tokens {
		tb.Row(t.ID, t.Username, t.Name, string(t.Origin),
			cliTime(t.CreatedAt), tokenExpiry(t.ExpiresAt), tokenRevoked(t.RevokedAt))
	}
	return tb.Render()
}

// cliTime renders a token timestamp the way the rest of the CLI's tables do:
// local wall-clock date+time.
func cliTime(t time.Time) string { return t.Local().Format(time.DateTime) }

// tokenExpiry renders expires_at; nil means the token never expires.
func tokenExpiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return cliTime(*t)
}

// tokenRevoked renders revoked_at; nil means the token is still active.
func tokenRevoked(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return cliTime(*t)
}

func newUserTokenRevokeCLICmd(dbDSN *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke one service token directly against the database",
		Long: "Revokes a token by opening the DSN directly — no running daemon is\n" +
			"contacted. Takes effect on new requests within 5s; streams already open\n" +
			"on a running daemon are not interrupted — use `rafiki token revoke`\n" +
			"against the daemon for that.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			pool, store, err := openUserStore(ctx, *dbDSN)
			if err != nil {
				return err
			}
			defer pool.Close()
			return runUserTokenRevokeCLI(ctx, cmd.OutOrStdout(), store, args[0])
		},
	}
	return cmd
}

// runUserTokenRevokeCLI is the DSN-agnostic core of `rafikid user token
// revoke`.
func runUserTokenRevokeCLI(ctx context.Context, out io.Writer, store users.Store, id string) error {
	t, err := store.RevokeToken(ctx, id)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return fmt.Errorf("no token with id %q", id)
		}
		return err
	}
	fmt.Fprintf(out, "revoked: %s (user %s, name %q)\n", t.ID, t.Username, t.Name)
	return nil
}
