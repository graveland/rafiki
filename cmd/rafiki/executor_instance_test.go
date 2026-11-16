// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// resetInstance restores the process-wide executor instance. Tests only: the
// instance is global on purpose (one per process, set before any path is read),
// so a test that names one must not leave the package's other tests resolving
// their paths under profiles/<name>.
func resetInstance(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = paths.SetInstance(paths.DefaultInstance) })
}

// `executor serve` takes its instance from the RAW -P value, never from
// profiles.toml: a remote target installed over ssh has no profiles.toml, and a
// service whose state moved when someone ran `rafiki profile use` would be a
// service serving another instance's credential.
func TestExecutorInstanceServeUsesRawFlagNotProfilesFile(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetInstance(t)
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))

	root := newRootCmd()
	serve := newExecutorServeCmd()
	root.AddCommand(serve)
	// ParseFlags merges the root's persistent flags into serve's set, exactly as
	// cobra does when it executes `rafiki -P ublk executor serve`.
	c.NoError(serve.ParseFlags([]string{"-P", "ublk"}), "parse -P ublk")
	c.Eq("ublk", executorInstanceName(serve), "executorInstanceName reads the inherited flag")

	// The first thing serve's RunE does, before any path is resolved.
	c.NoError(applyExecutorInstance(serve), "applyExecutorInstance")

	c.Eq("ublk", paths.Instance(), "instance")
	c.Eq(filepath.Join(dataHome, "rafiki", "profiles", "ublk"), paths.DataDir(), "DataDir")
	c.Eq(filepath.Join(dataHome, "rafiki", "profiles", "ublk", "executor.cred"),
		filepath.Join(paths.DataDir(), "executor.cred"), "the credential file is per instance")

	_, err := profile.Load()
	c.Error(err, "serve must not resolve — or bootstrap — a profiles.toml to learn its instance")
}

// A non-default instance with no CLAUDE_CONFIG_DIR of its own would write the
// same ~/.claude/skills the default instance writes. The implied forms stay off
// and an explicit --skills-sync=true is refused, naming the fix.
func TestExecutorInstanceSkillsSyncRefusedForNonDefault(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetInstance(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("RAFIKI_EXECUTOR_SKILLS_SYNC", "")

	c.NoError(paths.SetInstance("work"), "SetInstance(work)")

	explicit := newExecutorServeCmd()
	c.NoError(explicit.Flags().Set("skills-sync", "true"), "set --skills-sync=true")
	err := refuseSkillsSyncForNonDefaultInstance(explicit, true)
	c.Error(err, "an explicit --skills-sync=true must be refused for a non-default instance")
	c.StrContains(err.Error(),
		`--skills-sync writes ~/.claude/skills, which instance "default" also writes; `+
			`set CLAUDE_CONFIG_DIR for instance "work" to enable it`,
		"refusal text")

	// --launch claude implies the sync for the default instance; here it is
	// switched off rather than refused, because nothing asked for it explicitly.
	implied := newExecutorServeCmd()
	c.False(skillsSyncEnabled(implied, false, []string{"claude"}),
		"the --launch claude implication must be off for a non-default instance with no CLAUDE_CONFIG_DIR")
	c.NoError(refuseSkillsSyncForNonDefaultInstance(implied, false), "an explicit --skills-sync=false is never refused")

	// An instance with its own corpus is unaffected, in both directions.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	c.NoError(refuseSkillsSyncForNonDefaultInstance(explicit, true),
		"an instance with its own CLAUDE_CONFIG_DIR may sync")
	c.True(skillsSyncEnabled(implied, false, []string{"claude"}),
		"an instance with its own CLAUDE_CONFIG_DIR takes the --launch claude implication")

	// And the default instance always may: it is the one that owns the shared dir.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	c.NoError(paths.SetInstance(paths.DefaultInstance), "SetInstance(DefaultInstance)")
	c.NoError(refuseSkillsSyncForNonDefaultInstance(explicit, true), "the default instance may always sync")
	c.True(skillsSyncEnabled(implied, false, []string{"claude"}),
		"the default instance takes the --launch claude implication")
}
