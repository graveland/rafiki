// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/clientstate"
)

// configKey is one setting `rafiki config` knows about. Both get and set
// read/write the same clientstate.State so `show`'s output is valid input to
// `set` -- there is no separate key vocabulary for reading vs. writing.
//
// This is a small registry rather than a hand-written switch per key because
// there is more than one setting coming (a claude model-mapping table is the
// next one planned): adding a setting is one entry here, not a new
// subcommand or a bespoke flag set.
//
// set's error must depend only on its own value argument, never on the
// state it is passed -- runConfigSet relies on this to validate a whole
// `config set a=1 b=2` batch against a throwaway State before applying any
// of it for real. A key whose validity depends on prior state (e.g. "must
// differ from the current value") would validate correctly but could still
// apply partially, since Update saves whatever its mutate callback left
// behind regardless of an error return.
type configKey struct {
	name string
	get  func(s clientstate.State) string
	set  func(s *clientstate.State, value string) error

	// global marks a key as living in the shared document rather than the
	// resolved profile's. Per-profile is the DEFAULT and global is the
	// exception: a key qualifies only when it is a property of the person that
	// no daemon can influence — display and presentation. Currency clears that
	// bar; a model-mapping table would not.
	global bool
}

var configKeys = []configKey{
	{
		name:   "currency.code",
		global: true,
		get: func(s clientstate.State) string {
			if s.Currency == nil || s.Currency.Code == "" {
				return ""
			}
			return s.Currency.Code
		},
		set: func(s *clientstate.State, value string) error {
			code := strings.ToUpper(strings.TrimSpace(value))
			if code == "" {
				return fmt.Errorf("currency.code: value required")
			}
			if s.Currency == nil {
				s.Currency = &clientstate.Currency{}
			}
			s.Currency.Code = code
			return nil
		},
	},
	{
		name:   "currency.rate",
		global: true,
		get: func(s clientstate.State) string {
			if s.Currency == nil || s.Currency.Rate == 0 {
				return ""
			}
			return strconv.FormatFloat(s.Currency.Rate, 'f', -1, 64)
		},
		set: func(s *clientstate.State, value string) error {
			rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || rate <= 0 {
				return fmt.Errorf("currency.rate: %q must be a positive number (local units per USD)", value)
			}
			if s.Currency == nil {
				s.Currency = &clientstate.Currency{}
			}
			s.Currency.Rate = rate
			return nil
		},
	},
}

func findConfigKey(name string) (configKey, bool) {
	for _, k := range configKeys {
		if k.name == name {
			return k, true
		}
	}
	return configKey{}, false
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View or change client-side display preferences",
		Long: `View or change client-side display preferences, such as the currency
` + "`rafiki list`/the TUI convert cost figures into." + `

Purely local: stored in the client state file (see ` + "`rafiki --help`" + ` for its
path), never sent to the daemon. Costs are still tracked and billed in USD
everywhere else -- this only changes the last-mile string a person reads.`,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newConfigShowCmd(), newConfigSetCmd())
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print every known client setting",
		Args:  cobra.NoArgs,
		RunE:  runConfigShow,
	}
}

func runConfigShow(cmd *cobra.Command, _ []string) error {
	p := mustProfile(cmd)
	globalState := clientstate.LoadScoped(clientstate.Scope{})
	profState := clientstate.LoadScoped(clientstate.Scope{Profile: p.Name})
	mode, useColor := outputOpts(cmd)
	if mode == outputTable {
		fmt.Fprint(os.Stdout, profileIndicator(p.Name))
	}
	return renderConfig(os.Stdout, globalState, profState, mode, useColor)
}

