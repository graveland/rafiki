// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	rafikiv1connect "go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/table"
)

// newTagCmd is the TOP-LEVEL `rafiki tag` group: named, scoped handles to a
// point in a child's history that fork points resolve through. Creating or
// moving one is the grant to fork from it (`rafiki create --from-tag`).
//
// A tag is not a label: labels describe a child (`rafiki label`), a tag pins
// a NODE, survives the child, and is the address `--from-tag` forks from.
func newTagCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag",
		Short: "Named handles to a point in a child's history, forkable via --from-tag",
		Long: `Create, move, list and delete tags: named handles to a node in a child's
conversation history. Naming a node is the grant to fork from it —
` + "`rafiki create --from-tag <name>`" + ` starts a new fundi child from the
node the nearest visible tag of that name points at.

A tag MOVE appends a new version and keeps the history, so a fork already
made from an older position is unaffected. Only the tag's creator, that
creator's ancestors, or a user credential may move or untag one.`,
	}
	cmd.AddCommand(newTagListCmd(), newTagAddCmd(), newTagMoveCmd(), newTagRmCmd())
	return cmd
}

// parseTagCursor validates --at's "m:<ordinal>" (or "m:<ordinal>.<sub>", the
// message-within-row spelling agent_view prints, which a fork ignores). An
// empty flag means the child's last completed turn. Shape-only, mirroring the
// daemon's parseForkCursor: which ordinals are completed turns is the
// daemon's to judge.
func parseTagCursor(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	rest, ok := strings.CutPrefix(v, "m:")
	if !ok || rest == "" {
		return "", fmt.Errorf("--at must look like m:<ordinal> (got %q)", v)
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		if !allDigits(rest[i+1:]) {
			return "", fmt.Errorf("--at must look like m:<ordinal> (got %q)", v)
		}
		rest = rest[:i]
	}
	if !allDigits(rest) {
		return "", fmt.Errorf("--at must look like m:<ordinal> (got %q)", v)
	}
	return v, nil
}

// parseTagScope maps --scope's spelling onto the wire enum. Empty means
// UNSPECIFIED, which the daemon reads as lineage on create.
func parseTagScope(v string) (rafikiv1.TagScope, error) {
	switch v {
	case "":
		return rafikiv1.TagScope_TAG_SCOPE_UNSPECIFIED, nil
	case "lineage":
		return rafikiv1.TagScope_TAG_SCOPE_LINEAGE, nil
	case "tree":
		return rafikiv1.TagScope_TAG_SCOPE_TREE, nil
	case "owner":
		return rafikiv1.TagScope_TAG_SCOPE_OWNER, nil
	default:
		return rafikiv1.TagScope_TAG_SCOPE_UNSPECIFIED, fmt.Errorf("--scope must be lineage, tree or owner (got %q)", v)
	}
}

// tagScopeCell renders a scope for the list table; UNSPECIFIED never stores,
// so it only shows on a refused create.
func tagScopeCell(s rafikiv1.TagScope) string {
	switch s {
	case rafikiv1.TagScope_TAG_SCOPE_LINEAGE:
		return "lineage"
	case rafikiv1.TagScope_TAG_SCOPE_TREE:
		return "tree"
	case rafikiv1.TagScope_TAG_SCOPE_OWNER:
		return "owner"
	default:
		return "?"
	}
}

// resolveTagArg turns a `move`/`rm`/`--history` argument into a tag id: a
// UUID names the id directly, anything else is a name resolved through the
// caller's nearest-first listing (the same resolution --from-tag does at
// fork time). No match answers one error naming the argument.
//
// The row it answers is the NEWEST LIVE version when one exists — a tombstone
// or a superseded row is only answered for --history on a fully-deleted tag,
// where the delete/move refusals downstream are the honest outcome.
func resolveTagArg(ctx context.Context, ctrl rafikiv1connect.ControlClient, arg string) (*rafikiv1.TagRow, error) {
	// NAME first: the grammar admits hex and dashes, so a UUID-SHAPED NAME is
	// a legal tag ("1-64 of [a-z0-9._-]") and must not be captured by the id
	// path. The name is also what --from-tag resolves by, so the two verbs
	// cannot disagree. Only when no visible name matches does the argument get
	// a second reading as a tag id.
	// 500 is the listing cap: an exact match must not fall outside the page.
	resp, err := ctrl.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{
		NamePrefix: arg,
		Limit:      500,
	}))
	if err != nil {
		return nil, err
	}
	for _, t := range resp.Msg.GetRows() {
		if t.GetName() == arg {
			return t, nil
		}
	}
	if _, err := uuid.Parse(arg); err != nil {
		return nil, fmt.Errorf("no visible tag named %q", arg)
	}
	idResp, err := ctrl.ListTags(ctx, connect.NewRequest(&rafikiv1.ListTagsRequest{TagId: arg}))
	if err != nil {
		return nil, err
	}
	rows := idResp.Msg.GetRows()
	if len(rows) == 0 {
		return nil, fmt.Errorf("no tag %s", arg)
	}
	return newestLiveTagRow(rows), nil
}

