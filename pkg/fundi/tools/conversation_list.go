// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

func init() { DefaultBlueprint.Register(&ConversationListBlueprint{}) }

const conversationListDescription = "List conversations (metadata only, no content), open, exited and closed alike, newest activity first. " +
	"Rows nest: an unfiltered call returns top-level conversations, each with child_count; " +
	"pass parent_conversation_id to list one row's children. " +
	"Use conversation_export or recall to read content. " +
	"Pass next_cursor back as cursor for the next page."

// listSorts are the orderings conversation_list accepts. They map one-to-one
// onto the reader's insights.ListSort values.
var listSorts = []string{"recent", "created", "relevance"}

// listStates are the agent states conversation_list filters on. They are the
// insights.ListState vocabulary verbatim.
var listStates = []string{"open", "exited", "closed", "no_agent"}

type ConversationListBlueprint struct{}

func (ConversationListBlueprint) Name() string        { return "conversation_list" }
func (ConversationListBlueprint) Description() string { return conversationListDescription }
func (ConversationListBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "limit", Type: "integer",
				Description: "Max rows per page (default 50, maximum 500)."},
			{Name: "cursor", Type: "string",
				Description: "Opaque next_cursor from the previous page; pass it back unchanged to continue."},
			{Name: "sort", Type: "string", Enum: listSorts,
				Description: "Row order: \"recent\" (last activity, default), \"created\" or \"relevance\" (best match for query first)."},
			{Name: "parent_conversation_id", Type: "string",
				Description: "List this conversation's children instead of the top level."},
			{Name: "query", Type: "string",
				Description: "Content match over conversation text; required for sort=relevance."},
			{Name: "states", Type: "array",
				Description: "Only rows whose linked agent is in one of these states: \"open\", \"exited\", \"closed\", \"no_agent\". Omit for all.",
				Items:       &Schema{Type: "string"}},
			{Name: "kinds", Type: "array",
				Description: "Only rows whose agent is one of these kinds (e.g. \"fundi\", \"claude\"). Omit for all; a non-empty list excludes rows with no agent.",
				Items:       &Schema{Type: "string"}},
			{Name: "name_contains", Type: "string",
				Description: "Substring match against the conversation name or its agent's name."},
			{Name: "cwd_contains", Type: "string",
				Description: "Substring match against the conversation working directory or its agent's."},
			{Name: "labels", Type: "object",
				Description: "Only rows whose agent carries all these labels (exact key/value pairs)."},
			{Name: "since", Type: "string",
				Description: "RFC3339 time; only conversations with activity at or after this time."},
			{Name: "until", Type: "string",
				Description: "RFC3339 time; only conversations with activity before this time."},
		},
	}
}
func (ConversationListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (ConversationListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Conversations == nil {
		return nil, nil
	}
	return &conversationListTool{reader: opts.Conversations}, nil
}

// conversationListArgs is the raw JSON the conversation_list tool binds.
// Since/Until stay strings so a malformed bound can be refused naming the
// argument: rfc3339Time's parse error carries no field context, because
// encoding/json reports a custom unmarshaller's error verbatim. An EMPTY
// string -- a blanked optional argument -- is UNSET (no bound), the same rule
// as conversation_search. Multi-word fields carry explicit json tags:
// encoding/json does not cross underscores, so without them the schema's
// advertised argument would never bind.
type conversationListArgs struct {
	Limit                int               `json:"limit"`
	Cursor               string            `json:"cursor"`
	Sort                 string            `json:"sort"`
	ParentConversationID string            `json:"parent_conversation_id"`
	Query                string            `json:"query"`
	States               []string          `json:"states"`
	Kinds                []string          `json:"kinds"`
	NameContains         string            `json:"name_contains"`
	CwdContains          string            `json:"cwd_contains"`
	Labels               map[string]string `json:"labels"`
	Since, Until         string
}

type conversationListTool struct {
	ConversationListBlueprint
	reader ConversationReader
}

func (t *conversationListTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var args conversationListArgs
	if err := in.Unmarshal(&args); err != nil {
		return ToolResult{}, fmt.Errorf("conversation_list: %w", err)
	}
	if args.Sort != "" && !slices.Contains(listSorts, args.Sort) {
		return ToolResult{}, fmt.Errorf("conversation_list: unknown sort %q; valid values are %s",
			args.Sort, strings.Join(listSorts, ", "))
	}
	for _, s := range args.States {
		if !slices.Contains(listStates, s) {
			return ToolResult{}, fmt.Errorf("conversation_list: unknown state %q in states; valid values are %s",
				s, strings.Join(listStates, ", "))
		}
	}
	since, err := listBound("since", args.Since)
	if err != nil {
		return ToolResult{}, err
	}
	until, err := listBound("until", args.Until)
	if err != nil {
		return ToolResult{}, err
	}
	page, err := t.reader.ConversationList(ctx, ConversationListQuery{
		Limit: args.Limit, Cursor: args.Cursor, Sort: args.Sort,
		ParentConversationID: args.ParentConversationID, Query: args.Query,
		States: args.States, Kinds: args.Kinds,
		NameContains: args.NameContains, CwdContains: args.CwdContains,
		Labels: args.Labels, Since: since, Until: until,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("conversation_list: %w", err)
	}
	if page.Rows == nil {
		page.Rows = []ConversationListRow{}
	}
	b, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		return ToolResult{}, fmt.Errorf("conversation_list: marshal: %w", err)
	}
	return NewTextResult(string(b)), nil
}

// listBound parses one optional RFC3339 time bound, naming the argument in
// the error. An empty string is unbounded, never an error.
func listBound(field, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("conversation_list: %s must be an RFC3339 time: %w", field, err)
	}
	return &t, nil
}