func renderConfig(w io.Writer, globalState, profState clientstate.State, mode outputMode, useColor bool) error {
	stateFor := func(k configKey) clientstate.State {
		if k.global {
			return globalState
		}
		return profState
	}
	scopeOf := func(k configKey) string {
		if k.global {
			return "global"
		}
		return "profile"
	}

	names := make([]string, len(configKeys))
	for i, k := range configKeys {
		names[i] = k.name
	}
	sort.Strings(names)

	// The JSON shape stays a flat name->value object: it is machine-readable
	// output and adding a nested scope would break every existing consumer to
	// carry a hint only a human needs.
	if mode == outputJSON {
		out := make(map[string]string, len(configKeys))
		for _, k := range configKeys {
			out[k.name] = k.get(stateFor(k))
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(w)
	st := table.StyleLight
	st.Color = table.ColorOptions{}
	tw.SetStyle(st)

	headerRow := table.Row{"KEY", "SCOPE", "VALUE"}
	if useColor {
		headerRow = table.Row{dim("KEY"), dim("SCOPE"), dim("VALUE")}
	}
	tw.AppendHeader(headerRow)
	for _, name := range names {
		k, _ := findConfigKey(name)
		tw.AppendRow(table.Row{k.name, scopeOf(k), defaultDash(k.get(stateFor(k)))})
	}
	tw.Render()
	return nil
}

func newConfigSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set key=value [key=value ...]",
		Short: "Set one or more client settings",
		Long: `Set one or more client settings, given as key=value pairs (the same
"key" ` + "`config show`" + ` prints).

	rafiki config set currency.code=CAD currency.rate=1.38

All pairs are validated before anything is written, so a typo in the second
pair does not leave the first one applied.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runConfigSet,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		names := make([]string, len(configKeys))
		for i, k := range configKeys {
			names[i] = k.name + "="
		}
		return names, cobra.ShellCompDirectiveNoSpace
	}
	return cmd
}

// parseConfigPairs splits "key=value" args and resolves each to its
// configKey, without applying anything -- so runConfigSet can validate every
// pair before mutating clientstate.State.
func parseConfigPairs(args []string) ([]struct {
	key configKey
	val string
}, error) {
	known := make([]string, len(configKeys))
	for i, ck := range configKeys {
		known[i] = ck.name
	}

	out := make([]struct {
		key configKey
		val string
	}, 0, len(args))
	for _, arg := range args {
		idx := strings.IndexByte(arg, '=')
		if idx < 0 {
			return nil, fmt.Errorf("%q is not in key=value format", arg)
		}
		name, val := arg[:idx], arg[idx+1:]
		k, ok := findConfigKey(name)
		if !ok {
			return nil, fmt.Errorf("unknown config key %q (known: %s)", name, strings.Join(known, ", "))
		}
		out = append(out, struct {
			key configKey
			val string
		}{k, val})
	}
	return out, nil
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	pairs, err := parseConfigPairs(args)
	if err != nil {
		return err
	}

	// Every set func's error depends only on its own value, never on prior
	// state (see configKey's doc comment) -- so running them all against a
	// throwaway State first validates the whole batch before Update commits
	// anything for real. Without this, a later pair failing inside Update's
	// mutate callback would still leave the earlier pairs applied: Update
	// saves whatever the callback left behind regardless of an error return.
	scratch := &clientstate.State{}
	for _, p := range pairs {
		if err := p.key.set(scratch, p.val); err != nil {
			return err
		}
	}

	p := mustProfile(cmd)
	var globalPairs, profilePairs []struct {
		key configKey
		val string
	}
	for _, pair := range pairs {
		if pair.key.global {
			globalPairs = append(globalPairs, pair)
		} else {
			profilePairs = append(profilePairs, pair)
		}
	}
	if len(globalPairs) > 0 {
		clientstate.UpdateScoped(clientstate.Scope{}, func(s *clientstate.State) {
			for _, pair := range globalPairs {
				_ = pair.key.set(s, pair.val) // validated against a scratch State above
			}
		})
	}
	if len(profilePairs) > 0 {
		clientstate.UpdateScoped(clientstate.Scope{Profile: p.Name}, func(s *clientstate.State) {
			for _, pair := range profilePairs {
				_ = pair.key.set(s, pair.val)
			}
		})
	}

	globalFinal := clientstate.LoadScoped(clientstate.Scope{})
	profFinal := clientstate.LoadScoped(clientstate.Scope{Profile: p.Name})
	for _, pair := range pairs {
		final := profFinal
		if pair.key.global {
			final = globalFinal
		}
		fmt.Fprintf(os.Stdout, "%s = %s\n", pair.key.name, pair.key.get(final))
	}
	return nil
}
