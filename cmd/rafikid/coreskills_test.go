// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewAborting(t)
	old := coreSyncTimeout
	coreSyncTimeout = 50 * time.Millisecond
	t.Cleanup(func() { coreSyncTimeout = old })

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- syncCoreSkills(context.Background(), blockOnCtxStore{}, "v-test") }()

	select {
	case err := <-done:
		c.ErrorIs(err, context.DeadlineExceeded, "sync against a stuck store: got")
		c.LessOrEqual(10*time.Second, time.Since(start), "sync took")
	case <-time.After(10 * time.Second):
		t.Fatal("sync did not return; the stuck store holds startup forever")
	}
}

func TestLoadCoreSkillsParsesTheEmbeddedCorpus(t *testing.T) {
	c := assert.NewCollecting(t)
	recs, err := loadCoreSkills()
	c.Require().NoError(err, "loadCoreSkills")
	c.Require().NotEmpty(recs, "no core skills embedded; the corpus under skills/ is empty or unreadable")
	for _, r := range recs {
		c.Eq(skills.DefaultNamespace, r.Namespace, "%s: namespace %q, want", r.Name, r.Namespace)
		c.Eq(skills.CoreSource, r.Source, "%s: source %q, want", r.Name, r.Source)
		c.NotEq("", r.Description, "%s: empty description; it is the only thing the model sees in the inventory", r.Name)
		c.NotEq("", r.Body, "%s: empty body", r.Name)
		// The frontmatter must have been stripped: a body that still opens
		// with the delimiter would put YAML into the model's context.
		c.False(len(r.Body) >= 3 && r.Body[:3] == "---", "%s: body still carries its frontmatter block", r.Name)
	}
}

func TestCoreSkillNamesAreStableSlugs(t *testing.T) {
	c := assert.NewCollecting(t)
	recs, err := loadCoreSkills()
	c.Require().NoError(err, "loadCoreSkills")
	seen := map[string]bool{}
	for _, r := range recs {
		c.False(seen[r.Name], "duplicate core skill name %q", r.Name)
		seen[r.Name] = true
		for _, bad := range []string{" ", ":", "/", "\t"} {
			c.NotStrContains(r.Name, bad, "core skill name")
		}
	}
}
