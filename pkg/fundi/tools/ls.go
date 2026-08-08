package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	// lsMaxFiles caps how many files ls includes before truncating.
	lsMaxFiles = 1000

	lsDescription = "List files and directories in a tree structure starting from `path` " +
		"(defaults to the current working directory). Honours .gitignore. " +
		"`depth` limits traversal depth (0 = unlimited). `ignore` accepts " +
		"extra glob patterns to exclude."
)

func init() { DefaultBlueprint.Register(&LsBlueprint{}) }

// LsBlueprint is the static Tool blueprint for the ls tool.
type LsBlueprint struct{}

func (LsBlueprint) Name() string        { return "ls" }
func (LsBlueprint) Description() string { return lsDescription }
func (LsBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "Root directory to list. Defaults to the current working directory."},
			{Name: "ignore", Type: "array", Description: "Extra glob patterns to exclude from the listing.",
				Items: &Schema{Type: "string"},
			},
			{Name: "depth", Type: "integer", Description: "Maximum traversal depth. 0 = unlimited."},
		},
	}
}

func (LsBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (LsBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	return &lsTool{LsBlueprint: LsBlueprint{}, cwd: opts.Cwd, output: opts.OutputPolicy}, nil
}

type lsInput struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
	Depth  int      `json:"depth"`
}

type lsTool struct {
	LsBlueprint
	cwd    string
	output OutputPolicy
}

func (lt *lsTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in lsInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("ls: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	root := in.Path
	if root == "" {
		root = lt.cwd
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("ls: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("ls: %w", err)
	}
	if !rootInfo.IsDir() {
		return ToolResult{}, fmt.Errorf("ls: %q is not a directory", root)
	}

	// Request one more than the cap to detect truncation.
	paths, truncated, err := DiscoverFiles(ctx, FileQuery{Root: root, Limit: lsMaxFiles + 1})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ToolResult{}, ctxErr
		}
		return ToolResult{}, fmt.Errorf("ls: %w", err)
	}

	// Apply caller ignore patterns.
	if len(in.Ignore) > 0 {
		filtered := paths[:0]
		for _, p := range paths {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				continue
			}
			excluded := false
			for _, pattern := range in.Ignore {
				ok, mErr := doublestar.Match(pattern, rel)
				if mErr != nil {
					continue
				}
				if !ok && !strings.ContainsRune(pattern, '/') {
					ok, _ = doublestar.Match(pattern, filepath.Base(rel))
				}
				if ok {
					excluded = true
					break
				}
			}
			if !excluded {
				filtered = append(filtered, p)
			}
		}
		paths = filtered
	}

	if truncated {
		paths = paths[:lsMaxFiles]
	}

	if len(paths) == 0 {
		return NewTextResult(root + "\n(empty directory)"), nil
	}

	// Build a tree node set for depth-filtered tree rendering.
	tree := lsNodeFromPaths(paths, root, in.Depth)
	out := renderTree(tree, root, truncated)
	return NewTextResult(lt.output.Clip(out, "ls")), nil
}

// lsNodeFromPaths builds a tree of nodes from flat paths. Depth filtering is
// applied after the tree is built so intermediate directories always appear.
func lsNodeFromPaths(paths []string, root string, depth int) *lsNode {
	rootNode := &lsNode{name: root, isDir: true}
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		current := rootNode
		for i, part := range parts {
			isLast := i == len(parts)-1
			child := current.child(part)
			if child == nil {
				// Stat the path up to this component to determine if
				// it is a directory or a file.
				partial := filepath.Join(root, filepath.Join(parts[:i+1]...))
				info, err := os.Stat(partial)
				if err != nil {
					child = &lsNode{name: part, isDir: !isLast}
				} else {
					child = &lsNode{name: part, isDir: info.IsDir()}
				}
				current.children = append(current.children, child)
			}
			current = child
		}
	}
	if depth > 0 {
		pruneTree(rootNode, depth-1)
	}
	return rootNode
}

// pruneTree removes children at levels deeper than maxDepth (relative to root).
func pruneTree(n *lsNode, maxDepth int) {
	if maxDepth <= 0 {
		// At the depth boundary, keep the directory entry but drop its contents.
		for _, c := range n.children {
			c.children = nil
		}
		return
	}
	for _, c := range n.children {
		pruneTree(c, maxDepth-1)
	}
}

type lsNode struct {
	name     string
	isDir    bool
	children []*lsNode
}

func (n *lsNode) child(name string) *lsNode {
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

func renderTree(root *lsNode, rootPath string, truncated bool) string {
	// Sort children: directories first, then by name.
	sort.Slice(root.children, func(i, j int) bool {
		if root.children[i].isDir != root.children[j].isDir {
			return root.children[i].isDir
		}
		return root.children[i].name < root.children[j].name
	})

	var sb strings.Builder
	sb.WriteString(rootPath)
	if truncated {
		sb.WriteString(" [truncated]")
	}
	sb.WriteByte('\n')
	renderChildren(&sb, root.children, "")
	if truncated {
		sb.WriteString("(listing truncated)\n")
	}
	return sb.String()
}

func renderChildren(sb *strings.Builder, children []*lsNode, prefix string) {
	for i, c := range children {
		isLast := i == len(children)-1
		connector := "├── "
		childPrefix := "│   "
		if isLast {
			connector = "└── "
			childPrefix = "    "
		}

		display := c.name
		if c.isDir {
			display += "/"
		}
		sb.WriteString(prefix)
		sb.WriteString(connector)
		sb.WriteString(display)
		sb.WriteByte('\n')

		if c.isDir && len(c.children) > 0 {
			sort.Slice(c.children, func(i, j int) bool {
				if c.children[i].isDir != c.children[j].isDir {
					return c.children[i].isDir
				}
				return c.children[i].name < c.children[j].name
			})
			renderChildren(sb, c.children, prefix+childPrefix)
		}
	}
}
