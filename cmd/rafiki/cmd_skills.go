// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/skills"
)

func newSkillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage the daemon's skill corpus",
	}
	cmd.AddCommand(
		newSkillsListCmd(),
		newSkillsShowCmd(),
		newSkillsAddCmd(),
		newSkillsRmCmd(),
		newSkillsImportCmd(),
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
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSOURCE\tENABLED\tDESCRIPTION")
			for _, r := range resp.Msg.GetRows() {
				fmt.Fprintf(w, "%s:%s\t%s\t%t\t%s\n",
					r.GetNamespace(), r.GetName(), r.GetSource(), r.GetEnabled(), r.GetDescription())
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include disabled skills")
	return cmd
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
			fmt.Println(resp.Msg.GetRow().GetBody())
			return nil
		},
	}
	cmd.ValidArgsFunction = completeSkillArgs
	return cmd
}

func newSkillsAddCmd() *cobra.Command {
	var file, namespace string
	cmd := &cobra.Command{
		Use:   "add --file <SKILL.md>",
		Short: "Add or replace a skill from a local SKILL.md",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defer dropSkillCompletionCache(cmd)
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			name, description, body, err := skills.ParseSkillFile(string(data))
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().UpsertSkill(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.UpsertSkillRequest{
					Namespace:   namespace,
					Name:        name,
					Description: description,
					Body:        body,
				}))
			if err != nil {
				return err
			}
			r := resp.Msg.GetRow()
			fmt.Printf("%s:%s\n", r.GetNamespace(), r.GetName())
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to a SKILL.md")
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace (default rafiki)")
	return cmd
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

func newSkillsImportCmd() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:   "import <dir>",
		Short: "Import every <name>/SKILL.md under a directory",
		Long: "Imports a corpus laid out as <dir>/<name>/SKILL.md — a Claude Code " +
			"plugin's skills/ tree, or any checkout in that shape.\n\n" +
			"An imported corpus keeps its upstream plugin name as its namespace, " +
			"so a claude child on a machine where that plugin is really installed " +
			"deduplicates the two instead of seeing the same skill twice. Pass " +
			"--namespace to override the derived name.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer dropSkillCompletionCache(cmd)
			ns := namespace
			if ns == "" {
				var err error
				ns, err = derivePluginNamespace(args[0])
				if err != nil {
					return err
				}
			}
			metas, err := skills.DiscoverSkills([]string{args[0]}, nil)
			if err != nil {
				return err
			}
			if len(metas) == 0 {
				return fmt.Errorf("no skills found under %s (expected <name>/SKILL.md)", args[0])
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			for _, m := range metas {
				body, err := skills.SkillBody(m.Path)
				if err != nil {
					return err
				}
				if _, err := ep.control().UpsertSkill(cmdCtx(cmd),
					connect.NewRequest(&rafikiv1.UpsertSkillRequest{
						Namespace:   ns,
						Name:        m.Name,
						Description: m.Description,
						Body:        body,
						Source:      "import:" + ns,
					})); err != nil {
					return fmt.Errorf("%s: %w", m.Name, err)
				}
				fmt.Printf("%s:%s\n", ns, m.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace to import into (default: derived from the source)")
	return cmd
}

// derivePluginNamespace reads the upstream plugin name from a checkout's
// .claude-plugin manifest, falling back to the directory's own name. The
// fallback ERRS when it cannot produce a name — `rafiki skills import .`
// would otherwise upsert the whole corpus into a namespace literally named
// ".", which renders as ".:skill" in inventories and deduplicates against
// nothing. --namespace is the escape hatch.
//
// The name has to come from upstream's declaration rather than a guess:
// Claude Code suppresses a skills-dir plugin whose name an installed
// marketplace plugin already claims, and that deduplication is the whole
// reason an imported corpus keeps its own namespace.
func derivePluginNamespace(dir string) (string, error) {
	// Look one level up too: a plugin's skills live at <plugin>/skills/, so an
	// operator pointing at the skills dir itself still gets the plugin's name.
	for _, base := range []string{dir, filepath.Dir(dir)} {
		if n := readPluginName(base); n != "" {
			return n, nil
		}
	}
	fallback := filepath.Base(filepath.Clean(dir))
	if fallback == "." || fallback == ".." {
		return "", fmt.Errorf("cannot derive a namespace from %q; pass --namespace", dir)
	}
	return fallback, nil
}

// readPluginName reads a plugin name out of a checkout's .claude-plugin
// manifests. Returns "" for anything it cannot answer confidently — a corpus
// without a manifest is the normal case, not a failure, and a marketplace
// declaring several plugins cannot be resolved without knowing which subpath
// the caller meant.
func readPluginName(base string) string {
	mkt := filepath.Join(base, ".claude-plugin", "marketplace.json")
	if data, err := os.ReadFile(mkt); err == nil {
		var decl struct {
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
		}
		if json.Unmarshal(data, &decl) == nil && len(decl.Plugins) == 1 {
			// The PLUGIN's name, never the marketplace's: Claude Code
			// namespaces skills by plugin, so the marketplace name would
			// deduplicate against nothing.
			return validPluginName(decl.Plugins[0].Name)
		}
	}
	pj := filepath.Join(base, ".claude-plugin", "plugin.json")
	if data, err := os.ReadFile(pj); err == nil {
		var decl struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &decl) == nil {
			return validPluginName(decl.Name)
		}
	}
	return ""
}

// validPluginName guards a name that came from a file rafiki did not write:
// it becomes a directory name on an executor and a prefix in a model's
// inventory, so path separators and traversal must never survive it.
func validPluginName(n string) string {
	if n == "" || n == "." || n == ".." {
		return ""
	}
	if strings.ContainsAny(n, `/\:`+"\x00") {
		return ""
	}
	if len(n) > 64 {
		return ""
	}
	return n
}
