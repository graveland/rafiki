// SPDX-License-Identifier: Apache-2.0

// Plugin-name derivation for a corpus: the name a checkout's skills belong to
// upstream. Pure filesystem and JSON logic -- no store, no network, no
// subprocess.
package repos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// pluginName reads the upstream plugin name out of a directory's
// .claude-plugin manifests, returning "" for anything it cannot answer
// confidently. A corpus without a manifest is the normal case, not a failure,
// so the caller decides what a nameless source is called.
//
// The name has to come from upstream's declaration rather than a guess:
// Claude Code suppresses a skills-dir plugin whose name an installed
// marketplace plugin already claims, and that deduplication is the whole
// reason an imported corpus keeps its own namespace.
func pluginName(dir string) string {
	return readPluginName(dir)
}

// readPluginName reads a plugin name out of a directory's .claude-plugin
// manifests. Returns "" for anything it cannot answer confidently -- a corpus
// without a manifest is the normal case, not a failure, and a marketplace
// declaring several plugins cannot be resolved without knowing which subpath
// the caller meant.
func readPluginName(base string) string {
	mkt := filepath.Join(base, ".claude-plugin", "marketplace.json")
	if data, err := os.ReadFile(mkt); err == nil {
		var decl struct {
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
		}
		if json.Unmarshal(data, &decl) == nil && len(decl.Plugins) == 1 {
			// The PLUGIN's name, never the marketplace's: Claude Code
			// namespaces skills by plugin, so the marketplace name would
			// deduplicate against nothing.
			return validNamespace(decl.Plugins[0].Name)
		}
	}
	pj := filepath.Join(base, ".claude-plugin", "plugin.json")
	if data, err := os.ReadFile(pj); err == nil {
		var decl struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &decl) == nil {
			return validNamespace(decl.Name)
		}
	}
	return ""
}

// validNamespace guards a namespace that came from a file rafiki did not
// write: it becomes a directory name on an executor and a prefix in a model's
// inventory, so path separators and traversal must never survive it. Returns
// "" for a name that cannot be used.
func validNamespace(n string) string {
	if n == "" || n == "." || n == ".." {
		return ""
	}
	if strings.ContainsAny(n, `/\:`+"\x00") {
		return ""
	}
	if len(n) > 64 {
		return ""
	}
	return n
}
