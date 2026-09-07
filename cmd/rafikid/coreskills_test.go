// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/skills"
)

// blockOnCtxStore models a database that accepted the connection and then
// went quiet: ReplaceNamespaceSource returns only when its context dies.
type blockOnCtxStore struct{}

func (blockOnCtxStore) List(context.Context, bool) ([]skills.Record, error) { return nil, nil }
func (blockOnCtxStore) Get(context.Context, string, string) (skills.Record, error) {
	return skills.Record{}, skills.ErrNotFound
}
func (blockOnCtxStore) Upsert(_ context.Context, r skills.Record) (skills.Record, error) {
	return r, nil
}
func (blockOnCtxStore) SetEnabled(context.Context, string, string, bool) error { return nil }
func (blockOnCtxStore) Delete(context.Context, string, string) error           { return nil }
func (blockOnCtxStore) ReplaceNamespaceSource(ctx context.Context, _, _ string, _ []skills.Record) error {
	<-ctx.Done()
	return ctx.Err()
}

// The startup sync runs on the daemon's base context, which has no deadline
// of its own — without its own bound, a database that answers pings and then
// stalls hangs the whole startup. The timeout is the never-fatal contract:
// the corpus keeps last-good-wins rows and the daemon serves children.
func TestSyncCoreSkillsIsBoundedByATimeout(t *testing.T) {
	old := coreSyncTimeout
	coreSyncTimeout = 50 * time.Millisecond
	t.Cleanup(func() { coreSyncTimeout = old })

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- syncCoreSkills(context.Background(), blockOnCtxStore{}, "v-test") }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("sync against a stuck store: got %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("sync took %v; the deadline did not bound it", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sync did not return; the stuck store holds startup forever")
	}
}

func TestLoadCoreSkillsParsesTheEmbeddedCorpus(t *testing.T) {
	recs, err := loadCoreSkills()
	if err != nil {
		t.Fatalf("loadCoreSkills: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no core skills embedded; the corpus under skills/ is empty or unreadable")
	}
	for _, r := range recs {
		if r.Namespace != skills.DefaultNamespace {
			t.Errorf("%s: namespace %q, want %q", r.Name, r.Namespace, skills.DefaultNamespace)
		}
		if r.Source != skills.CoreSource {
			t.Errorf("%s: source %q, want %q", r.Name, r.Source, skills.CoreSource)
		}
		if r.Description == "" {
			t.Errorf("%s: empty description; it is the only thing the model sees in the inventory", r.Name)
		}
		if r.Body == "" {
			t.Errorf("%s: empty body", r.Name)
		}
		// The frontmatter must have been stripped: a body that still opens
		// with the delimiter would put YAML into the model's context.
		if len(r.Body) >= 3 && r.Body[:3] == "---" {
			t.Errorf("%s: body still carries its frontmatter block", r.Name)
		}
	}
}

func TestCoreSkillNamesAreStableSlugs(t *testing.T) {
	recs, err := loadCoreSkills()
	if err != nil {
		t.Fatalf("loadCoreSkills: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		if seen[r.Name] {
			t.Errorf("duplicate core skill name %q", r.Name)
		}
		seen[r.Name] = true
		for _, bad := range []string{" ", ":", "/", "\t"} {
			if strings.Contains(r.Name, bad) {
				t.Errorf("core skill name %q contains %q; names are directory names and tool arguments", r.Name, bad)
			}
		}
	}
}
