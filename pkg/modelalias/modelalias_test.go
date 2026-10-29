// SPDX-License-Identifier: Apache-2.0

package modelalias

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// Every rejection wraps ErrInvalid, so a caller can tell "you wrote it wrong"
// from "the store is down" with errors.Is.
func TestValidateNameRejects(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, name := range []string{
		"",
		"Ds",             // uppercase
		"-ds",            // must start alphanumeric
		"a/b",            // slash
		"ds[sort=price]", // a name is never a routing spec
		strings.Repeat("a", MaxNameLen+1),
	} {
		err := ValidateName(name)
		c.Require().ErrorIs(err, ErrInvalid, "ValidateName(%q) = %v, want ErrInvalid", name, err)
	}
}

func TestValidateNameAccepts(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, name := range []string{"ds", "glm-5.3", "r1_fast", strings.Repeat("a", MaxNameLen)} {
		c.NoError(ValidateName(name), "ValidateName(%q)", name)
	}
}

// A target names a provider/id, so a bare slash-free string (the alias chain
// case) is refused, as is a malformed routing spec.
func TestValidateTargetRejects(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, target := range []string{
		"ds2",                   // no slash: would be an alias chaining to an alias
		"/x",                    // empty provider
		"openrouter/",           // empty id
		"openrouter/x[bogus=1]", // the routing parser's error, surfaced as ErrInvalid
		"",                      // empty model
	} {
		err := ValidateTarget(target)
		c.Require().ErrorIs(err, ErrInvalid, "ValidateTarget(%q) = %v, want ErrInvalid", target, err)
	}
}

func TestValidateTargetAccepts(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, target := range []string{
		"openrouter/~deepseek/deepseek-flash-latest",
		"openrouter/z-ai/glm-5.3-flash[sort=price,nodata]",
		"anthropic/claude-sonnet-5-5",
		"vmlx/qwen",
	} {
		c.NoError(ValidateTarget(target), "ValidateTarget(%q)", target)
	}
}

// IsCandidate is the cheap "could this be an alias at all?" test: a slash-free
// base. It must not parse the model -- a malformed bracket on a slashed id is
// the routing parser's error to report.
func TestIsCandidate(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, model := range []string{"ds", "ds[sort=price]"} {
		c.True(IsCandidate(model), "IsCandidate(%q) = false, want true", model)
	}
	for _, model := range []string{"", "openrouter/x", "openrouter/x[sort=price]", "[sort=price]", "openrouter/x[bogus"} {
		c.False(IsCandidate(model), "IsCandidate(%q) = true, want false", model)
	}
}

func TestMemStorePutGetListOrdering(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := NewMemStore()

	for _, kv := range [][2]string{{"zeta", "openrouter/z"}, {"alpha", "openrouter/a"}, {"mid", "anthropic/m"}} {
		if _, err := st.Put(ctx, "", kv[0], kv[1]); err != nil {
			t.Fatalf("put %s: %v", kv[0], err)
		}
	}
	got, err := st.Get(ctx, "", "mid")
	c.NoError(err, "get")
	c.Eq("mid", got.Name, "name")
	c.Eq("anthropic/m", got.Target, "target")
	c.False(got.CreatedAt.IsZero() || got.UpdatedAt.IsZero(), "timestamps must be set: %+v", got)

	list, err := st.List(ctx, "")
	c.NoError(err, "list")
	var names []string
	for _, a := range list {
		names = append(names, a.Name)
	}
	c.EqDeep([]string{"alpha", "mid", "zeta"}, names, "list must be ordered by name")

	empty, err := st.List(ctx, "nobody")
	c.NoError(err, "list empty")
	c.NotNil(empty, "List must never return nil")
	c.Len(empty, 0, "list of an unknown owner")
}

// A replace updates the target in place: CreatedAt is preserved and UpdatedAt
// moves forward -- no history is kept, by design.
func TestMemStoreReplaceKeepsCreatedAt(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := NewMemStore()

	first, err := st.Put(ctx, "", "ds", "openrouter/a")
	c.NoError(err, "first put")
	time.Sleep(2 * time.Millisecond)
	second, err := st.Put(ctx, "", "ds", "openrouter/b")
	c.NoError(err, "second put")

	c.Eq("openrouter/b", second.Target, "replace must set the new target")
	c.Eq(first.CreatedAt, second.CreatedAt, "replace must keep CreatedAt")
	c.True(second.UpdatedAt.After(first.UpdatedAt), "replace must move UpdatedAt forward: %v -> %v", first.UpdatedAt, second.UpdatedAt)

	list, err := st.List(ctx, "")
	c.NoError(err, "list")
	c.Len(list, 1, "a replace must not add a row")
	c.Eq("openrouter/b", list[0].Target, "list target after replace")
}

func TestMemStoreDelete(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := NewMemStore()

	if _, err := st.Put(ctx, "", "ds", "openrouter/a"); err != nil {
		t.Fatalf("put: %v", err)
	}
	c.NoError(st.Delete(ctx, "", "ds"), "delete")
	_, err := st.Get(ctx, "", "ds")
	c.ErrorIs(err, ErrNotFound, "get after delete")
	c.ErrorIs(st.Delete(ctx, "", "ds"), ErrNotFound, "delete of a missing alias")
}

// Owner scoping is absolute: owner A's alias is invisible to owner B and to
// the unattributed bucket, and each bucket can hold the same name independently.
func TestMemStoreOwnerScoping(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := NewMemStore()

	targets := map[string]string{"owner-a": "openrouter/a", "owner-b": "openrouter/b", "": "openrouter/x"}
	for owner, target := range targets {
		if _, err := st.Put(ctx, owner, "ds", target); err != nil {
			t.Fatalf("put for owner %q: %v", owner, err)
		}
	}
	for owner, target := range targets {
		got, err := st.Get(ctx, owner, "ds")
		c.NoError(err, "get for owner %q", owner)
		c.Eq(target, got.Target, "owner %q must read its own target", owner)
		list, err := st.List(ctx, owner)
		c.NoError(err, "list for owner %q", owner)
		c.Len(list, 1, "owner %q must see exactly its own alias", owner)
	}
	c.NoError(st.Delete(ctx, "owner-a", "ds"), "delete owner-a's")
	if _, err := st.Get(ctx, "owner-b", "ds"); err != nil {
		t.Fatalf("deleting owner-a's alias must not touch owner-b's: %v", err)
	}
	if _, err := st.Get(ctx, "owner-a", "ds"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner-a's alias survived its own delete: %v", err)
	}
}

// Put validates before writing: an invalid name or target is refused and
// leaves nothing behind.
func TestMemStorePutRefusesInvalid(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	st := NewMemStore()

	_, err := st.Put(ctx, "", "Ds", "openrouter/a")
	c.ErrorIs(err, ErrInvalid, "invalid name")
	_, err = st.Put(ctx, "", "ds", "otheralias")
	c.ErrorIs(err, ErrInvalid, "invalid target")
	list, err := st.List(ctx, "")
	c.NoError(err, "list")
	c.Len(list, 0, "a refused put must write nothing")
}
