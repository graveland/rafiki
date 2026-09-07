// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	rafiki "go.graveland.dev/rafiki"
	"go.graveland.dev/rafiki/pkg/skills"
)

// coreSkillsFS carries rafiki's own skill corpus into the binary. The embed is
// a CARRIER, not a serving tier: everything downstream reads the database, so
// there is one authority and one management surface. Keeping the source in git
// is what lets CLAUDE.md's doc-sync rule apply — changing a tool's parameters
// updates its skill in the same commit.
//
// The embed directive itself lives in the module-root package (rafiki.Skills):
// a go:embed pattern cannot cross a package boundary, and the corpus is kept at
// the repo root for review like code.
var coreSkillsFS = rafiki.Skills

// loadCoreSkills parses the embedded corpus into rows ready for the store.
//
// A malformed skill is fatal here, unlike DiscoverSkills' tolerance of a bad
// file on an operator's disk: this corpus ships inside the binary, so a broken
// entry is a build-time mistake that must not reach a running fleet quietly.
func loadCoreSkills() ([]skills.Record, error) {
	entries, err := coreSkillsFS.ReadDir("skills")
	if err != nil {
		return nil, fmt.Errorf("read embedded skills: %w", err)
	}
	out := make([]skills.Record, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := path.Join("skills", e.Name(), "SKILL.md")
		data, err := fs.ReadFile(coreSkillsFS, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		name, description, body, err := skills.ParseSkillFile(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if name != e.Name() {
			return nil, fmt.Errorf("%s: frontmatter name %q does not match its directory %q", p, name, e.Name())
		}
		if strings.ContainsAny(name, " :/\t") {
			return nil, fmt.Errorf("%s: name %q must be a slug (no spaces, colons or slashes)", p, name)
		}
		out = append(out, skills.Record{
			Namespace:   skills.DefaultNamespace,
			Name:        name,
			Description: description,
			Body:        body,
			Source:      skills.CoreSource,
			Enabled:     true,
		})
	}
	return out, nil
}

// coreSyncTimeout bounds the startup sync, warnStaleOverrides included. A
// var rather than a const so a test can shrink it; a sync that misses its
// deadline leaves last-good-wins rows in place, exactly as a failed one does.
var coreSyncTimeout = 30 * time.Second

// syncCoreSkills reconciles the embedded corpus into the store, owning exactly
// (rafiki, rafiki-core) and nothing else.
//
// It runs at every daemon startup rather than as a deploy step. In a
// mixed-version fleet that means the rows follow whichever daemon started last,
// for the length of a rollout — but the sync is byte-stable, so a converged
// fleet writes nothing, and nobody can forget to run it. A silently empty
// corpus is a worse failure than a briefly inconsistent one.
//
// Never fatal: a daemon that cannot reach its store still serves children.
// The timeout is part of that never-fatal contract — the sync runs on the
// daemon's base context, which has no deadline of its own.
func syncCoreSkills(ctx context.Context, st skills.Store, version string) error {
	if st == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, coreSyncTimeout)
	defer cancel()
	want, err := loadCoreSkills()
	if err != nil {
		return fmt.Errorf("load core skills: %w", err)
	}
	if err := st.ReplaceNamespaceSource(ctx, skills.DefaultNamespace, skills.CoreSource, want); err != nil {
		return fmt.Errorf("sync core skills: %w", err)
	}
	warnStaleOverrides(ctx, st, want, version)
	return nil
}

// warnStaleOverrides announces an operator override whose core skill has moved
// on. An override is allowed deliberately, but its risk is that it silently
// misdescribes machinery that changed underneath it — so the staleness is
// reported rather than left to be discovered by a confused agent.
func warnStaleOverrides(ctx context.Context, st skills.Store, core []skills.Record, version string) {
	rows, err := st.List(ctx, true)
	if err != nil {
		return
	}
	coreNames := make(map[string]bool, len(core))
	for _, r := range core {
		coreNames[r.Name] = true
	}
	for _, r := range rows {
		if r.Source == skills.CoreSource || !coreNames[r.Name] {
			continue
		}
		if r.ShadowedCoreVersion != "" && r.ShadowedCoreVersion != version {
			slog.Warn("skill override may be stale: it replaces a core skill that has changed since",
				"skill", r.Namespace+":"+r.Name,
				"overrideWrittenFor", r.ShadowedCoreVersion,
				"currentVersion", version)
		}
	}
}
