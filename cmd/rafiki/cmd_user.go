// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage rafiki users",
		Long: `Manage the users a rafiki daemon authenticates.

A user is an identity plus a bearer token. The token is the single credential
for BOTH surfaces: the control plane and the LLM proxy face.

While a daemon has no users it is in bootstrap mode — ` + "`user create`" + ` is the
only command it accepts, from anyone who can reach it. Create the first user
before the daemon is reachable: run it against a local daemon, or through a
port-forward before ingress is live.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newUserCreateCmd(), newUserListCmd(), newUserRmCmd())
	return cmd
}

func newUserCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a user and print its token once",
		Long: `Create a user. The daemon mints a token, stores only its digest, and
returns the plaintext ONCE — it cannot be shown again. The token is written to
` + "`~/.config/rafiki/token`" + ` (mode 0600) unless --no-write is given, so creating
a user also logs this machine in.`,
		Args: cobra.ExactArgs(1),
		RunE: runUserCreate,
	}
	cmd.Flags().Bool("no-write", false, "Print the token but do not write it to the token file")
	return cmd
}

func runUserCreate(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	resp, err := c.Request(cmdCtx(cmd), protocol.UserCreateRequest{
		Type: protocol.TypeCtrlUserCreate, Username: args[0],
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}

	noWrite, _ := cmd.Flags().GetBool("no-write")
	return renderUserCreate(os.Stdout, os.Stderr, resp, paths.TokenFile(), !noWrite, writeTokenFile)
}

// decodeUserCreate decodes a ctrl_user_create payload. dispatch.go hands this
// straight to okResponse as a bare protocol.UserCreateResponseData
// (okUserCreate → okResponse(protocol.TypeCtrlUserCreate, id, data)) — unlike
// ctrl_user_list, whose rows are wrapped in {"users": [...]}. Read
// pkg/control/dispatch.go before changing this, never infer the shape from a
// sibling verb: that exact mistake once shipped for ctrl_conversation_search.
func decodeUserCreate(resp *protocol.Response) (protocol.UserCreateResponseData, error) {
	var data protocol.UserCreateResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return data, fmt.Errorf("decode response: %w", err)
	}
	return data, nil
}

// renderUserCreate decodes resp and prints the token exactly once, persisting
// it via writeFn unless shouldWrite is false. writeFn is injected (rather than
// calling writeTokenFile directly) so tests can exercise a write failure
// without touching the filesystem.
//
// The token is printed to stdout UNCONDITIONALLY, including when writeFn
// fails: it is the daemon's only transmission of the plaintext, so a write
// failure must degrade to "you have to copy it from scrollback yourself,"
// never to "it's gone."
func renderUserCreate(stdout, stderr io.Writer, resp *protocol.Response, tokenPath string, shouldWrite bool, writeFn func(path, token string) error) error {
	data, err := decodeUserCreate(resp)
	if err != nil {
		return err
	}

	if shouldWrite {
		if err := writeFn(tokenPath, data.Token); err != nil {
			fmt.Fprintf(stderr, "warning: could not write %s: %v\n", tokenPath, err)
			fmt.Fprintln(stderr, "save the token below yourself — it cannot be shown again")
		} else {
			fmt.Fprintf(stderr, "token written to %s\n", tokenPath)
		}
	}

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	fmt.Fprintln(stdout, string(out))
	return nil
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

func runUserList(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	all, _ := cmd.Flags().GetBool("all")
	resp, err := c.Request(cmdCtx(cmd), protocol.UserListRequest{
		Type: protocol.TypeCtrlUserList, IncludeDeleted: all,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return renderUserList(os.Stdout, resp)
}

// decodeUserList decodes a ctrl_user_list payload. dispatch.go WRAPS the rows
// (okUserList → okResponse(protocol.TypeCtrlUserList, id,
// map[string]any{"users": list})) — unlike ctrl_user_create, which sends its
// payload bare. This is the asymmetry documented on decodeUserCreate; getting
// it backwards produces a runtime "cannot unmarshal object/array" that a test
// built from the wrong sibling's shape would not catch.
func decodeUserList(resp *protocol.Response) ([]json.RawMessage, error) {
	var payload struct {
		Users []json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(resp.Data, &payload); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return payload.Users, nil
}

// renderUserList decodes resp and prints the user rows. Tokens are never part
// of this payload (ctrl_user_list never returns them), so there is nothing to
// redact here.
func renderUserList(w io.Writer, resp *protocol.Response) error {
	users, err := decodeUserList(resp)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	fmt.Fprintln(w, string(out))
	return nil
}

func newUserRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a user; its token stops working immediately",
		Long: `Remove a user. The row is tombstoned rather than deleted, so every
conversation and turn they authored keeps resolving to their name. The token
stops authenticating at once on the control plane, and within the face's
5-second verification cache.

Removing the last user returns the daemon to bootstrap mode.`,
		Args: cobra.ExactArgs(1),
		RunE: runUserRm,
	}
}

func runUserRm(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	resp, err := c.Request(cmdCtx(cmd), protocol.UserRmRequest{
		Type: protocol.TypeCtrlUserRm, Username: args[0],
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", args[0])
	return nil
}
