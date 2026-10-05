// SPDX-License-Identifier: Apache-2.0

package main

// The token verbs ride the Connect control plane (MintToken/ListTokens/
// RevokeToken) through newConnectEndpoint, like the user verbs. A user
// manages its own tokens by default; another user's need admin authority on
// the daemon side, and the daemon's refusals surface through userConnectErr.

import (
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage bearer tokens",
		Long: `Mint, list and revoke a user's bearer tokens. A token authenticates BOTH
surfaces — the control plane and the LLM proxy face — and its plaintext is
returned exactly once at mint time; the daemon stores only its digest.

With no --user, the verbs act on the CALLER's own tokens; naming another user
requires admin authority.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newTokenMintCmd(), newTokenLsCmd(), newTokenRevokeCmd())
	return cmd
}

func newTokenMintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mint",
		Short: "Mint a token and print its plaintext once",
		Args:  cobra.NoArgs,
		RunE:  runTokenMint,
	}
	cmd.Flags().String("name", "", "Token name (default: cli <hostname>)")
	cmd.Flags().String("ttl", "", "Time to live, e.g. 720h (default: never expires)")
	cmd.Flags().String("user", "", "User to mint for (default: the caller)")
	return cmd
}

// tokenTTLSeconds parses --ttl into whole seconds; empty means 0 = never
// expires. A POSITIVE duration under a second is refused client-side rather
// than truncated: int64(d/time.Second) would round 500ms down to 0, which on
// the wire means "never expires" — the user asked for a short-lived token and
// would silently get a permanent one. Negative durations are not caught here;
// the daemon answers them with InvalidArgument.
func tokenTTLSeconds(flag string) (int64, error) {
	if flag == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(flag)
	if err != nil {
		return 0, fmt.Errorf("invalid --ttl %q: %w", flag, err)
	}
	if d > 0 && d < time.Second {
		return 0, fmt.Errorf("invalid --ttl %q: must be at least 1s", flag)
	}
	return int64(d / time.Second), nil
}

func runTokenMint(cmd *cobra.Command, _ []string) error {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		host, hostErr := os.Hostname()
		if hostErr != nil || host == "" {
			host = "unknown"
		}
		name = "cli " + host
	}
	ttlFlag, _ := cmd.Flags().GetString("ttl")
	ttlSeconds, err := tokenTTLSeconds(ttlFlag)
	if err != nil {
		return err
	}
	user, _ := cmd.Flags().GetString("user")

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().MintToken(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.MintTokenRequest{Name: name, TtlSeconds: ttlSeconds, Username: user}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}
	return renderTokenMint(cmd.OutOrStdout(), resp.Msg, mode)
}

// renderTokenMint prints the plaintext token exactly once. Table mode prints
// the token and nothing else — it IS the payload, and anything else beside it
// invites it into a pipe. The JSON modes are the canonical protojson of the
// response, which carries the token beside its metadata row.
func renderTokenMint(w io.Writer, resp *rafikiv1.MintTokenResponse, mode outputMode) error {
	if mode == outputJSON || mode == outputJSONL {
		return emitProto(w, resp, mode)
	}
	fmt.Fprintln(w, resp.GetToken())
	return nil
}

func newTokenLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List tokens (secrets are never shown)",
		Args:  cobra.NoArgs,
		RunE:  runTokenLs,
	}
	cmd.Flags().String("user", "", "List another user's tokens (admin)")
	cmd.Flags().Bool("all", false, "List every user's tokens (admin)")
	cmd.Flags().Bool("revoked", false, "Include revoked tokens")
	return cmd
}

func runTokenLs(cmd *cobra.Command, _ []string) error {
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	user, _ := cmd.Flags().GetString("user")
	all, _ := cmd.Flags().GetBool("all")
	revoked, _ := cmd.Flags().GetBool("revoked")

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().ListTokens(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.ListTokensRequest{Username: user, IncludeRevoked: revoked, AllUsers: all}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}
	return emitTokenList(cmd.OutOrStdout(), resp.Msg.GetTokens(), mode, useColor)
}

// emitTokenList writes the token rows in the resolved mode. -j/-J are the
// canonical protojson via emitProtoRows; the table renders metadata only —
// a TokenRow is never its secret.
func emitTokenList(w io.Writer, rows []*rafikiv1.TokenRow, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON, outputJSONL:
		return emitProtoRows(w, rows, mode)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "ID", "USER", "NAME", "ORIGIN", "CREATED", "EXPIRES", "REVOKED")...)
		for _, r := range rows {
			tb.Row(r.GetId(), r.GetUsername(), defaultDash(r.GetName()), defaultDash(r.GetOrigin()),
				formatTimestamp(r.GetCreatedAt()), formatTimestamp(r.GetExpiresAt()), formatTimestamp(r.GetRevokedAt()))
		}
		return tb.Render()
	}
}

func newTokenRevokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token; it stops working immediately",
		Long: `Revoke a token. Revocation is permanent: the token stops authenticating at
once and never revives. List ids with 'rafiki token ls'.`,
		Args: cobra.ExactArgs(1),
		RunE: runTokenRevoke,
	}
	return cmd
}

func runTokenRevoke(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	_, err = ep.control().RevokeToken(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.RevokeTokenRequest{Id: args[0]}))
	if err != nil {
		return userConnectErr(err, ep.describe)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "revoked %s\n", args[0])
	return nil
}
