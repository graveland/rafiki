// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// One resolution per process. Every verb asks for the profile — several ask
// more than once — and re-reading the manifest per call would let the answer
// change mid-command, which is exactly the class of bug this feature removes.
var (
	profileOnce sync.Once
	profileVal  profile.Resolved
	profileErr  error
)

// resolveProfile resolves the process's profile, once.
func resolveProfile(cmd *cobra.Command) (profile.Resolved, error) {
	profileOnce.Do(func() {
		if err := profile.CheckRetiredEnv(); err != nil {
			profileErr = err
			return
		}
		flag := ""
		if cmd != nil {
			flag, _ = cmd.Flags().GetString("profile")
		}
		env, envSet := os.LookupEnv("RAFIKI_PROFILE")
		profileVal, profileErr = profile.Resolve(profile.Selection{
			Flag: flag, Env: env, EnvSet: envSet,
		})
		if profileErr == nil && profileVal.Bootstrapped {
			fmt.Fprintf(os.Stderr,
				"created %s with a %q profile for the local daemon (%s)\n",
				profile.ProfilesFile(), profileVal.Name, profileVal.Socket)
		}
	})
	return profileVal, profileErr
}

// mustProfile is resolveProfile for the many call sites that cannot proceed
// without one. Exit code 2, matching mustDial: a configuration failure is not
// a user-input error (1).
func mustProfile(cmd *cobra.Command) profile.Resolved {
	p, err := resolveProfile(cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	return p
}

// completeProfileNames offers profile names for -P. A completion handler must
// never print or fail, so every error degrades to no candidates.
func completeProfileNames(toComplete string) []string {
	set, err := profile.Load()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range set.Names() {
		if strings.HasPrefix(n, toComplete) {
			out = append(out, n)
		}
	}
	return out
}

// resetProfileCache clears the once-per-process resolution. Tests only: each
// test case supplies its own manifest, and a sync.Once shared across them
// would make the first case's profile win for the whole package.
func resetProfileCache() {
	profileOnce = sync.Once{}
	profileVal = profile.Resolved{}
	profileErr = nil
}
