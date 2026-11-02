// SPDX-License-Identifier: Apache-2.0

package main

// `rafiki mcp login` / `rafiki mcp logout`: the explicit, user-facing half of
// MCP OAuth. The daemon does the flow and holds the tokens; this process only
// ferries the authorize URL to a browser and the redirect query back.
//
// Four rules shape the file:
//
//   - The config file is the DAEMON's, by absolute path. No project file is
//     uploaded and none is read or parsed here: this client resolves a path and
//     the daemon reads that exact file and entry. Resolving the path is why the
//     client does stat ONE candidate — `./.mcp.json`, to decide whether the
//     working directory has one — and that stat is the only local filesystem
//     knowledge involved. A remote profile therefore needs the file to exist on
//     the daemon host, and running from a project directory names that
//     directory's path even when the daemon has no such checkout.
//   - Every message goes to stderr for a login (it leaves a credential behind,
//     not command output) and the logout confirmation goes to stdout (it is
//     the command's result). Neither ever prints a callback query, a code, a
//     token or a client secret — not even in a diagnostic.
//   - The callback values come off the wire (redirect_port, callback_path,
//     redirect_hosts) and are served as given: the daemon fixed them, a busy
//     port is an error with a retry instruction, and the port is never
//     substituted.
//   - One endpoint resolution per command (newConnectEndpoint), so the whole
//     flow lands on the daemon the profile names and the bearer rides the
//     transport.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
)

// mcpLoginPinnedBy is why `rafiki mcp login`'s callback port cannot move. It is
// the parenthetical of a bind failure, the way loginPinnedBy and
// providerLoginPinnedBy are for the other two browser flows.
const mcpLoginPinnedBy = "fixed by rafiki's MCP login callback"

// mcpLoginRetryHint is what a timed-out login tells the user to re-run.
func mcpLoginRetryHint(server string) string {
	return fmt.Sprintf("rerun 'rafiki mcp login %s'", server)
}

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Log in to remote MCP servers that authenticate with OAuth",
		Long: `Authenticate to an MCP server that requires OAuth, and forget that
credential again.

A login is EXPLICIT: it never starts during a spawn, and nothing in a spawn can
start one. The daemon runs the whole flow, stores the resulting grant encrypted
against YOUR user id, and hands back only a consent URL — the access token, the
refresh token and the client secret never leave the daemon, and no CLI process
or child ever holds one of them.

Only a direct ` + "`url`" + ` entry can log in. A stdio server is a local process that
needs no OAuth; a via_executor server runs on an executor's machine, where its
credentials live; an entry that sets its own Authorization header has an
explicit credential that stored OAuth must not override.

The config file is read by the DAEMON, at the absolute path named here
(--mcp-config, else ./.mcp.json, else $RAFIKI_MCP_CONFIG, else the config
directory's mcp.json). With a remote profile that path must exist on the daemon
host: this command never uploads a project file, and it never reads or parses
one — it only stat's ./.mcp.json to decide which of those paths applies.

Subcommands:
  login       start a login for one configured server
  logout      forget a stored grant, by server or by resource URL`,
	}
	cmd.AddCommand(newMCPLoginCmd(), newMCPLogoutCmd())
	return cmd
}

