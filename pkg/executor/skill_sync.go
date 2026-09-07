// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"gopkg.in/yaml.v3"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// managedMarker names a file rafiki drops in every namespace directory it
// writes. It is what distinguishes "a tree we own and may replace" from "the
// operator's own skill directory", and it is checked before every removal.
//
// Ownership by marker rather than by a central manifest: a manifest is state
// that can disagree with the filesystem, and the disagreement is always
// discovered at the moment of an rm.
const managedMarker = ".rafiki-managed"

// claudeSkillsDir resolves where Claude Code will look for skills on this
// machine: $CLAUDE_CONFIG_DIR/skills, else ~/.claude/skills.
//
// This is correct by construction rather than by agreement between components.
// The executor's own process environment is what a launched claude child
// inherits: paths.LoadEnvFile/LoadEnvFileOverrides apply executor.env and
// executor-overrides.env to this process, AdminService.Launch starts daraja
// with cmd.Env = append(os.Environ(), ...), and daraja calls
// proxyenv.Claude(os.Environ(), ...) where CLAUDE_CONFIG_DIR is in neither
// Managed nor Credentials and so passes through untouched.
//
// Do NOT point this at a private directory: ~/.claude is where Claude Code
// keeps the OAuth credential --passthrough-auth depends on, and moving it
// silently breaks credential discovery.
func claudeSkillsDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "skills")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

