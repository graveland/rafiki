// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the client and daemon versions",
		Long: "Show the client and daemon versions.\n" +
			"\n" +
			"The daemon's version comes from the control plane (Status), so this\n" +
			"command dials the profile's daemon — a client/daemon build mismatch\n" +
			"shows up right here. `rafiki --version` prints the client's version\n" +
			"only and never dials.",
		Args: cobra.NoArgs,
		RunE: runVersion,
	}
}

// runVersion prints this binary's version, then asks the daemon for its own
// over the Connect control plane — Status's version field, which the daemon
// fills from its own version.String(). One command answers "are the client
// and the daemon the same build?", the question a version verb exists for in
// a two-binary system.
//
// The client line is written BEFORE the dial, so a dead daemon still leaves
// the client's version on stdout and the failure on stderr (main's RunE
// path, exit 1) — the same split every verb uses, applied to a command whose
// first half cannot fail. `rafiki --version` remains the dial-free form for
// scripting the client's version alone.
//
// The output mode is deliberately ignored: version is a text diagnostic with
// no proto payload to render (the client's own version is not Connect data),
// so there is no JSON form to join. outputOpts is still consulted so `-j -J`
// stays the user-input error it is on every other verb, and so --color
// resolves the same way.
func runVersion(cmd *cobra.Command, _ []string) error {
	_, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	emitVersionLine(cmd.OutOrStdout(), "client", version.String(), useColor)

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().Status(cmdCtx(cmd), connect.NewRequest(&rafikiv1.StatusRequest{}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	emitVersionLine(cmd.OutOrStdout(), "server", resp.Msg.GetVersion(), useColor)
	return nil
}

// emitVersionLine writes one "key: value" line, the key dimmed under color —
// the same block style `rafiki status` renders. An empty value (a daemon that
// reported no version) reads "unknown" rather than a blank.
func emitVersionLine(w io.Writer, key, value string, useColor bool) {
	if value == "" {
		value = "unknown"
	}
	k := key + ":"
	if useColor {
		k = dim(k)
	}
	fmt.Fprintf(w, "%s %s\n", k, value)
}