// newMCPLoginCmd builds `rafiki mcp login`.
func newMCPLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login <server>",
		Short: "Log in to a remote MCP server that requires OAuth",
		Long: `Run the OAuth flow for one direct-url entry of an MCP config file and store
the resulting grant against your user id. A running child of yours then spends
it passively: it never prompts, never opens a browser and never discovers the
authorization server itself. A server it cannot reach or authenticate is
skipped with a hint naming this command.

The flow may take more than one consent round (a server can ask for a wider
scope once it sees your tool list). Each round opens a new URL and waits again;
the same login continues, and the grant is stored only when the last round
succeeds.

The callback is served on the port and path the daemon names (1456,
/mcp/oauth/callback, on 127.0.0.1) — that is the redirect URI the server
registered. A port already in use is an error naming the port: free it and
retry. --no-browser opens no browser but still binds that port, and also
accepts the redirect URL pasted back, which is how a browser on another machine
finishes the flow.

Examples:
  rafiki mcp login linear
  rafiki mcp login linear --scope tools:read --scope tools:write
  rafiki mcp login linear --no-browser
  rafiki mcp login linear --mcp-config /srv/app/.mcp.json`,
		Args: cobra.ExactArgs(1),
		RunE: runMCPLogin,
	}
	cmd.Flags().String("mcp-config", "",
		"path to the MCP config file (absolute on the daemon host; default: ./.mcp.json, $RAFIKI_MCP_CONFIG, else the config dir's mcp.json)")
	cmd.Flags().StringArray("scope", nil,
		"request this OAuth scope (repeatable); empty means the scopes the server asks for")
	cmd.Flags().Bool("no-browser", false,
		"do not open a browser; print the authorize URL and also accept the pasted redirect URL")
	return cmd
}

// mcpConfigPath resolves the absolute path this command names, in the documented
// precedence order.
//
// It is resolved HERE and not by the daemon because only the client knows which
// directory the user meant — and it is not resolved further than that: the file
// is never read or parsed here, and it is stat'd only to decide precedence (a
// ./.mcp.json that exists outranks $RAFIKI_MCP_CONFIG and the config-dir file),
// so a path that only exists on the daemon host still works, and a missing one
// is the daemon's refusal to make (it names the path and the problem).
func mcpConfigPath(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve --mcp-config %q: %w", explicit, err)
		}
		return abs, nil
	}
	if cwd, err := os.Getwd(); err == nil {
		local := filepath.Join(cwd, ".mcp.json")
		if st, statErr := os.Stat(local); statErr == nil && !st.IsDir() {
			return local, nil
		}
	}
	// $RAFIKI_MCP_CONFIG, else <ConfigDir>/mcp.json — the same fallback a spawn
	// uses when the working directory has no .mcp.json.
	abs, err := filepath.Abs(paths.GlobalMCPConfig())
	if err != nil {
		return "", fmt.Errorf("resolve the MCP config path: %w", err)
	}
	return abs, nil
}

// normalizeCallbackQuery drops ONE leading "?" from a callback query.
//
// The wire carries the raw query WITHOUT it: the listener hands over RawQuery
// already stripped, but a pasted redirect URL can arrive with its "?" intact,
// and an SDK caller reading "the raw query string" may hand one over too. The
// engine parses the value with url.ParseQuery, which would read "?code=x" as
// the key "?code" and refuse the callback — so the normalisation belongs here,
// once, rather than in every consumer.
func normalizeCallbackQuery(query string) string {
	return strings.TrimPrefix(query, "?")
}

