// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// syncOIDAbbrev is how many leading hex characters of an object id the
// sync-repo table prints. Twelve is unambiguous in practice and matches what
// the executor logs use; the full oid is always available under -j/-J.
const syncOIDAbbrev = 12

// ─── sync ────────────────────────────────────────────────────────────────────

func newSandboxSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync <executor>:<path> <executor>:<path>",
		Short: "Copy a tree between two executors",
		Long: `Copy a tree (or a single file) from one executor to another.

Each argument names an end as <executor>:<absolute path>: the executor is a
ref or selector the daemon resolves to an executor row, and the path is
absolute on that executor. The source is read and the destination written
through the daemon; the client never dials an executor directly.

--overwrite replaces the destination, and the daemon allows it only when the
destination's executor row says isolation=container. --max-bytes caps the
transfer; an omitted flag means no caller cap, and a supplied value must be
positive.

Table output is one line: "copied <files> files, <bytes> bytes". -o json/-j
and -J print the response's canonical protojson instead.`,
		Args: cobra.ExactArgs(2),
		RunE: runSandboxSync,
	}
	cmd.Flags().Bool("overwrite", false, "Replace the destination (container-isolated destination only)")
	cmd.Flags().Int64("max-bytes", 0, "Cap on the transferred bytes (default: no caller cap)")
	return cmd
}

func runSandboxSync(cmd *cobra.Command, args []string) error {
	// The output mode is a user-input decision (-j and -J together is an
	// error), so resolve it before any round trip.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	src, err := syncEndpointFlag(args[0])
	if err != nil {
		return err
	}
	dst, err := syncEndpointFlag(args[1])
	if err != nil {
		return err
	}

	overwrite, _ := cmd.Flags().GetBool("overwrite")
	req := &rafikiv1.SyncPathRequest{Src: src, Dst: dst, Overwrite: overwrite}
	// An unset --max-bytes means "no caller cap", which is not the same as an
	// explicit 0. Only a CHANGED flag populates the optional field, and a
	// non-positive value is refused here rather than sent (zero meaning
	// "unlimited" is a trap the wire's optional field exists to avoid).
	if cmd.Flags().Changed("max-bytes") {
		maxBytes, _ := cmd.Flags().GetInt64("max-bytes")
		if maxBytes <= 0 {
			return fmt.Errorf("--max-bytes must be > 0")
		}
		req.MaxBytes = &maxBytes
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().SyncPath(cmdCtx(cmd), connect.NewRequest(req))
	if err != nil {
		return connectVerbErr(err, ep.describe)
	}
	if mode == outputTable {
		_, err := fmt.Fprintf(os.Stdout, "copied %d files, %d bytes\n", resp.Msg.GetFiles(), resp.Msg.GetBytes())
		return err
	}
	return emitProto(os.Stdout, resp.Msg, mode)
}

// ─── sync-repo ───────────────────────────────────────────────────────────────

func newSandboxSyncRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-repo <executor>:<repo> <executor>:<repo> <branch>",
		Short: "Move a git branch between two executors",
		Long: `Move a git branch from one executor's repository to another's.

Each endpoint names an executor and the repository directory on it, exactly as
'sandbox sync' does. The branch is moved as a bundle — the source's refs are
read, bundled, relayed and fetched at the destination — so nothing the source
planted (config, hooks) can execute at the destination. It never touches a
working tree.

A checked-out branch and a non-fast-forward update are refused; --force allows
the non-fast-forward rewrite. There is no overwrite: re-seeding an existing
repository is an incremental fetch.

Table output is "created repo" for a freshly created destination, "up to date"
when the branch is already at the source's tip, or "<old>..<new>" (object ids
abbreviated to twelve characters; an empty old prints "(new)"). -o json/-j and
-J print the response's canonical protojson instead.`,
		Args: cobra.ExactArgs(3),
		RunE: runSandboxSyncRepo,
	}
	cmd.Flags().Bool("force", false, "Allow a non-fast-forward update at the destination")
	return cmd
}

func runSandboxSyncRepo(cmd *cobra.Command, args []string) error {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	src, err := syncEndpointFlag(args[0])
	if err != nil {
		return err
	}
	dst, err := syncEndpointFlag(args[1])
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")

	req := &rafikiv1.SyncRepoRequest{Src: src, Dst: dst, Branch: args[2], Force: force}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().SyncRepo(cmdCtx(cmd), connect.NewRequest(req))
	if err != nil {
		return connectVerbErr(err, ep.describe)
	}
	if mode == outputTable {
		return renderSyncRepoResult(os.Stdout, resp.Msg)
	}
	return emitProto(os.Stdout, resp.Msg, mode)
}

// ─── parsing and rendering ───────────────────────────────────────────────────

// syncEndpointFlag parses one "<executor>:<absolute path>" argument into its
// wire endpoint.
func syncEndpointFlag(arg string) (*rafikiv1.SyncEndpoint, error) {
	executor, path, err := splitSyncEndpoint(arg)
	if err != nil {
		return nil, err
	}
	return &rafikiv1.SyncEndpoint{Executor: executor, Path: path}, nil
}

// splitSyncEndpoint splits an endpoint argument at the FIRST ":" — a path may
// itself contain a colon (a Windows-style drive, say), but the executor name
// may not: it must be non-empty and contain no "/". The path part must be
// absolute. Anything else is refused, naming the offending argument.
func splitSyncEndpoint(s string) (executor, path string, err error) {
	executor, path, ok := strings.Cut(s, ":")
	if !ok || executor == "" || strings.Contains(executor, "/") || !strings.HasPrefix(path, "/") {
		return "", "", fmt.Errorf("%q: want <executor>:<absolute path>", s)
	}
	return executor, path, nil
}

// renderSyncRepoResult writes the sync-repo table line. A created repository
// and an up-to-date branch are mutually exclusive at the daemon; the third
// form is the transfer, whose old oid is empty for a branch the destination
// did not have.
func renderSyncRepoResult(w io.Writer, res *rafikiv1.SyncRepoResponse) error {
	var line string
	switch {
	case res.GetCreatedRepo():
		line = "created repo"
	case res.GetUpToDate():
		line = "up to date"
	default:
		old := abbreviateOID(res.GetOldOid())
		if old == "" {
			old = "(new)"
		}
		line = old + ".." + abbreviateOID(res.GetNewOid())
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

// abbreviateOID truncates an object id to syncOIDAbbrev characters; a shorter
// (or empty) value is returned unchanged.
func abbreviateOID(oid string) string {
	if len(oid) <= syncOIDAbbrev {
		return oid
	}
	return oid[:syncOIDAbbrev]
}
