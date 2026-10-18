// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// pluginName reads a plugin.json name.
func TestPluginNameReadsAPluginManifest(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := writeTree(t, map[string]string{
		".claude-plugin/plugin.json": `{"name":"aiguide"}`,
	})
	c.Eq("aiguide", pluginName(dir), "plugin name")
}

// A marketplace declaring exactly one plugin names it by that plugin, never by
// the marketplace: Claude Code namespaces skills by plugin.
func TestPluginNameReadsAMarketplaceManifest(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := writeTree(t, map[string]string{
		".claude-plugin/marketplace.json": `{"name":"aiguide","plugins":[{"name":"pg","source":"./"}]}`,
	})
	c.Eq("pg", pluginName(dir), "plugin name")
}

// A marketplace declaring several plugins cannot be resolved without knowing
// which subpath the caller meant, so the name is unknown rather than guessed.
func TestPluginNameIgnoresAMultiPluginMarketplace(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := writeTree(t, map[string]string{
		".claude-plugin/marketplace.json": `{"name":"aiguide","plugins":[{"name":"pg"},{"name":"ops"}]}`,
	})
	c.Eq("", pluginName(dir), "plugin name")
}

// A corpus without a manifest is the normal case, not a failure: pluginName
// answers "" and leaves the fallback to its caller.
func TestPluginNameEmptyWithoutAManifest(t *testing.T) {
	c := assert.NewCollecting(t)
	c.Eq("", pluginName(t.TempDir()), "plugin name")
}

// validNamespace accepts a directory-shaped name and refuses anything that
// would escape it.
func TestPluginNameValidNamespace(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name, want string
	}{
		{"pg", "pg"},
		{"review-swarm", "review-swarm"},
		{"Review_Swarm", "Review_Swarm"},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"", ""},
		{".", ""},
		{"..", ""},
		{"a/b", ""},
		{`a\b`, ""},
		{"a:b", ""},
		{"a\x00b", ""},
		{strings.Repeat("a", 65), ""},
	} {
		c.Eq(tc.want, validNamespace(tc.name), "validNamespace(%q)", tc.name)
	}
}
