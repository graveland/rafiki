package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file is the single source of truth for the rafiki CLI's command aliases
// and short-flag shorthands, and it guards the two invariants a future editor
// could break without noticing:
//
//     - presence: every alias / shorthand we decided on is actually set, so a
//       refactor that drops one is a test failure rather than a silent
//       regression ("rafiki k" stops working and nobody notices for weeks).
//     - no collisions: within a single parent no two commands share a name or
//       alias (cobra does NOT panic on a duplicate command alias — it just makes
//       the command unreachable or ambiguous), and within a single command no
//       two flags share a shorthand.
//
// It deliberately does NOT assert that every command has an alias: short,
// unambiguous verbs (executor create, service start, user list) earn no
// alias, and forcing one is churn, not value.

// findCmd locates a command by its canonical path from the root (the names a
// user would type, aliases excluded). An empty path is the root itself.
func findCmd(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	root := newRootCmd()
	if len(path) == 0 {
		return root
	}
	cmd, _, err := root.Find(path)
	if err != nil {
		t.Fatalf("find %v: %v", path, err)
	}
	return cmd
}

// lookupFlag finds a flag on cmd's own (local) flagset first, then its
// persistent flagset. Persistent flags — like the root's -o/-c/-s — are NOT in
// a command's local Flags() until cobra merges inherited flags at Execute time,
// so a construction-time Lookup on Flags() alone would miss them.
func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	return cmd.PersistentFlags().Lookup(name)
}

// walkCommands calls fn on the root and every subcommand, depth-first.
func walkCommands(root *cobra.Command, fn func(*cobra.Command)) {
	fn(root)
	for _, c := range root.Commands() {
		walkCommands(c, fn)
	}
}

// ─── alias presence ──────────────────────────────────────────────────────────

func TestCommandAliases(t *testing.T) {
	cases := []struct {
		path    []string
		aliases []string
	}{
		// Top-level verbs.
		{[]string{"list"}, []string{"ls"}},
		{[]string{"get"}, []string{"show", "info"}},
		{[]string{"status"}, []string{"st"}},
		{[]string{"create"}, []string{"cr"}},
		{[]string{"attach"}, []string{"a"}},
		{[]string{"resume"}, []string{"res"}},
		{[]string{"kill"}, []string{"k"}},
		{[]string{"forget"}, []string{"rm"}},
		{[]string{"recent"}, []string{}},
		{[]string{"search"}, []string{"find"}},
		{[]string{"tasks"}, []string{"task"}},
		{[]string{"conversations"}, []string{"c", "conv"}},
		{[]string{"send"}, []string{"snd"}},
		{[]string{"tail"}, []string{"stream"}},
		{[]string{"logs"}, []string{"log"}},
		{[]string{"label"}, []string{"lab"}},
		{[]string{"models"}, []string{"model"}},
		{[]string{"presets"}, []string{"preset"}},
		{[]string{"service"}, []string{"svc"}},
		{[]string{"install-extension"}, []string{"ie", "install-ext"}},
		{[]string{"completion"}, []string{"comp"}},
		{[]string{"claude"}, []string{"cl"}},
		{[]string{"executor"}, []string{"exec", "ex"}},
		{[]string{"user"}, []string{"usr"}},
		// Subcommands.
		{[]string{"conversations", "search"}, []string{"ls", "list"}},
		{[]string{"executor", "list"}, []string{"ls"}},
		{[]string{"executor", "label"}, []string{"lab"}},
		{[]string{"executor", "delete"}, []string{"del"}},
		{[]string{"executor", "service"}, []string{"svc"}},
		{[]string{"service", "logs"}, []string{"log"}},
	}
	for _, tc := range cases {
		cmd := findCmd(t, tc.path...)
		for _, a := range tc.aliases {
			if !containsAlias(cmd.Aliases, a) {
				t.Errorf("command %v: expected alias %q, got %v", tc.path, a, cmd.Aliases)
			}
		}
	}
}

// ─── alias collisions ────────────────────────────────────────────────────────

