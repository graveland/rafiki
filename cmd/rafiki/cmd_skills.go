// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/skills"
	"go.graveland.dev/rafiki/pkg/table"
)

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage the daemon's skill corpus",
	}
	cmd.AddCommand(
		newSkillsListCmd(),
		newSkillsShowCmd(),
		newSkillsRmCmd(),
	)
	cmd.AddCommand(newSkillsEnableCmds()...)
	return cmd
}

// splitQualified parses "namespace:name" or a bare "name" (which means the
// default namespace). It is the inverse of skills.SkillMeta.QualifiedName, and
// the two must agree: an operator types the name the model saw.
func splitQualified(ref string) (namespace, name string) {
	ns, n, found := strings.Cut(ref, ":")
	if !found {
		return skills.DefaultNamespace, ref
	}
	return ns, n
}

func newSkillsListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List skills in the daemon's corpus",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListSkills(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListSkillsRequest{IncludeDisabled: all}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			if mode != outputTable {
				return emitProtoRows(os.Stdout, resp.Msg.GetRows(), mode)
			}
			return renderSkillsTable(os.Stdout, resp.Msg.GetRows(), useColor)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include disabled skills")
	return cmd
}

// renderSkillsTable is the inventory: one row per visible skill row, its layer
// and its source, and whether it is in effect. SHADOWED marks an enabled row a
// higher layer overrides — it is listed so an operator can see what is being
// overridden, and why disabling the override changes nothing.
func renderSkillsTable(w io.Writer, rows []*rafikiv1.SkillRow, useColor bool) error {
	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "NAME", "LAYER", "SOURCE", "ENABLED", "SHADOWED", "DESCRIPTION")...)
	for _, r := range rows {
		tb.Row(
			r.GetNamespace()+":"+r.GetName(),
			r.GetLayer(),
			r.GetSource(),
			strconv.FormatBool(r.GetEnabled()),
			strconv.FormatBool(r.GetShadowed()),
			r.GetDescription(),
		)
	}
	return tb.Render()
}

func newSkillsShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <namespace:name>",
		Short: "Print one skill's body",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			ns, name := splitQualified(args[0])
			resp, err := ep.control().GetSkill(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.GetSkillRequest{Namespace: ns, Name: name}))
			if err != nil {
				return err
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			if mode != outputTable {
				return emitProto(os.Stdout, resp.Msg, mode)
			}
			return renderSkillShow(os.Stdout, resp.Msg.GetRow())
		},
	}
	cmd.ValidArgsFunction = completeSkillArgs
	return cmd
}

// renderSkillShow prints the body, then — for a skill that has supporting
// files — a `files:` section naming each one. The files themselves stay on the
// daemon: this is an inventory of what an agent would get beside SKILL.md, not
// their content.
func renderSkillShow(w io.Writer, row *rafikiv1.SkillRow) error {
	if _, err := fmt.Fprintln(w, row.GetBody()); err != nil {
		return err
	}
	files := row.GetFiles()
	if len(files) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "files:"); err != nil {
		return err
	}
	for _, f := range files {
		// Mode is always 0644 or 0755, so four octal digits is the full mode.
		if _, err := fmt.Fprintf(w, "%04o %d %s\n", f.GetMode(), f.GetSize(), f.GetPath()); err != nil {
			return err
		}
	}
	return nil
}

func newSkillsRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <namespace:name>",
		Short: "Delete a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer dropSkillCompletionCache(cmd)
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			ns, name := splitQualified(args[0])
			_, err = ep.control().DeleteSkill(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.DeleteSkillRequest{Namespace: ns, Name: name}))
			return err
		},
	}
	cmd.ValidArgsFunction = completeSkillArgs
	return cmd
}

// completeSkillArgs completes the <namespace:name> argument shared by
// skills show/rm/enable/disable. The verbs take exactly one ref, so past it
// there is nothing to offer.
func completeSkillArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeSkills(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// newSkillsEnableCmds builds BOTH enable and disable: they differ only in the
// boolean they send, and two near-identical files is how the two drift.
func newSkillsEnableCmds() []*cobra.Command {
	run := func(enabled bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			defer dropSkillCompletionCache(cmd)
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			ns, name := splitQualified(args[0])
			_, err = ep.control().SetSkillEnabled(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.SetSkillEnabledRequest{
					Namespace: ns, Name: name, Enabled: enabled,
				}))
			return err
		}
	}
	return []*cobra.Command{
		{
			Use: "enable <namespace:name>", Short: "Re-enable a disabled skill",
			Args: cobra.ExactArgs(1), RunE: run(true),
			ValidArgsFunction: completeSkillArgs,
		},
		{
			Use:   "disable <namespace:name>",
			Short: "Disable a skill; its name is then free for a replacement",
			Args:  cobra.ExactArgs(1), RunE: run(false),
			ValidArgsFunction: completeSkillArgs,
		},
	}
}
