// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/table"
)

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "user",
		Aliases: []string{"usr"},
		Short:   "Manage rafiki users",
		Long: `Manage the users a rafiki daemon authenticates.

A user is an identity plus a bearer token. The token is the single credential
for BOTH surfaces: the control plane and the LLM proxy face.

A daemon with no users rejects every connection: it logs that fact once at
startup and names ` + "`rafikid user create`" + `, which mints the first user by opening
the database directly on the daemon host.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newUserCreateCmd(), newUserUpdateCmd(), newUserListCmd(), newUserRmCmd())
	return cmd
}

func newUserCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a NON-admin user and print its token once",
		Long: `Create a NON-admin user. By default the daemon decides whether to mint a
bearer token: on a daemon with OIDC login configured the user authenticates
against its IdP and no token is needed; otherwise one is minted so the user
can authenticate at all. --token / --no-token override that decision.

The daemon returns the plaintext token ONCE — it stores only its digest and
cannot reproduce it. The token is written to the current profile's token file
(see ` + "`rafiki profile show`" + `) (mode 0600) unless --no-write is given, so creating a
user also logs this machine in.

This verb never mints an admin: admins come only from
` + "`rafikid user create --admin`" + ` run on the daemon host.`,
		Args: cobra.ExactArgs(1),
		RunE: runUserCreate,
	}
	cmd.Flags().String("email", "", "Optional email for the user")
	cmd.Flags().Bool("token", false, "Mint a bearer token for the new user")
	cmd.Flags().Bool("no-token", false, "Do not mint a bearer token")
	cmd.Flags().Bool("no-write", false, "Print the token but do not write it to the token file")
	return cmd
}

// createMintFlag resolves the tri-state mint decision from the flags: unset
// unless one of --token/--no-token was given, because absent and false mean
// different things on the wire and the flag's zero value must not stand in
// for "the operator chose no token". WHICH flag was named decides — not its
// value, because cobra sets a bool flag's value to true on mere presence
// (--no-token would otherwise read as "mint").
func createMintFlag(cmd *cobra.Command) (*bool, error) {
	tokenChanged := cmd.Flags().Changed("token")
	noTokenChanged := cmd.Flags().Changed("no-token")
	if tokenChanged && noTokenChanged {
		return nil, errors.New("--token and --no-token are mutually exclusive")
	}
	switch {
	case tokenChanged:
		mint := true
		return &mint, nil
	case noTokenChanged:
		mint := false
		return &mint, nil
	default:
		return nil, nil
	}
}

func runUserCreate(cmd *cobra.Command, args []string) error {
	// Resolve the mode and the flag conflict before dialing: a malformed
	// combination (-j -J, --token --no-token) is a user-input error and must
	// not cost a connection (same rule runStatus applies).
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	mint, err := createMintFlag(cmd)
	if err != nil {
		return err
	}
	defer dropUserCompletionCache(cmd)

	email, _ := cmd.Flags().GetString("email")
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().CreateUser(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.CreateUserRequest{Username: args[0], Email: email, MintToken: mint}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}

	p := mustProfile(cmd)
	noWrite, _ := cmd.Flags().GetBool("no-write")
	return renderUserCreate(cmd.OutOrStdout(), cmd.ErrOrStderr(), resp.Msg,
		profile.TokenFile(p.Name), !noWrite, writeTokenFile, mode)
}

// renderUserCreate persists the minted token via writeFn unless shouldWrite
// is false, then prints the credentials. writeFn is injected (rather than
// calling writeTokenFile directly) so tests can exercise a write failure
// without touching the filesystem.
//
// The token is printed to stdout UNCONDITIONALLY, including when writeFn
// fails: it is the daemon's only transmission of the plaintext, so a write
// failure must degrade to "you have to copy it from scrollback yourself,"
// never to "it's gone." Every output mode therefore prints the payload —
// the canonical protojson of the Connect response; JSONL renders it as one
// compact line, every other mode as the pretty re-indent (emitProto's
// contract). No mode reduces it to a table that could lose the token.
//
// stderr explains the mint decision so a scripted operator can tell the four
// states apart: a token minted only because OIDC login is unconfigured, a
// token written to disk, a user who logs in with `rafiki login`, and a user
// who cannot authenticate until someone mints for them.
func renderUserCreate(stdout, stderr io.Writer, resp *rafikiv1.CreateUserResponse, tokenPath string, shouldWrite bool, writeFn func(path, token string) error, mode outputMode) error {
	if resp.GetToken() != "" {
		if shouldWrite {
			if err := writeFn(tokenPath, resp.GetToken()); err != nil {
				fmt.Fprintf(stderr, "warning: could not write %s: %v\n", tokenPath, err)
				fmt.Fprintln(stderr, "save the token below yourself — it cannot be shown again")
			} else {
				fmt.Fprintf(stderr, "token written to %s\n", tokenPath)
			}
		}
		if resp.GetTokenReason() == "oidc not configured" {
			fmt.Fprintln(stderr, "OIDC login is not configured on this daemon, so a token was minted")
		}
	} else if resp.GetLoginConfigured() {
		fmt.Fprintf(stderr, "no token minted; %s logs in with 'rafiki login'\n", resp.GetUsername())
	} else {
		fmt.Fprintf(stderr, "no token minted and OIDC login is not configured on this daemon: %s cannot authenticate until you run 'rafiki token mint --user %s'\n",
			resp.GetUsername(), resp.GetUsername())
	}
	return emitProto(stdout, resp, mode)
}

// writeTokenFile writes token to path with mode 0600, replacing any existing
// content. O_TRUNC matters: a shorter token written over a longer one would
// otherwise leave the old token's tail in the file.
func writeTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// An existing file keeps its old mode through OpenFile, so set it
	// explicitly — a 0644 token file is a credential anyone can read. Close
	// explicitly on every path (no defer) so a write or flush error on Close
	// is never silently dropped behind a deferred second close.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func newUserListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List users (tokens are never shown)",
		Args:  cobra.NoArgs,
		RunE:  runUserList,
	}
	cmd.Flags().Bool("all", false, "Include removed users (history still resolves to them)")
	return cmd
}

// newUserUpdateCmd edits a user's email. The flag is required rather than
// optional-on-the-wire: a request with no optional email set is refused by
// the daemon ("nothing to update"), so there is no legitimate empty call.
// `--email ""` clears the address.
func newUserUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Set a user's email",
		Args:  cobra.ExactArgs(1),
		RunE:  runUserUpdate,
	}
	cmd.Flags().String("email", "", "Email to set (empty clears it)")
	_ = cmd.MarkFlagRequired("email")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeUsers(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runUserUpdate(cmd *cobra.Command, args []string) error {
	defer dropUserCompletionCache(cmd)
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	email, _ := cmd.Flags().GetString("email")
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().UpdateUser(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.UpdateUserRequest{Username: args[0], Email: &email}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}
	return emitUserList(cmd.OutOrStdout(), []*rafikiv1.UserRow{resp.Msg.GetUser()}, mode, useColor)
}

func runUserList(cmd *cobra.Command, _ []string) error {
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	all, _ := cmd.Flags().GetBool("all")
	resp, err := ep.control().ListUsers(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.ListUsersRequest{IncludeDeleted: all}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}
	return emitUserList(cmd.OutOrStdout(), resp.Msg.GetUsers(), mode, useColor)
}

// emitUserList writes the user rows in the resolved mode. -j/-J are the
// canonical protojson via emitProtoRows — the {"rows":[...]} envelope pretty,
// one compact row per line unwrapped in JSONL. A UserRow carries no token
// (tokens are never returned once minted), so there is nothing to redact.
func emitUserList(w io.Writer, rows []*rafikiv1.UserRow, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON, outputJSONL:
		return emitProtoRows(w, rows, mode)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "ID", "USER", "EMAIL", "ADMIN", "CREATED", "REMOVED")...)
		for _, r := range rows {
			tb.Row(r.GetId(), r.GetUsername(), defaultDash(r.GetEmail()), adminCell(r.GetIsAdmin()),
				formatTimestamp(r.GetCreatedAt()), formatTimestamp(r.GetDeletedAt()))
		}
		return tb.Render()
	}
}

func adminCell(admin bool) string {
	if admin {
		return "yes"
	}
	return "-"
}

func newUserRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a user; its token stops working immediately",
		Long: `Remove a user. The row is tombstoned rather than deleted, so every
conversation and turn they authored keeps resolving to their name. The token
stops authenticating at once on the control plane, and within the face's
5-second verification cache.

A daemon with no users left refuses every connection: identity is row-backed,
so nothing can authenticate. Recovery is ` + "`rafikid user create <name> --admin`" + `
run on the daemon host, which opens the database directly.`,
		Args: cobra.ExactArgs(1),
		RunE: runUserRm,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// One target only; past it there is nothing to offer.
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeUsers(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runUserRm(cmd *cobra.Command, args []string) error {
	defer dropUserCompletionCache(cmd)
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	_, err = ep.control().RemoveUser(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.RemoveUserRequest{Username: args[0]}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "removed %s\n", args[0])
	return nil
}

// userConnectErr renders a failed user RPC for the verb's stderr: a thin
// alias of the shared connectVerbErr (see connecterr.go).
func userConnectErr(err error, describe string) error {
	return connectVerbErr(err, describe)
}