// runMCPLogin drives Begin → (loopback or paste) → Complete over ONE Control
// client, looping while the daemon asks for another consent round.
//
// The listener is inside loopbackCallback, which shuts it down before returning
// on every path: the next round binds the same fixed port, so a listener left
// behind would make round two fail to bind.
func runMCPLogin(cmd *cobra.Command, args []string) error {
	server := args[0]
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	explicitConfig, _ := cmd.Flags().GetString("mcp-config")
	scopes, _ := cmd.Flags().GetStringArray("scope")
	noBrowser, _ := cmd.Flags().GetBool("no-browser")

	configPath, err := mcpConfigPath(explicitConfig)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)
	ctrl := ep.control()
	ui := loginIO{
		noBrowser: noBrowser,
		stdin:     cmd.InOrStdin(),
		stderr:    cmd.ErrOrStderr(),
	}

	begin, err := ctrl.BeginMcpLogin(ctx, connect.NewRequest(&rafikiv1.BeginMcpLoginRequest{
		ConfigPath: configPath,
		Server:     server,
		Scopes:     scopes,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	// Each round needs a human (a browser redirect or a paste), so this loop
	// cannot spin; the daemon decides how many rounds a login needs.
	descriptor := begin.Msg
	for {
		query, err := loopbackCallback(ctx, ui,
			descriptor.GetRedirectHosts(), descriptor.GetRedirectPort(), descriptor.GetCallbackPath(),
			descriptor.GetAuthorizeUrl(), mcpLoginPinnedBy, mcpLoginRetryHint(server))
		if err != nil {
			return err
		}
		query = normalizeCallbackQuery(query)
		complete, err := ctrl.CompleteMcpLogin(ctx, connect.NewRequest(&rafikiv1.CompleteMcpLoginRequest{
			LoginId:       descriptor.GetLoginId(),
			CallbackQuery: query,
		}))
		if err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
		// A continuation is the WHOLE outcome of the round: nothing is stored
		// yet, the same login id carries on, and reporting success here would be
		// a lie the user acts on.
		if continuation := complete.Msg.GetContinuation(); continuation != nil {
			fmt.Fprintln(ui.stderr, "the server asked for more access; continuing the same login")
			descriptor = continuation
			continue
		}
		// The daemon names what it verified; the argument is the fallback for a
		// daemon that did not echo the name.
		name := complete.Msg.GetServer()
		if name == "" {
			name = server
		}
		fmt.Fprintf(ui.stderr, "logged in to %s (resource %s, %d tool(s))\n",
			name, complete.Msg.GetResource(), len(complete.Msg.GetTools()))
		return nil
	}
}

// newMCPLogoutCmd builds `rafiki mcp logout`.
func newMCPLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout [server]",
		Short: "Forget a stored MCP OAuth grant",
		Long: `Remove one stored MCP grant: EVERY grant that belongs to the named server
(an explicit login creates a fresh registration and supersedes the previous
one), or the grant bound to a resource URL.

The two forms are exclusive — a server with --mcp-config, or --resource — and
one of them is required. The server form resolves the same direct-url entry a
login would and invalidates its endpoint, which also fences a first login still
in flight; the resource form touches no config at all, so a grant can be removed
after its project file is gone.

A logout happens locally first and is not undone by a revocation that fails
upstream. A running child of yours then fails its next authenticated MCP request
with the re-login hint. A grant the caller does not have is not_found, whether
or not the entry still exists.

Examples:
  rafiki mcp logout linear
  rafiki mcp logout linear --mcp-config /srv/app/.mcp.json
  rafiki mcp logout --resource https://mcp.example.net/mcp`,
		Args: cobra.MaximumNArgs(1),
		RunE: runMCPLogout,
	}
	cmd.Flags().String("mcp-config", "",
		"path to the MCP config file (absolute on the daemon host); used with a server name")
	cmd.Flags().String("resource", "",
		"canonical MCP resource URL to remove, instead of a configured server")
	return cmd
}

func runMCPLogout(cmd *cobra.Command, args []string) error {
	resource, _ := cmd.Flags().GetString("resource")
	explicitConfig, _ := cmd.Flags().GetString("mcp-config")
	switch {
	case resource != "" && len(args) > 0:
		return fmt.Errorf("give either a server name or --resource, not both")
	case resource == "" && len(args) == 0:
		return fmt.Errorf("give a server name (with --mcp-config if needed) or --resource")
	case resource != "" && explicitConfig != "":
		return fmt.Errorf("--mcp-config applies to the server form; --resource needs no config file")
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	req := &rafikiv1.DeleteMcpCredentialRequest{Resource: resource}
	label := resource
	if resource == "" {
		configPath, err := mcpConfigPath(explicitConfig)
		if err != nil {
			return err
		}
		req.ConfigPath, req.Server = configPath, args[0]
		label = args[0]
	}
	if _, err := ep.control().DeleteMcpCredential(cmdCtx(cmd), connect.NewRequest(req)); err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "logged out of %s\n", label)
	return nil
}
