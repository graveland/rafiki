// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// The profile verbs deliberately never call resolveProfile. They must work on
// a machine with no manifest at all, or the feature could not bootstrap
// itself: `rafiki profile list` on a bare machine prints nothing and exits 0.
func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage client profiles: a daemon and the credential it needs",
		Long: `A profile names one daemon and the credential that daemon needs, so the
two travel together. The selected profile is recorded in the ` + "`current-profile`" + `
file; -P overrides it for one command and $RAFIKI_PROFILE for one shell.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		newProfileListCmd(),
		newProfileShowCmd(),
		newProfileCurrentCmd(),
		newProfileUseCmd(),
		newProfileAddCmd(),
		newProfileRemoveCmd(),
	)
	return cmd
}

// loadForEdit reads the manifest, treating "no manifest" as an empty set so
// `profile add` works as the very first command on a machine.
func loadForEdit() (profile.Set, error) {
	set, err := profile.Load()
	if errors.Is(err, profile.ErrNoManifest) {
		return profile.Set{Profiles: map[string]profile.Profile{}}, nil
	}
	if err != nil {
		return profile.Set{}, err
	}
	if set.Profiles == nil {
		set.Profiles = map[string]profile.Profile{}
	}
	return set, nil
}

func endpointOf(p profile.Profile) string {
	if p.URL != "" {
		return p.URL
	}
	return p.Socket
}

func newProfileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			set, err := loadForEdit()
			if err != nil {
				return err
			}
			current := profile.LoadPointer()

			tw := table.NewWriter()
			tw.SetOutputMirror(cmd.OutOrStdout())
			st := table.StyleLight
			st.Color = table.ColorOptions{}
			tw.SetStyle(st)
			tw.AppendHeader(table.Row{"", "NAME", "ENDPOINT", "TOKEN", "KIND", "MODEL"})
			for _, name := range set.Names() {
				p, _ := set.Get(name)
				marker := ""
				if name == current {
					marker = "*"
				}
				tok := "-"
				if profile.ReadToken(name) != "" {
					tok = "yes"
				}
				tw.AppendRow(table.Row{
					marker, name, endpointOf(p), tok,
					defaultDash(p.Kind), defaultDash(p.Model),
				})
			}
			tw.Render()
			return nil
		},
	}
}

func newProfileShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [name]",
		Short: "Print one profile in full",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := loadForEdit()
			if err != nil {
				return err
			}
			name := profile.LoadPointer()
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" {
				return errors.New("no profile selected and none named; try `rafiki profile list`")
			}
			p, ok := set.Get(name)
			if !ok {
				return fmt.Errorf("unknown profile %q (known: %s)", name, strings.Join(set.Names(), ", "))
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "name:     %s\n", p.Name)
			fmt.Fprintf(w, "endpoint: %s\n", endpointOf(p))
			fmt.Fprintf(w, "proxy:    %s\n", defaultDash(p.Proxy))
			if profile.ReadToken(name) == "" {
				// Legal, and it degrades in ways worth naming rather than
				// leaving to be discovered: the control socket admits without
				// one, but the proxy face requires it and per-user reads
				// resolve to nobody.
				fmt.Fprintf(w, "token:    none (%s) — no `rafiki claude`, no quota status\n", profile.TokenFile(name))
			} else {
				fmt.Fprintf(w, "token:    present (%s)\n", profile.TokenFile(name))
			}
			fmt.Fprintf(w, "kind:     %s\n", defaultDash(p.Kind))
			fmt.Fprintf(w, "model:    %s\n", defaultDash(p.Model))
			fmt.Fprintf(w, "preset:   %s\n", defaultDash(p.Preset))
			fmt.Fprintf(w, "labels:   %s\n", defaultDash(formatProfileLabels(p.Labels)))
			fmt.Fprintf(w, "presets:  %s\n", profile.PresetsFile(name))
			return nil
		},
	}
	cmd.ValidArgsFunction = func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeProfileNames(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func newProfileCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Print the selected profile's name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name := profile.LoadPointer()
			if name == "" {
				return errors.New("no profile selected; run `rafiki profile use <name>`")
			}
			fmt.Fprintln(cmd.OutOrStdout(), name)
			return nil
		},
	}
}

func newProfileUseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use <name>",
		Short: "Select a profile for every terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := loadForEdit()
			if err != nil {
				return err
			}
			p, ok := set.Get(args[0])
			if !ok {
				return fmt.Errorf("unknown profile %q (known: %s)", args[0], strings.Join(set.Names(), ", "))
			}
			if err := profile.SavePointer(p.Name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "now using profile %q (%s)\n", p.Name, endpointOf(p))
			return nil
		},
	}
	cmd.ValidArgsFunction = func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeProfileNames(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func newProfileAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a profile",
		Args:  cobra.ExactArgs(1),
		RunE:  runProfileAdd,
	}
	cmd.Flags().String("url", "", "remote daemon control plane, https://host[:port]")
	cmd.Flags().String("socket", "", "local daemon control socket path")
	cmd.Flags().String("proxy", "", "LLM proxy base URL for `rafiki claude`")
	cmd.Flags().String("token", "", "control-plane token; required with --url")
	cmd.Flags().String("kind", "", "default agent kind for `rafiki create`")
	cmd.Flags().String("model", "", "default model for `rafiki create`")
	cmd.Flags().String("preset", "", "default preset for `rafiki create`")
	cmd.Flags().StringArray("label", nil, "default label k=v (repeatable)")
	cmd.ValidArgsFunction = func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runProfileAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := profile.ValidName(name); err != nil {
		return fmt.Errorf("profile add: %w", err)
	}
	url, _ := cmd.Flags().GetString("url")
	socket, _ := cmd.Flags().GetString("socket")
	proxy, _ := cmd.Flags().GetString("proxy")
	token, _ := cmd.Flags().GetString("token")
	kind, _ := cmd.Flags().GetString("kind")
	model, _ := cmd.Flags().GetString("model")
	preset, _ := cmd.Flags().GetString("preset")
	labelPairs, _ := cmd.Flags().GetStringArray("label")

	switch {
	case url != "" && socket != "":
		return errors.New("--url and --socket are mutually exclusive; a profile names exactly one endpoint")
	case url == "" && socket == "":
		return errors.New("one of --url or --socket is required")
	}
	// This plane has no bootstrap mode — there is no user-create RPC on it —
	// so a remote with no credential can only ever produce a 401. Refuse now
	// rather than after a round trip.
	if url != "" && token == "" {
		return errors.New("--token is required with --url: a remote profile with no token can only ever be rejected")
	}

	labels, err := parseLabelPairs(labelPairs)
	if err != nil {
		return fmt.Errorf("--label: %w", err)
	}

	set, err := loadForEdit()
	if err != nil {
		return err
	}
	if _, exists := set.Get(name); exists {
		return fmt.Errorf("profile %q already exists; edit %s or remove it first", name, profile.ProfilesFile())
	}
	newProfile := profile.Profile{
		Name: name, URL: url, Socket: socket, Proxy: proxy,
		Kind: kind, Model: model, Preset: preset, Labels: labels,
	}
	// Validate BEFORE writing anything: a bad profile written to disk corrupts
	// the manifest for every subsequent `rafiki profile *` command until it is
	// hand-edited back to something that parses.
	if err := profile.Validate(newProfile); err != nil {
		return err
	}
	set.Profiles[name] = newProfile
	if err := profile.Save(set); err != nil {
		return err
	}
	if token != "" {
		if err := profile.WriteToken(name, token); err != nil {
			return err
		}
	}
	// First profile on the machine selects itself; otherwise leave the
	// current selection alone, because adding a profile is not choosing it.
	if profile.LoadPointer() == "" {
		if err := profile.SavePointer(name); err != nil {
			return err
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "added profile %q\n", name)
	return nil
}

func newProfileRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a profile and its token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			force, _ := cmd.Flags().GetBool("force")

			set, err := loadForEdit()
			if err != nil {
				return err
			}
			if _, ok := set.Get(name); !ok {
				return fmt.Errorf("unknown profile %q (known: %s)", name, strings.Join(set.Names(), ", "))
			}
			// Removing the selected profile leaves every other command
			// unresolvable until someone runs `profile use`. Legal, but only
			// on purpose.
			if profile.LoadPointer() == name && !force {
				return fmt.Errorf("%q is the selected profile; run `rafiki profile use <other>` first, or pass --force", name)
			}
			delete(set.Profiles, name)
			if err := profile.Save(set); err != nil {
				return err
			}
			if profile.LoadPointer() == name {
				if err := profile.SavePointer(""); err != nil {
					return err
				}
			}
			if err := os.RemoveAll(profile.Dir(name)); err != nil {
				return fmt.Errorf("remove %s: %w", profile.Dir(name), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed profile %q\n", name)
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "remove even if it is the selected profile")
	cmd.ValidArgsFunction = func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeProfileNames(toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

// formatProfileLabels renders a label map as a stable "k=v,k2=v2" string.
//
// Named distinctly from labels.go's formatLabels (which has a different
// signature: maxLen/includeAutoLabels for the list-table column) to avoid a
// redeclaration in this package.
func formatProfileLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