// TestNoCommandAliasCollisions walks the whole tree and asserts that, within
// every parent, no child name or alias is claimed by more than one child. Cobra
// never panics on this, so without this test a duplicate would silently make a
// command unreachable.
func TestNoCommandAliasCollisions(t *testing.T) {
	root := newRootCmd()
	walkCommands(root, func(cmd *cobra.Command) {
		seen := map[string]string{} // identity -> the child name that first claimed it
		for _, child := range cmd.Commands() {
			for _, id := range append([]string{child.Name()}, child.Aliases...) {
				if owner, ok := seen[id]; ok {
					t.Errorf("parent %q: command identity %q claimed by both %q and %q",
						cmd.Name(), id, owner, child.Name())
					continue
				}
				seen[id] = child.Name()
			}
		}
	})
}

// ─── short-flag presence ─────────────────────────────────────────────────────

func TestShortFlags(t *testing.T) {
	cases := []struct {
		path  []string // path to the command that owns the flag; empty = root
		flag  string
		short string
	}{
		// Root persistent flags: inherited by every subcommand.
		{nil, "output", "o"},
		{nil, "color", "c"},
		{nil, "socket", "s"},
		// Spawn-related verbs.
		{[]string{"create"}, "model", "m"},
		{[]string{"create"}, "detached", "d"},
		{[]string{"create"}, "preset", "p"},
		{[]string{"resume"}, "model", "m"},
		{[]string{"resume"}, "detached", "d"},
		{[]string{"resume"}, "preset", "p"},
		// -l limits, -r raw, and the pre-existing -n/-f/-v/-y.
		{[]string{"search"}, "limit", "l"},
		{[]string{"recent"}, "limit", "l"},
		{[]string{"tasks"}, "limit", "l"},
		{[]string{"executor", "list"}, "limit", "l"},
		{[]string{"logs"}, "raw", "r"},
		{[]string{"tail"}, "raw", "r"},
		{[]string{"logs"}, "tail", "n"},
		{[]string{"logs"}, "follow", "f"},
		{[]string{"logs"}, "verbose", "v"},
		{[]string{"tail"}, "tail", "n"},
		{[]string{"tail"}, "verbose", "v"},
		{[]string{"executor", "delete"}, "yes", "y"},
		// Launcher.
		{[]string{"claude"}, "url", "u"},
		{[]string{"claude"}, "model", "m"},
	}
	for _, tc := range cases {
		cmd := findCmd(t, tc.path...)
		f := lookupFlag(cmd, tc.flag)
		if f == nil {
			t.Errorf("command %v: flag --%s not found", tc.path, tc.flag)
			continue
		}
		if f.Shorthand != tc.short {
			t.Errorf("command %v: --%s shorthand = %q, want %q", tc.path, tc.flag, f.Shorthand, tc.short)
		}
	}
}

// ─── shorthand collisions ────────────────────────────────────────────────────

// TestNoShorthandCollisions asserts that, within every command, no two flags
// share a shorthand. It checks the command's full effective flagset — its own
// local flags, its own persistent flags, and the persistent shorthands it
// inherits from the root (-o/-c/-s) — since a local -o would clash with the
// inherited one. Before cobra merges inherited flags at Execute time the three
// flagsets are disjoint, so visiting each once covers the merged set without
// double-counting.
func TestNoShorthandCollisions(t *testing.T) {
	root := newRootCmd()
	walkCommands(root, func(cmd *cobra.Command) {
		seen := map[string]string{} // shorthand -> the flag that first claimed it
		visit := func(fs *pflag.FlagSet) {
			fs.VisitAll(func(f *pflag.Flag) {
				if f.Shorthand == "" {
					return
				}
				if owner, ok := seen[f.Shorthand]; ok {
					t.Errorf("command %q: shorthand -%s used by both --%s and --%s",
						cmd.Name(), f.Shorthand, owner, f.Name)
					return
				}
				seen[f.Shorthand] = f.Name
			})
		}
		visit(cmd.Flags())
		visit(cmd.PersistentFlags())
		visit(cmd.InheritedFlags())
	})
}
