// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/table"
)

func newPythonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "python",
		Aliases: []string{"py"},
		Short:   "Manage your saved pymodules (reusable, owner-scoped Python snippets)",
	}
	cmd.AddCommand(newPythonListCmd(), newPythonGetCmd(), newPythonPutCmd(), newPythonDeleteCmd())
	return cmd
}

func newPythonListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your saved pymodules (code is never printed here)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListPymodules(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListPymodulesRequest{}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitPymoduleList(os.Stdout, resp.Msg.GetRows(), mode, useColor)
		},
	}
}

// emitPymoduleList writes list's output in the resolved mode. The wire rows
// already omit code (the daemon leaves it unset on ListPymodules — an
// inventory is not a document), so every mode can emit the row verbatim and
// none of the corpus rides along.
func emitPymoduleList(w io.Writer, rows []*rafikiv1.PymoduleRow, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, map[string]any{"rows": rows})
	case outputJSONL:
		return writeJSONL(w, pymoduleJSONLRows(rows))
	default:
		return renderPymoduleList(w, rows, useColor)
	}
}

// renderPymoduleList renders the inventory table: NAME, VERSION, SAVED,
// DESCRIPTION — never CODE.
func renderPymoduleList(w io.Writer, rows []*rafikiv1.PymoduleRow, useColor bool) error {
	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "NAME", "VERSION", "SAVED", "DESCRIPTION")...)
	for _, r := range rows {
		name, version, saved, description := pymoduleCells(r)
		tb.Row(name, version, saved, description)
	}
	return tb.Render()
}

// pymoduleCells renders one row's table cells: the name verbatim, the version
// as a plain number, SAVED as "2006-01-02 15:04", and a description that
// falls back to "-" when absent. Extracted so the unit tests pin the
// rendering without a daemon.
func pymoduleCells(r *rafikiv1.PymoduleRow) (name, version, saved, description string) {
	return r.GetName(),
		strconv.FormatInt(r.GetVersion(), 10),
		formatSavedAt(r.GetCreatedAt()),
		defaultDash(r.GetDescription())
}

// formatSavedAt renders a row's RFC3339 createdAt the way the other list
// tables render a timestamp (minutes are enough for a save time): "-" when
// absent, the raw string when it does not parse — an unparseable daemon
// timestamp is displayed, not silently blanked.
func formatSavedAt(s string) string {
	if s == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Format("2006-01-02 15:04")
}

// pymoduleJSONLRows adapts a PymoduleRow slice to writeJSONL's []any.
func pymoduleJSONLRows(rows []*rafikiv1.PymoduleRow) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

func newPythonGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Print one pymodule's code",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().GetPymodule(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.GetPymoduleRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitPymoduleCode(os.Stdout, resp.Msg.GetRow(), mode)
		},
	}
	cmd.ValidArgsFunction = completePyModuleNames
	return cmd
}

// emitPymoduleCode writes a get's output in the resolved mode. Table mode
// prints the code raw, mirroring `skills show` — the code IS the output, so
// it can feed a file or an editor unchanged. JSON modes emit the full row
// (code included) so a script gets version and createdAt alongside.
func emitPymoduleCode(w io.Writer, row *rafikiv1.PymoduleRow, mode outputMode) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, row)
	case outputJSONL:
		return writeJSONL(w, []any{row})
	default:
		fmt.Fprintln(w, row.GetCode())
		return nil
	}
}

func newPythonPutCmd() *cobra.Command {
	var file, description string
	cmd := &cobra.Command{
		Use:   "put <name> --file <path>",
		Short: "Save a new version of a pymodule from a file or stdin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if file == "" {
				return fmt.Errorf("--file is required (--file - reads stdin)")
			}
			var code []byte
			var err error
			if file == "-" {
				code, err = io.ReadAll(os.Stdin)
			} else {
				code, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}
			// Fast, local rejection: the daemon re-checks ValidName, but a
			// name that can never be a path segment should fail before the
			// code is shipped over the wire.
			if err := pymodules.ValidName(name); err != nil {
				return err
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().PutPymodule(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.PutPymoduleRequest{
					Name:        name,
					Code:        string(code),
					Description: description,
				}))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			row := resp.Msg.GetRow()
			if mode == outputJSON || mode == outputJSONL {
				return emitPymoduleCode(os.Stdout, row, mode)
			}
			fmt.Printf("saved %s as version %d\n", row.GetName(), row.GetVersion())
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to the Python source (--file - reads stdin)")
	cmd.Flags().StringVar(&description, "description", "", "one-line description of the module")
	// Completion only ever suggests existing names — put may always create a
	// new one — but it steers the read-modify-write round trip (get, edit,
	// put back under the same name) at the name you already have.
	cmd.ValidArgsFunction = completePyModuleNames
	return cmd
}

func newPythonDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a pymodule (every live version of it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			_, err = ep.control().DeletePymodule(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.DeletePymoduleRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", args[0])
			return nil
		},
	}
	cmd.ValidArgsFunction = completePyModuleNames
	return cmd
}

// completePyModuleNames completes the <name> argument shared by python
// get/delete. The verbs take exactly one name, so past it there is nothing to
// offer. It never exits, never prints, and cannot block past
// completionDeadline: every failure degrades to "no candidates" by design.
func completePyModuleNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmdCtx(cmd), completionDeadline)
	defer cancel()
	resp, err := ep.control().ListPymodules(ctx, connect.NewRequest(&rafikiv1.ListPymodulesRequest{}))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(resp.Msg.GetRows()))
	for _, r := range resp.Msg.GetRows() {
		names = append(names, r.GetName())
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