// validSegment guards a name that arrived over the wire and is about to become
// a path segment. Checked at BOTH the write and the delete call site: a check
// written once, far from the RemoveAll, is the shape this class of bug takes.
func validSegment(n string) error {
	switch {
	case n == "":
		return errors.New("empty name")
	case n == "." || n == "..":
		return fmt.Errorf("reserved name %q", n)
	case strings.ContainsAny(n, `/\`+"\x00"):
		return fmt.Errorf("name %q contains a path separator", n)
	case strings.Contains(n, ":"):
		return fmt.Errorf("name %q contains a colon; the namespace prefix is derived, never written", n)
	case len(n) > 64:
		return fmt.Errorf("name %q exceeds 64 bytes", n)
	}
	return nil
}

// renderSkillMd builds a SKILL.md byte-for-byte deterministically.
//
// yaml.Marshal rather than fmt: a description containing a colon, a quote or a
// newline breaks naive formatting and would produce a file Claude Code cannot
// parse — silently, since a skill with unreadable frontmatter is skipped
// rather than reported.
func renderSkillMd(name, description, body string) ([]byte, error) {
	fm, err := yaml.Marshal(map[string]string{"name": name, "description": description})
	if err != nil {
		return nil, fmt.Errorf("marshal frontmatter for %s: %w", name, err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// SyncSkills replaces this executor's rafiki-managed skill trees with req's
// corpus. See the RPC's doc comment in executor.proto for why it is
// whole-corpus rather than incremental.
func (s *Server) SyncSkills(
	_ context.Context,
	req *connect.Request[executorpb.SyncSkillsRequest],
) (*connect.Response[executorpb.SyncSkillsResponse], error) {
	if !s.opts.SkillsSync {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("this executor does not accept skill syncs"))
	}

	root := claudeSkillsDir()
	if root == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("cannot resolve a claude config directory on this executor"))
	}

	// Validate EVERYTHING before touching the filesystem: a partial apply that
	// aborts halfway leaves a corpus no daemon ever published.
	want := make(map[string]bool, len(req.Msg.GetNamespaces()))
	for _, ns := range req.Msg.GetNamespaces() {
		if err := validSegment(ns.GetName()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("namespace: %w", err))
		}
		if want[ns.GetName()] {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("namespace %q appears twice", ns.GetName()))
		}
		want[ns.GetName()] = true
		for _, sk := range ns.GetSkills() {
			if err := validSegment(sk.GetName()); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("skill in namespace %q: %w", ns.GetName(), err))
			}
		}
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("create skills dir: %w", err))
	}

	var written, pruned int32
	for _, ns := range req.Msg.GetNamespaces() {
		changed, err := s.writeNamespace(root, ns)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if changed {
			written++
		}
	}

	n, err := pruneUnwantedNamespaces(root, want)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pruned = n

	return connect.NewResponse(&executorpb.SyncSkillsResponse{
		Written: written, Pruned: pruned, SkillsDir: root,
	}), nil
}

// writeNamespace stages a whole namespace tree in a sibling directory and
// renames it into place, so a claude child launching mid-sync never observes a
// half-written corpus. Reports whether anything actually changed.
func (s *Server) writeNamespace(root string, ns *executorpb.SkillNamespace) (bool, error) {
	final := filepath.Join(root, ns.GetName())

	staged, err := os.MkdirTemp(root, ".rafiki-staging-*")
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", ns.GetName(), err)
	}
	defer os.RemoveAll(staged) // no-op once the rename has moved it

	if err := os.WriteFile(filepath.Join(staged, managedMarker), []byte(ns.GetName()+"\n"), 0o644); err != nil {
		return false, fmt.Errorf("mark %s: %w", ns.GetName(), err)
	}

	pluginDir := filepath.Join(staged, ".claude-plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return false, fmt.Errorf("plugin dir for %s: %w", ns.GetName(), err)
	}
	// Hand-built rather than json.Marshal so key order is fixed: a map would
	// serialise deterministically today and is not promised to, and a reordered
	// manifest would make every sync look like a change.
	manifest := fmt.Sprintf("{\n  \"name\": %q,\n  \"version\": %q\n}\n",
		ns.GetName(), ns.GetVersion())
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		return false, fmt.Errorf("plugin.json for %s: %w", ns.GetName(), err)
	}

	for _, sk := range ns.GetSkills() {
		content, err := renderSkillMd(sk.GetName(), sk.GetDescription(), sk.GetBody())
		if err != nil {
			return false, err
		}
		dir := filepath.Join(staged, sk.GetName())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("mkdir %s/%s: %w", ns.GetName(), sk.GetName(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), content, 0o644); err != nil {
			return false, fmt.Errorf("write %s/%s: %w", ns.GetName(), sk.GetName(), err)
		}
	}

	same, err := treesIdentical(final, staged)
	if err != nil {
		return false, err
	}
	if same {
		// Byte-stable: leave the live tree alone so Claude Code's file watching
		// stays quiet across a converged fleet's restarts.
		return false, nil
	}

	// Only ever replace a tree we own. An unmanaged directory of the same name
	// is the operator's; refuse rather than clobber it.
	if err := assertManagedOrAbsent(final); err != nil {
		return false, err
	}
	if err := os.RemoveAll(final); err != nil {
		return false, fmt.Errorf("replace %s: %w", ns.GetName(), err)
	}
	if err := os.Rename(staged, final); err != nil {
		return false, fmt.Errorf("publish %s: %w", ns.GetName(), err)
	}
	return true, nil
}

// assertManagedOrAbsent refuses to replace or remove a path rafiki did not
// write. This is the entire safety argument for writing into a directory that
// also holds an operator's own skills.
func assertManagedOrAbsent(dir string) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	// Lstat, not Stat: a symlink must be judged as itself. Following one would
	// let a link planted in the skills dir redirect a RemoveAll anywhere.
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory; refusing to replace it", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, managedMarker)); err != nil {
		return fmt.Errorf("%s is not rafiki-managed; refusing to replace it", dir)
	}
	return nil
}

// pruneUnwantedNamespaces removes managed namespace trees absent from want,
// and touches nothing else.
func pruneUnwantedNamespaces(root string, want map[string]bool) (int32, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read skills dir: %w", err)
	}
	var pruned int32
	for _, e := range entries {
		name := e.Name()
		if want[name] {
			continue
		}
		// Re-validate before an rm even though this name came off the local
		// filesystem: the delete call site does its own checking, always.
		if validSegment(name) != nil {
			continue
		}
		dir := filepath.Join(root, name)
		if err := assertManagedOrAbsent(dir); err != nil {
			continue // the operator's; not ours to remove
		}
		if err := os.RemoveAll(dir); err != nil {
			return pruned, fmt.Errorf("prune %s: %w", name, err)
		}
		pruned++
	}
	return pruned, nil
}

// treesIdentical reports whether the live tree already holds exactly the staged
// content. Absent live tree is not identical.
func treesIdentical(live, staged string) (bool, error) {
	liveFiles, err := collectTree(live)
	if err != nil {
		return false, err
	}
	if liveFiles == nil {
		return false, nil
	}
	stagedFiles, err := collectTree(staged)
	if err != nil {
		return false, err
	}
	if len(liveFiles) != len(stagedFiles) {
		return false, nil
	}
	for p, content := range stagedFiles {
		if liveFiles[p] != content {
			return false, nil
		}
	}
	return true, nil
}

func collectTree(root string) (map[string]string, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return out, nil
}