// newestLiveTagRow picks the last row that is neither superseded nor deleted
// (history is oldest first), falling back to the very last row — the caller's
// failure paths then name the real state.
func newestLiveTagRow(rows []*rafikiv1.TagRow) *rafikiv1.TagRow {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].GetSupersededAt() == nil && rows[i].GetDeletedAt() == nil {
			return rows[i]
		}
	}
	if len(rows) > 0 {
		return rows[len(rows)-1]
	}
	return nil
}

func newTagListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tags visible to you",
		Long: `List the live tags visible to you, nearest scope first (lineage, then tree,
then owner). --history <name-or-id> shows one tag's whole version history,
oldest first, tombstones included.`,
		Args: cobra.NoArgs,
		RunE: runTagList,
	}
	cmd.Flags().String("prefix", "", "Only tags whose name starts with this")
	cmd.Flags().String("history", "", "Show one tag's whole history (name or tag id) instead of the live set")
	cmd.Flags().Int("limit", 0, "Cap the rows (default 50, max 500)")
	_ = cmd.RegisterFlagCompletionFunc("history", completeTagNames)
	_ = cmd.RegisterFlagCompletionFunc("prefix", completeTagNames)
	return cmd
}

func runTagList(cmd *cobra.Command, args []string) error {
	mode, color, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)

	prefix, _ := cmd.Flags().GetString("prefix")
	history, _ := cmd.Flags().GetString("history")
	limit, _ := cmd.Flags().GetInt("limit")

	req := &rafikiv1.ListTagsRequest{NamePrefix: prefix}
	if limit > 0 {
		req.Limit = int32(limit)
	}
	if history != "" {
		row, err := resolveTagArg(ctx, ep.control(), history)
		if err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
		req = &rafikiv1.ListTagsRequest{TagId: row.GetTagId()}
	}

	resp, err := ep.control().ListTags(ctx, connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	rows := resp.Msg.GetRows()

	if mode == outputJSON || mode == outputJSONL {
		return emitProtoRows(os.Stdout, rows, mode)
	}

	tb := table.New(os.Stdout, table.Options{Color: color})
	tb.Header("NAME", "SCOPE", "CHILD", "AT", "CREATED", "NOTE")
	for _, t := range rows {
		created := "-"
		if ts := t.GetCreatedAt(); ts != nil && ts.AsTime().Unix() > 0 {
			created = ts.AsTime().UTC().Format("2006-01-02 15:04")
		}
		tb.Row(
			t.GetName(),
			tagScopeCell(t.GetScope()),
			t.GetChildId(),
			tagAtCell(t),
			created,
			t.GetNote(),
		)
	}
	return tb.Render()
}

// tagAtCell renders the node a row points at: "m:<ordinal>", or "last" when
// the cursor was empty at create time (the node, not the spelling, is stored).
func tagAtCell(t *rafikiv1.TagRow) string {
	if t.GetNodeId() == "" {
		return "-"
	}
	return fmt.Sprintf("m:%d", t.GetOrdinal())
}

func newTagAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <child> <name>",
		Short: "Tag a point in a child's history",
		Long: `Tag the point --at names in a child's history (default: the child's last
completed turn). The name is 1-64 characters and, within its scope, unique —
a name already held answers an error unless --replace tombstones the live
holder first (user credentials only).

Scope says who may fork from the tag: lineage (default) limits it to the
tagging child's ancestors and descendants, tree to everything under the
tagger's top-level ancestor, owner to every child of the owner.`,
		Args: cobra.ExactArgs(2),
		RunE: runTagAdd,
	}
	cmd.Flags().String("at", "", "Node to tag, as m:<ordinal> (default: the child's last completed turn)")
	cmd.Flags().String("scope", "", "Who may fork from it: lineage (default), tree or owner")
	cmd.Flags().String("note", "", "Free text carried on the tag")
	cmd.Flags().Bool("replace", false, "Tombstone the live holder of this name first (user credentials only)")
	_ = cmd.RegisterFlagCompletionFunc("at", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("scope", cobra.FixedCompletions([]string{"lineage", "tree", "owner"}, cobra.ShellCompDirectiveNoFileComp))
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runTagAdd(cmd *cobra.Command, args []string) error {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)

	childID, err := resolveTargetConnect(ctx, ep.control(), mustProfile(cmd).Name, args[0], ep.describe)
	if err != nil {
		return err
	}
	name := args[1]
	note, _ := cmd.Flags().GetString("note")
	replace, _ := cmd.Flags().GetBool("replace")
	at, _ := cmd.Flags().GetString("at")
	cursor, err := parseTagCursor(at)
	if err != nil {
		return err
	}
	scopeFlag, _ := cmd.Flags().GetString("scope")
	scope, err := parseTagScope(scopeFlag)
	if err != nil {
		return err
	}

	resp, err := ep.control().Tag(ctx, connect.NewRequest(&rafikiv1.TagRequest{
		Name:    name,
		Scope:   scope,
		ChildId: childID,
		Cursor:  cursor,
		Note:    note,
		Replace: replace,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	dropTagCompletionCache(cmd)

	t := resp.Msg.GetTag()
	if mode == outputJSON || mode == outputJSONL {
		return emitProto(os.Stdout, t, mode)
	}
	fmt.Fprintf(os.Stdout, "tagged %s %s at %s (scope %s, id %s)\n",
		childID, name, tagAtCell(t), tagScopeCell(t.GetScope()), t.GetTagId())
	return nil
}

func newTagMoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "move <name> <child>",
		Short: "Move a tag to another point (compare-and-swap)",
		Long: `Move a tag to the point --at names in a child's history (default: that
child's last completed turn). The move is a compare-and-swap: this verb reads
the tag's current node first and moves it only if it still points there, so
two concurrent moves cannot silently interleave — a loser answers
failed_precondition and re-runs ` + "`rafiki tag list --history`" + ` to see who won.

Every move appends a version; the old position stays in the history.`,
		Args: cobra.ExactArgs(2),
		RunE: runTagMove,
	}
	cmd.Flags().String("at", "", "Node to move to, as m:<ordinal> (default: the child's last completed turn)")
	_ = cmd.RegisterFlagCompletionFunc("at", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeTagNames(cmd, nil, toComplete)
		}
		if len(args) == 1 {
			return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runTagMove(cmd *cobra.Command, args []string) error {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)

	// The CAS expectation is the CURRENT node: read it, then move. Two moves
	// racing both read the same node; only the first commit wins, the second
	// answers failed_precondition rather than repointing silently.
	current, err := resolveTagArg(ctx, ep.control(), args[0])
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	childID, err := resolveTargetConnect(ctx, ep.control(), mustProfile(cmd).Name, args[1], ep.describe)
	if err != nil {
		return err
	}
	at, _ := cmd.Flags().GetString("at")
	cursor, err := parseTagCursor(at)
	if err != nil {
		return err
	}

	resp, err := ep.control().Tag(ctx, connect.NewRequest(&rafikiv1.TagRequest{
		TagId:        current.GetTagId(),
		ExpectNodeId: current.GetNodeId(),
		ChildId:      childID,
		Cursor:       cursor,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	dropTagCompletionCache(cmd)

	t := resp.Msg.GetTag()
	if mode == outputJSON || mode == outputJSONL {
		return emitProto(os.Stdout, t, mode)
	}
	fmt.Fprintf(os.Stdout, "moved %s to %s %s (id %s)\n",
		t.GetName(), childID, tagAtCell(t), t.GetTagId())
	return nil
}

func newTagRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <name-or-id>",
		Short: "Delete a tag",
		Long: `Delete a tag by name (your nearest visible one) or by tag id. The tombstone
is permanent: forks already made from it are unaffected, but nothing can fork
from the name until a new tag takes it.`,
		Args: cobra.ExactArgs(1),
		RunE: runTagRm,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeTagNames(cmd, nil, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runTagRm(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)

	row, err := resolveTagArg(ctx, ep.control(), args[0])
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	if _, err := ep.control().Untag(ctx, connect.NewRequest(&rafikiv1.UntagRequest{TagId: row.GetTagId()})); err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	dropTagCompletionCache(cmd)
	fmt.Fprintf(os.Stdout, "removed %s (id %s)\n", row.GetName(), row.GetTagId())
	return nil
}
