// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// resolveRow builds a Record whose Description names it, so a test can assert
// which layer's copy of a name survived resolution.
func resolveRow(desc, namespace, name, source, owner string, enabled bool) Record {
	return Record{
		ID:          namespace + "/" + name + "/" + desc,
		Namespace:   namespace,
		Name:        name,
		Description: desc,
		Source:      source,
		OwnerUserID: owner,
		Enabled:     enabled,
	}
}

// resolveDescs projects the resolved rows onto their Description markers.
func resolveDescs(rows []Record) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Description)
	}
	return out
}

// resolveQualified projects the resolved rows onto "namespace:name".
func resolveQualified(rows []Record) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Namespace+":"+r.Name)
	}
	return out
}

// TestResolveHigherLayerReplacesNamespaceWhole: a namespace present in the
// user layer replaces the whole namespace from the global layer, so global's
// extra name is gone even though the user never touched it.
func TestResolveHigherLayerReplacesNamespaceWhole(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("global-a", "ns", "a", "manual", "", true),
		resolveRow("global-b", "ns", "b", "manual", "", true),
		resolveRow("user-a", "ns", "a", "manual", "u1", true),
	}

	got := Resolve(rows, "u1")

	c.Len(got, 1, "expected the user layer to replace namespace ns whole, got %v", got)
	c.Eq("user-a", got[0].Description, "expected the user's row, got")
}

// TestResolveRafikiMergesPerName: DefaultNamespace is the exception to
// whole-namespace replacement — core and user rows merge name by name, so
// the user's "a" wins and core's untouched "b" survives.
func TestResolveRafikiMergesPerName(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("core-a", DefaultNamespace, "a", CoreSource, "", true),
		resolveRow("core-b", DefaultNamespace, "b", CoreSource, "", true),
		resolveRow("user-a", DefaultNamespace, "a", "manual", "u1", true),
	}

	got := Resolve(rows, "u1")

	c.Len(got, 2, "expected rafiki to merge per name, got %v", got)
	c.EqDeep([]string{"user-a", "core-b"}, resolveDescs(got), "expected the user's a and core's b, got")
}

// TestResolveIgnoresOtherOwners: rows owned by someone else are invisible —
// they neither surface nor shadow, in a plain namespace or in rafiki.
func TestResolveIgnoresOtherOwners(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("global-a", "ns", "a", "manual", "", true),
		resolveRow("other-a", "ns", "a", "manual", "u2", true),
		resolveRow("core-a", DefaultNamespace, "a", CoreSource, "", true),
		resolveRow("other-rafiki-a", DefaultNamespace, "a", "manual", "u2", true),
	}

	got := Resolve(rows, "u1")

	c.EqDeep([]string{"global-a", "core-a"}, resolveDescs(got), "expected only the rows u1 can see, got")
}

// TestResolveEmptyOwnerHasNoUserLayer: with no owner there is no user layer,
// so every owned row is another user's row and is ignored.
func TestResolveEmptyOwnerHasNoUserLayer(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("global-a", "ns", "a", "manual", "", true),
		resolveRow("user-a", "ns", "a", "manual", "u1", true),
		resolveRow("core-a", DefaultNamespace, "a", CoreSource, "", true),
		resolveRow("user-rafiki-a", DefaultNamespace, "a", "manual", "u1", true),
	}

	got := Resolve(rows, "")

	c.EqDeep([]string{"global-a", "core-a"}, resolveDescs(got), "expected no user layer at all, got")
}

// TestResolveDisabledDropped: disabled rows are dropped last, after
// resolution, so they neither surface nor drag a same-layer sibling with them.
func TestResolveDisabledDropped(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("global-a", "ns", "a", "manual", "", true),
		resolveRow("global-b-off", "ns", "b", "manual", "", false),
		resolveRow("core-a", DefaultNamespace, "a", CoreSource, "", true),
		resolveRow("core-b-off", DefaultNamespace, "b", CoreSource, "", false),
	}

	got := Resolve(rows, "u1")

	c.EqDeep([]string{"global-a", "core-a"}, resolveDescs(got), "expected only enabled rows, got")
}

// TestResolveAllDisabledStillReplaces: a user namespace whose only row is
// disabled still claims the namespace, so nothing from it survives — the
// global rows do not come back.
func TestResolveAllDisabledStillReplaces(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("user-a-off", "ns", "a", "manual", "u1", false),
		resolveRow("global-a", "ns", "a", "manual", "", true),
		resolveRow("global-b", "ns", "b", "manual", "", true),
	}

	got := Resolve(rows, "u1")

	c.Len(got, 0, "expected the disabled user namespace to hide global ns entirely, got %v", got)
}

// TestResolveDisabledRafikiOverrideHides: a disabled rafiki override hides
// the core skill of the same name. That is the whole point of the override
// state — switching a core skill off — so core must not resurface.
func TestResolveDisabledRafikiOverrideHides(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("user-a-off", DefaultNamespace, "a", "manual", "u1", false),
		resolveRow("core-a", DefaultNamespace, "a", CoreSource, "", true),
		resolveRow("core-b", DefaultNamespace, "b", CoreSource, "", true),
	}

	got := Resolve(rows, "u1")

	c.EqDeep([]string{"core-b"}, resolveDescs(got), "expected the disabled override to hide core a, got")
}

// TestResolveSorted: the result is sorted by (Namespace, Name) regardless of
// input order — it feeds the system prompt's cached prefix.
func TestResolveSorted(t *testing.T) {
	c := assert.NewAborting(t)
	rows := []Record{
		resolveRow("core-z", DefaultNamespace, "z", CoreSource, "", true),
		resolveRow("zeta-b", "zeta", "b", "manual", "", true),
		resolveRow("core-a", DefaultNamespace, "a", CoreSource, "", true),
		resolveRow("alpha-b", "alpha", "b", "manual", "", true),
		resolveRow("alpha-a", "alpha", "a", "manual", "", true),
	}

	got := Resolve(rows, "")

	c.EqDeep([]string{"alpha:a", "alpha:b", "rafiki:a", "rafiki:z", "zeta:b"},
		resolveQualified(got), "expected sorted output, got")
}
