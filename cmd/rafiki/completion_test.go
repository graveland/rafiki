// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// seedRemoteProfile isolates profile state for the test and seeds a single
// remote profile pointed at url, with token as its credential (empty means
// no token file is written at all).
func seedRemoteProfile(t *testing.T, name, url, token string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		name: {Name: name, URL: url},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if token != "" {
		if err := profile.WriteToken(name, token); err != nil {
			t.Fatalf("WriteToken: %v", err)
		}
	}
	if err := profile.SavePointer(name); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// seedLocalProfile is seedRemoteProfile's socket-based counterpart.
func seedLocalProfile(t *testing.T, name, socket string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		name: {Name: name, Socket: socket},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer(name); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// The cache is the only part of this path testable without a live daemon: the
// fetch itself needs a Connect server. What matters here is that a cached
// answer is served without one, and that a drop forces a refetch.
func TestChildCompletionServesFromCache(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	want := []completionChild{{ChildID: "c_1", Name: "alpha", Status: "idle"}}
	cacheWrite("children", completionEndpointKey(nil), want)

	got := completionChildrenCached(nil, childCacheTTL)
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("got %+v, want the cached row", got)
	}
}

func TestDropChildCompletionCacheForcesARefetch(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cacheWrite("children", completionEndpointKey(nil), []completionChild{{Name: "alpha"}})
	dropChildCompletionCache(nil)

	if got := completionChildrenCached(nil, childCacheTTL); len(got) != 0 {
		t.Errorf("got %+v after a drop, want no cached rows", got)
	}
}

// An unreachable daemon must yield no candidates, never an error or an exit.
func TestChildCompletionOnAnUnreachableDaemonIsEmpty(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://127.0.0.1:1", "t")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	done := make(chan []completionChild, 1)
	go func() { done <- completionChildren(nil) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completion blocked the shell for 5s; it must bound its own deadline")
	}
}

// The completion key follows the resolved PROFILE, not an env var: the old
// bug this task closes was a helper reading only --socket and never
// consulting RAFIKI_URL, so a remote operator got no candidates at all,
// silently. Now the key is the profile's URL when it names a remote, and a
// "unix:"-prefixed socket path when it names a local daemon.
func TestCompletionEndpointKeyFollowsTheProfile(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://daemon.example", "t")
	if got := completionEndpointKey(nil); got != "https://daemon.example" {
		t.Errorf("key = %q, want the remote URL", got)
	}

	seedLocalProfile(t, "work", "/tmp/work-scratch/controller.sock")
	if got := completionEndpointKey(nil); !strings.HasPrefix(got, "unix:") {
		t.Errorf("key = %q, want a unix: key for a local (socket) profile", got)
	}
}

// A -P profile override must move the cache KEY with it — the key names the
// endpoint the answer came from, and a command pointed at a scratch daemon's
// profile must not read, write or drop the default profile's entries. The key
// must also equal the identity the endpoint resolver answers, so reads,
// writes and drops can never drift from what is actually dialed.
func TestCompletionKeyFollowsTheProfileOverride(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"default": {Name: "default", Socket: "/tmp/default-scratch/controller.sock"},
		"scratch": {Name: "scratch", Socket: "/tmp/scratch-1/rafiki/controller.sock"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("default"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().StringP("profile", "P", "", "")
	if err := cmd.Flags().Set("profile", "scratch"); err != nil {
		t.Fatal(err)
	}

	want := "unix:/tmp/scratch-1/rafiki/connect.sock"
	if got := completionEndpointKey(cmd); got != want {
		t.Errorf("key = %q, want %q — the override must move the identity off the default profile", got, want)
	}
	if got := completionEndpointKey(cmd); got == "unix:/tmp/default-scratch/connect.sock" {
		t.Errorf("key = %q, still the DEFAULT profile's connect socket", got)
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.identity != completionEndpointKey(cmd) {
		t.Errorf("endpoint identity %q != cache key %q — reads, writes and drops must not drift from what is dialed", ep.identity, completionEndpointKey(cmd))
	}
}

// A remote endpoint with no token cannot succeed. It must be a miss, not the
// error newConnectEndpoint returns for interactive callers.
func TestChildCompletionWithoutATokenIsEmpty(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if got := completionChildren(nil); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// ─── Task 5.1: the completion sweep ───────────────────────────────────────────

// seedCompletionCache writes names straight into a cache entry for kind, so a
// completer can be exercised without a live daemon (the fetch itself needs
// one; see TestChildCompletionOnAnUnreachableDaemonIsEmpty for that path).
func seedCompletionCache(t *testing.T, kind string, names any) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheWrite(kind, completionEndpointKey(nil), names)
}

// seedChildrenCompletionCache seeds the children cache with two rows and
// returns the completer's expected id/name universe.
func seedChildrenCompletionCache(t *testing.T) {
	t.Helper()
	seedCompletionCache(t, "children", []completionChild{
		{ChildID: "c_01HXABC", Name: "alpha", Status: "idle"},
		{ChildID: "c_02HXDEF", Name: "beta", Status: "exited"},
	})
}

func TestCompleteHistoryOffersChildren(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedChildrenCompletionCache(t)

	cmd := newHistoryCmd()
	if cmd.ValidArgsFunction == nil {
		t.Fatal("ValidArgsFunction not set — `rafiki history <TAB>` completes nothing")
	}
	got, directive := cmd.ValidArgsFunction(cmd, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
	for _, want := range []string{"c_01HXABC", "alpha", "beta"} {
		if !containsCandidate(got, want) {
			t.Errorf("candidates %v missing %q (ids and names both target history)", got, want)
		}
	}
	// One target is all the verb takes; past it there is nothing to offer.
	if got, _ := cmd.ValidArgsFunction(cmd, []string{"c_01HXABC"}, ""); len(got) != 0 {
		t.Errorf("past the single target got %v, want none", got)
	}
}

func TestCompleteStatusOffersChildren(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedChildrenCompletionCache(t)

	cmd := newStatusCmd()
	if cmd.ValidArgsFunction == nil {
		t.Fatal("ValidArgsFunction not set — `rafiki status <TAB>` completes nothing")
	}
	got, directive := cmd.ValidArgsFunction(cmd, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
	for _, want := range []string{"c_01HXABC", "alpha", "beta"} {
		if !containsCandidate(got, want) {
			t.Errorf("candidates %v missing %q", got, want)
		}
	}
	if got, _ := cmd.ValidArgsFunction(cmd, []string{"alpha"}, ""); len(got) != 0 {
		t.Errorf("past the single target got %v, want none", got)
	}
	// The arg signature the completion promises: one OPTIONAL target. NoArgs
	// would make the offered candidates unusable; more than one is rejected.
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("zero args rejected: %v", err)
	}
	if err := cmd.Args(cmd, []string{"alpha"}); err != nil {
		t.Errorf("one arg rejected: %v", err)
	}
	if err := cmd.Args(cmd, []string{"alpha", "beta"}); err == nil {
		t.Error("two args accepted; status takes at most one target")
	}
}

func TestCompleteSkillsNamespaced(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedCompletionCache(t, "skills", []completionSkill{
		{Namespace: "rafiki", Name: "commit-style"},
		{Namespace: "acme", Name: "deploy"},
	})

	if got := completeSkills(nil, ""); !containsCandidate(got, "rafiki:commit-style") || !containsCandidate(got, "acme:deploy") {
		t.Errorf("bare TAB got %v, want every qualified ns:name ref", got)
	}
	// A bare prefix names the skill: the candidate is still the qualified ref,
	// since splitQualified resolves a colonless arg against the default
	// namespace only.
	got := completeSkills(nil, "com")
	if len(got) != 1 || got[0] != "rafiki:commit-style" {
		t.Errorf("completeSkills(nil, %q) = %v, want [rafiki:commit-style]", "com", got)
	}
	// A colon fixes the namespace; past it only the qualified form matches.
	got = completeSkills(nil, "rafiki:co")
	if len(got) != 1 || got[0] != "rafiki:commit-style" {
		t.Errorf("completeSkills(nil, %q) = %v, want [rafiki:commit-style]", "rafiki:co", got)
	}
	// The namespace alone is a prefix of the qualified ref.
	got = completeSkills(nil, "acme")
	if len(got) != 1 || got[0] != "acme:deploy" {
		t.Errorf("completeSkills(nil, %q) = %v, want [acme:deploy]", "acme", got)
	}
	// A colon-prefixed query never falls back to bare names — it would offer
	// unrelated namespaces' skills past a fixed namespace.
	if got := completeSkills(nil, "nope:"); len(got) != 0 {
		t.Errorf("completeSkills(nil, %q) = %v, want none", "nope:", got)
	}
}

func TestCompleteUsers(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedCompletionCache(t, "users", []string{"brent", "alice"})

	got := completeUsers(nil, "")
	if len(got) != 2 || got[0] != "alice" || got[1] != "brent" {
		t.Errorf("completeUsers(nil, \"\") = %v, want [alice brent]", got)
	}
	got = completeUsers(nil, "br")
	if len(got) != 1 || got[0] != "brent" {
		t.Errorf("completeUsers(nil, \"br\") = %v, want [brent]", got)
	}
	if got := completeUsers(nil, "z"); len(got) != 0 {
		t.Errorf("completeUsers(nil, \"z\") = %v, want none", got)
	}
}

func TestCompleteClaudeModel(t *testing.T) {
	seedRemoteProfile(t, "personal", "https://example.invalid", "t")
	seedCompletionCache(t, "models-claude", []string{
		"claude-sonnet-5", "claude-opus-4-6", "glm-5.3-flash",
	})

	got := completeModel(nil, "claude", "")
	if len(got) != 3 {
		t.Fatalf("completeModel(nil, claude, \"\") = %v, want all three ids", got)
	}
	got = completeModel(nil, "claude", "clau")
	if len(got) != 2 || got[0] != "claude-opus-4-6" || got[1] != "claude-sonnet-5" {
		t.Errorf("completeModel(nil, claude, \"clau\") = %v, want the two claude ids", got)
	}
}

// TestEnumCompletions pins every FixedCompletions registration Task 5.1
// added: the offered set, and that file completion is suppressed. Driving the
// real registered func (GetFlagCompletionFunc) rather than the local slice
// means a registration that never lands, or lands on a renamed flag, fails
// here instead of silently offering nothing.
func TestEnumCompletions(t *testing.T) {
	cases := []struct {
		name string
		cmd  func() *cobra.Command
		flag string
		want []string
	}{
		{"conversations stats --path", newConversationsStatsCmd, "path", []string{"proxy", "direct"}},
		{"conversations search --path", newConversationsSearchCmd, "path", []string{"proxy", "direct"}},
		{"tasks --status", newTasksCmd, "status",
			[]string{"pending", "in_progress", "blocked", "completed", "failed", "orphaned", "dropped"}},
		{"executor enroll --isolation", newExecutorEnrollCmd, "isolation",
			[]string{"none", "container", "vm"}},
		{"executor enroll --workspace-mode", newExecutorEnrollCmd, "workspace-mode",
			[]string{"ephemeral", "pinned"}},
		{"executor create --isolation", newExecutorCreateCmd, "isolation",
			[]string{"none", "container", "vm"}},
		{"executor create --workspace-mode", newExecutorCreateCmd, "workspace-mode",
			[]string{"ephemeral", "pinned"}},
		{"claude --passthrough-auth", newClaudeCmd, "passthrough-auth",
			[]string{string(passthroughAuto), string(passthroughOn), string(passthroughOff)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.cmd()
			fn, ok := cmd.GetFlagCompletionFunc(tc.flag)
			if !ok {
				t.Fatalf("no completion registered for --%s", tc.flag)
			}
			got, directive := fn(cmd, nil, "")
			if directive != cobra.ShellCompDirectiveNoFileComp {
				t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !containsCandidate(got, w) {
					t.Errorf("got %v, missing %q", got, w)
				}
			}
		})
	}
}

func containsCandidate(candidates []string, want string) bool {
	for _, c := range candidates {
		if c == want {
			return true
		}
	}
	return false
}
