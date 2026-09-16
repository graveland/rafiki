package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/conversationview"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/table"
)

func newConversationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "conversations",
		Aliases: []string{"c", "conv"},
		Short:   "Query persisted conversation history from the daemon's agent database",
		Long: `Global stats, search, transcript export, named catalogue queries, and the
conversation review verbs over the conversations schema the daemon persists
to when RAFIKI_DB is set. Unlike "rafiki search" (live, in-memory,
currently-running children only), these query history in Postgres regardless
of whether anything is still running.

Output matches "rafikid agent stats|search|export|query" exactly — same queries,
same renderers, only the transport differs. --output controls the format: tables
at a terminal, JSON when piped.`,
	}
	cmd.AddCommand(
		newConversationsStatsCmd(),
		newConversationsSearchCmd(),
		newConversationsExportCmd(),
		newConversationsQueryCmd(),
		newConversationsReviewCmd(),
		newConversationsFindingsCmd(),
	)
	return cmd
}

// bindConversationFilterFlags registers the filter flags shared by stats and
// search, matching rafiki agent's flag names exactly.
//
// --path gets a closed-set completion because its values are an enum. --persona
// and --source deliberately get none: their value sets are whatever the
// database happens to contain, and a wrong closed list is worse than none.
func bindConversationFilterFlags(cmd *cobra.Command) {
	cmd.Flags().String("since", "", "RFC3339 timestamp or duration like 24h")
	cmd.Flags().String("until", "", "RFC3339 timestamp or duration like 24h")
	cmd.Flags().String("owner", "", "filter by owner username")
	cmd.Flags().String("persona", "", "filter by persona")
	cmd.Flags().String("source", "", "filter by source")
	cmd.Flags().String("model", "", "filter by model")
	cmd.Flags().String("path", "", `filter by path ("proxy" or "direct")`)
	_ = cmd.RegisterFlagCompletionFunc("path", cobra.FixedCompletions(
		[]string{"proxy", "direct"}, cobra.ShellCompDirectiveNoFileComp))
}

// conversationFilterVals reads the shared filter flags into an
// conversationview.FilterVals, the same flag-value bag rafiki agent binds from.
func conversationFilterVals(cmd *cobra.Command) conversationview.FilterVals {
	v := conversationview.FilterVals{}
	v.Since, _ = cmd.Flags().GetString("since")
	v.Until, _ = cmd.Flags().GetString("until")
	v.Owner, _ = cmd.Flags().GetString("owner")
	v.Persona, _ = cmd.Flags().GetString("persona")
	v.Source, _ = cmd.Flags().GetString("source")
	v.Model, _ = cmd.Flags().GetString("model")
	v.Path, _ = cmd.Flags().GetString("path")
	return v
}

// unixOrZero converts a resolved filter timestamp to the wire's Unix-seconds
// convention, where 0 means unset.
func unixOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

// unixPtrOrNil converts a resolved filter timestamp to the Connect wire's
// optional int64: nil means unset, so the field is absent rather than sent as
// 0. The proto's optional int64 generates *int64, where a bare unixOrZero
// would wrongly send "unset" as a present zero.
func unixPtrOrNil(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	u := t.Unix()
	return &u
}

// conversationsMode maps the global --output flag (and its -j/-J shorthands)
// onto the render mode shared with `rafikid agent`. Table is the default on
// TTY and pipe alike; JSON (or JSONL, rendered as JSON here) only on request.
func conversationsMode(cmd *cobra.Command) (conversationview.Mode, error) {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return conversationview.ModeJSON, err
	}
	if mode == outputTable {
		return conversationview.ModeTable, nil
	}
	return conversationview.ModeJSON, nil
}

// renderConversationResponse decodes a ctrl_conversation_* payload into the
// domain type the daemon marshalled it from (pkg/control/dispatch.go hands
// *insightstypes.Stats and friends straight to okResponse) and hands it to the same
// renderer `rafikid agent` uses. Both surfaces read the same rows through the
// same pkg/insights queries; routing both through conversationview.Render is what keeps
// them from presenting those rows differently.
func renderConversationResponse[T any](w io.Writer, m conversationview.Mode, resp *protocol.Response, table func(io.Writer, T) error) error {
	var v T
	if err := decodeConversationData(resp, &v); err != nil {
		return err
	}
	return conversationview.Render(w, v, m, table)
}

// decodeConversationData decodes a ctrl_conversation_* payload into v. Split
// out of renderConversationResponse for search, whose payload is an envelope
// around the value it renders rather than the value itself.
func decodeConversationData[T any](resp *protocol.Response, v *T) error {
	if err := json.Unmarshal(resp.Data, v); err != nil {
		return fmt.Errorf("decode %s response: %w", resp.Command, err)
	}
	return nil
}

// ─── stats ──────────────────────────────────────────────────────────────────

func newConversationsStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats [conv-id]",
		Short: "Global or per-conversation stats over persisted history",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runConversationsStats,
	}
	bindConversationFilterFlags(cmd)
	return cmd
}

func runConversationsStats(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req := protocol.ConversationStatsRequest{Type: protocol.TypeCtrlConversationStats}
	if len(args) == 1 {
		req.ConversationID = args[0]
	} else {
		f, err := conversationview.BindStatsFilter(conversationFilterVals(cmd))
		if err != nil {
			return err
		}
		req.SinceUnix = unixOrZero(f.Since)
		req.UntilUnix = unixOrZero(f.Until)
		req.Owner = f.Owner
		req.Persona = f.Persona
		req.Source = f.Source
		req.Model = f.Model
		req.Path = string(f.Path)
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_stats: %s", client.FormatError(resp))
	}
	mode, err := conversationsMode(cmd)
	if err != nil {
		return err
	}
	return renderConversationResponse(os.Stdout, mode, resp, conversationview.RenderStats)
}

// ─── search ─────────────────────────────────────────────────────────────────

func newConversationsSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "search",
		Aliases: []string{"ls", "list"},
		Short:   "Search persisted conversation history",
		Args:    cobra.NoArgs,
		RunE:    runConversationsSearch,
	}
	bindConversationFilterFlags(cmd)
	cmd.Flags().String("status", "", "filter by status")
	cmd.Flags().Int64("min-tokens", 0, "minimum total tokens")
	cmd.Flags().String("text", "", "full-text search over first messages")
	cmd.Flags().Int("limit", 0, "max results (0 = default)")
	return cmd
}

func runConversationsSearch(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	v := conversationFilterVals(cmd)
	v.Status, _ = cmd.Flags().GetString("status")
	v.MinTokens, _ = cmd.Flags().GetInt64("min-tokens")
	v.Text, _ = cmd.Flags().GetString("text")
	v.Limit, _ = cmd.Flags().GetInt("limit")

	f, err := conversationview.BindSearchFilter(v)
	if err != nil {
		return err
	}

	req := protocol.ConversationSearchRequest{
		Type:      protocol.TypeCtrlConversationSearch,
		SinceUnix: unixOrZero(f.Since),
		UntilUnix: unixOrZero(f.Until),
		Owner:     f.Owner,
		Persona:   f.Persona,
		Source:    f.Source,
		Model:     f.Model,
		Path:      string(f.Path),
		Status:    f.Status,
		MinTokens: f.MinTokens,
		Text:      f.Text,
		Limit:     f.Limit,
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_search: %s", client.FormatError(resp))
	}
	mode, err := conversationsMode(cmd)
	if err != nil {
		return err
	}
	return renderConversationSearch(os.Stdout, mode, resp)
}

// renderConversationSearch unwraps ctrl_conversation_search's payload before
// rendering. Alone among the three verbs it wraps its rows in a {"rows": [...]}
// envelope (control-protocol.md §6.18) where stats and export send the domain
// value bare, so it cannot go through renderConversationResponse. Unwrapping
// here keeps both the table and the JSON matching `rafikid agent search`, which
// prints the rows themselves.
func renderConversationSearch(w io.Writer, m conversationview.Mode, resp *protocol.Response) error {
	var payload struct {
		Rows []insightstypes.ConversationSummary `json:"rows"`
	}
	if err := decodeConversationData(resp, &payload); err != nil {
		return err
	}
	return conversationview.Render(w, payload.Rows, m, conversationview.RenderSearch)
}

// ─── export ─────────────────────────────────────────────────────────────────

func newConversationsExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <conv-id>",
		Short: "Export a persisted conversation's full transcript",
		Args:  cobra.ExactArgs(1),
		RunE:  runConversationsExport,
	}
}

func runConversationsExport(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req := protocol.ConversationExportRequest{
		Type:           protocol.TypeCtrlConversationExport,
		ConversationID: args[0],
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_export: %s", client.FormatError(resp))
	}
	mode, err := conversationsMode(cmd)
	if err != nil {
		return err
	}
	return renderConversationResponse(os.Stdout, mode, resp, conversationview.RenderTranscriptMD)
}

// ─── query ────────────────────────────────────────────────────────────────

// newConversationsQueryCmd returns `rafiki conversations query <name>`, the
// client half of the conversation-query catalogue. Unlike its stats/search
// siblings it goes over Connect rather than the framed protocol — the framed
// protocol is frozen and takes no new verbs.
func newConversationsQueryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query <name>",
		Short: "Run a named catalogue query (tools, skills, classes, models, sizes, coverage)",
		Args:  cobra.ExactArgs(1),
		RunE:  runConversationsQuery,
	}
	// The query names are static and shared with the daemon's registry (the
	// insightstypes list is held in step by TestCatalogueMatchesSharedQueryNames),
	// so completion never dials the daemon and can never block or print.
	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) >= 1 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var names []string
		for _, n := range insightstypes.QueryNames {
			if strings.HasPrefix(n, toComplete) {
				names = append(names, n)
			}
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
	bindConversationFilterFlags(cmd)
	return cmd
}

func runConversationsQuery(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()
	ctx := cmdCtx(cmd)

	v := conversationFilterVals(cmd)
	f, err := conversationview.BindStatsFilter(v)
	if err != nil {
		return err
	}
	resp, err := client.ConversationQuery(ctx, connect.NewRequest(&rafikiv1.ConversationQueryRequest{
		Name: args[0], SinceUnix: unixPtrOrNil(f.Since), UntilUnix: unixPtrOrNil(f.Until),
		Owner: f.Owner, Persona: f.Persona, Source: f.Source, Model: f.Model, Path: string(f.Path),
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	mode, err := conversationsMode(cmd)
	if err != nil {
		return err
	}
	return renderQueryResponse(os.Stdout, mode, resp.Msg)
}

// ─── review ────────────────────────────────────────────────────────────────

// newConversationsReviewCmd returns `rafiki conversations review <id|name>…`,
// the general entry for the review verb (design §5) — `rafiki close --review`
// is only the close-triggered convenience spelling of it. Like `query` it
// goes over Connect: the framed protocol is frozen and takes no new verbs.
func newConversationsReviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review <id|name>...",
		Short: "Ask the daemon to review one or more conversations (detect or rank)",
		Long: `Ask the daemon's detector over one or more closed conversations. Each
optional request field resolves per-value: an explicit flag, else
~/.config/rafiki/review.json (RAFIKI_REVIEW_CONFIG overrides the path), else
the daemon's own defaults — a missing file is a working configuration.

A request naming N conversations is N independent per-conversation calls; the
response carries one accept status per id, and out-of-scope ids are dropped
silently (never a permission error). Detect does not persist findings; rank
does.

Note: --analyzer-profile names the analyzer profile. The global --profile/-P
selects the DAEMON profile and keeps that meaning here; profile resolution
(profile_glue.go) reads a flag named "profile" straight off the command's own
flag set, so this verb's flag must not reuse the name.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runConversationsReview,
	}
	cmd.Flags().String("stage", "detect", `analysis stage: "detect" or "rank" (rank persists findings)`)
	_ = cmd.RegisterFlagCompletionFunc("stage", cobra.FixedCompletions(
		[]string{"detect", "rank"}, cobra.ShellCompDirectiveNoFileComp))
	cmd.Flags().String("model", "", "provider-qualified detector model")
	// --analyzer-profile, not --profile: the client's global -P/--profile
	// selects the daemon profile, and profile resolution (profile_glue.go)
	// reads a flag named "profile" straight off the command's flag set — a
	// local one under that name would make `--profile sql` resolve (and
	// usually exit(2) on) an unknown DAEMON profile instead of naming the
	// analyzer profile the request field wants.
	cmd.Flags().String("analyzer-profile", "", "analyzer profile name (RAFIKI_ANALYZER_DIR)")
	cmd.Flags().Float64("budget-usd", 0, "per-conversation spend ceiling in USD")
	cmd.Flags().Int32("min-turns", 0, "skip conversations under this many turns")
	cmd.Flags().Bool("force", false, "re-analyze conversations that already carry analysis")
	return cmd
}

// reviewStageFlag maps --stage onto the wire enum. The accepted set is the
// design's closed set — detect (the flag default) and rank — and anything
// else, including an explicitly empty value, errors before any dial.
func reviewStageFlag(cmd *cobra.Command) (rafikiv1.ReviewStage, error) {
	s, _ := cmd.Flags().GetString("stage")
	switch s {
	case "detect":
		return rafikiv1.ReviewStage_REVIEW_STAGE_DETECT, nil
	case "rank":
		return rafikiv1.ReviewStage_REVIEW_STAGE_RANK, nil
	default:
		return rafikiv1.ReviewStage_REVIEW_STAGE_UNSPECIFIED,
			fmt.Errorf("--stage must be \"detect\" or \"rank\", got %q", s)
	}
}

// applyReviewFlags copies every review flag the caller actually typed onto
// req. Changed(), not a zero-value check, is the test: it keeps a typed
// `--budget-usd 0` a real (explicit) request field instead of collapsing it
// to "unset, daemon decides", and it leaves untyped flags for the config
// file and the daemon's own defaults (design §3's resolution order).
func applyReviewFlags(cmd *cobra.Command, req *rafikiv1.ConversationReviewRequest) {
	fs := cmd.Flags()
	if fs.Changed("model") {
		v, _ := fs.GetString("model")
		req.Model = &v
	}
	if fs.Changed("analyzer-profile") {
		v, _ := fs.GetString("analyzer-profile")
		req.Profile = &v
	}
	if fs.Changed("budget-usd") {
		v, _ := fs.GetFloat64("budget-usd")
		req.BudgetUsd = &v
	}
	if fs.Changed("min-turns") {
		v, _ := fs.GetInt32("min-turns")
		req.MinTurns = &v
	}
	if fs.Changed("force") {
		v, _ := fs.GetBool("force")
		req.Force = &v
	}
}

// runConversationsReview resolves each target through the same framed-client
// Resolve every other client verb uses, then sends ONE batched review
// request over Connect. The batch is convenience only: the daemon treats it
// as N independent per-conversation calls (design §4), each accepted or
// rejected on its own.
func runConversationsReview(cmd *cobra.Command, args []string) error {
	// Resolve the stage before any dial: --stage garbage is a user-input
	// error and must not spend a round trip.
	stage, err := reviewStageFlag(cmd)
	if err != nil {
		return err
	}

	c := mustDial(cmd)
	defer c.Close()
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctx := cmdCtx(cmd)

	ids := make([]string, 0, len(args))
	for _, arg := range args {
		id, err := c.Resolve(ctx, arg)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", arg, err)
		}
		ids = append(ids, id)
	}

	req := &rafikiv1.ConversationReviewRequest{ConversationIds: ids, Stage: stage}
	applyReviewFlags(cmd, req)
	// The config file fills only what the flags left unset — mergeInto never
	// overwrites a set field, so flags win (design §3).
	cfg, err := loadReviewConfig()
	if err != nil {
		return err
	}
	cfg.mergeInto(req)

	resp, err := ep.control().ConversationReview(ctx, connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	return renderReviewAccepts(os.Stdout, resp.Msg.GetAccepted())
}

// ─── findings ───────────────────────────────────────────────────────────────

// newConversationsFindingsCmd returns `rafiki conversations findings`, the
// read-only findings list over Connect. No positional args: it lists across
// the caller's whole scope, matching `rafikid agent findings`'s no-args
// behavior. Triage (dismiss/action) deliberately stays rafikid-only (design
// §2) — acting on findings is manual by owner decision.
func newConversationsFindingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "findings",
		Short: "List review findings and recent analysis runs",
		Args:  cobra.NoArgs,
		RunE:  runConversationsFindings,
	}
	cmd.Flags().String("axis", "", "filter by axis")
	cmd.Flags().String("skill", "", "filter by skill name")
	cmd.Flags().String("status", "", `filter by status: "open" (the daemon's default), "dismissed" or "actioned"`)
	cmd.Flags().Int("limit", 0, "max results (0 = daemon default)")
	return cmd
}

func runConversationsFindings(cmd *cobra.Command, _ []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	axis, _ := cmd.Flags().GetString("axis")
	skill, _ := cmd.Flags().GetString("skill")
	status, _ := cmd.Flags().GetString("status")
	limit, _ := cmd.Flags().GetInt("limit")
	if limit < 0 || limit > math.MaxInt32 {
		return fmt.Errorf("--limit must be between 0 and %d", int32(math.MaxInt32))
	}

	resp, err := ep.control().ConversationFindings(cmdCtx(cmd), connect.NewRequest(&rafikiv1.ConversationFindingsRequest{
		Axis: axis, Skill: skill, Status: status, Limit: int32(limit),
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	return renderFindingsResponse(os.Stdout, mode, resp.Msg)
}

// renderFindingsResponse writes a ConversationFindingsResponse in the
// requested mode. Table mode renders the findings table with the same
// columns `rafikid agent findings` prints, followed by the recent-analyses
// table — the analysis rows are where an enqueued review's outcome becomes
// visible over the wire (design §2: rows are inserted at completion only,
// so an in-flight review reads as empty until then).
func renderFindingsResponse(w io.Writer, mode outputMode, resp *rafikiv1.ConversationFindingsResponse) error {
	switch mode {
	case outputJSON:
		b, err := (protojson.MarshalOptions{Multiline: true, Indent: "  "}).Marshal(resp)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	case outputJSONL:
		rows := make([]any, 0, len(resp.GetFindings()))
		for _, f := range resp.GetFindings() {
			rows = append(rows, f)
		}
		return writeJSONL(w, rows)
	default:
		return renderFindingsTable(w, resp)
	}
}

// renderFindingsTable renders the table arms of the response.
func renderFindingsTable(w io.Writer, resp *rafikiv1.ConversationFindingsResponse) error {
	findings, analyses := resp.GetFindings(), resp.GetAnalyses()
	if len(findings) == 0 && len(analyses) == 0 {
		_, err := fmt.Fprintln(w, "no findings")
		return err
	}
	if len(findings) > 0 {
		tb := table.New(w, table.Options{})
		tb.Header("Axis", "Skill", "Title", "Savings", "Status", "ID")
		for _, r := range findings {
			tb.Row(r.GetAxis(), r.GetSkillName(), r.GetTitle(),
				humanize.Comma(r.GetExpectedSavingsTokens()), r.GetStatus(), r.GetId())
		}
		if err := tb.Render(); err != nil {
			return err
		}
	}
	if len(analyses) > 0 {
		tb := table.New(w, table.Options{})
		tb.Header("Analysis", "Conversation", "Model", "Status", "Cost", "Created")
		for _, a := range analyses {
			tb.Row(a.GetId(), a.GetConversationId(), a.GetModel(), a.GetStatus(),
				fmt.Sprintf("$%.4f", a.GetCostUsd()),
				time.Unix(a.GetCreatedAtUnix(), 0).Local().Format("2006-01-02 15:04"))
		}
		if err := tb.Render(); err != nil {
			return err
		}
	}
	return nil
}

// renderQueryResponse prints a ConversationQueryResponse as a table or as JSON
// with real typed values (ints and floats decode as numbers, never strings —
// design doc §3). A bare marker interface (insights.Entry, mirrored on the
// wire by QueryValue's oneof) does not marshal to clean JSON on its own, so
// there are two explicit type switches, one per output mode — the JSON cell
// switch above and the pkg/table formatting switch below — kept adjacent so
// the two can't drift apart silently.
func renderQueryResponse(w io.Writer, m conversationview.Mode, resp *rafikiv1.ConversationQueryResponse) error {
	headers := make([]string, len(resp.GetColumns()))
	for i, c := range resp.GetColumns() {
		headers[i] = c.GetName()
	}

	if m != conversationview.ModeTable {
		type row = []any
		out := struct {
			Columns []string `json:"columns"`
			Rows    []row    `json:"rows"`
		}{Columns: headers}
		for _, r := range resp.GetRows() {
			jr := make(row, 0, len(r.GetCells()))
			for _, cell := range r.GetCells() {
				switch v := cell.GetV().(type) {
				case *rafikiv1.QueryValue_IntValue:
					jr = append(jr, v.IntValue)
				case *rafikiv1.QueryValue_FloatValue:
					jr = append(jr, v.FloatValue)
				default:
					jr = append(jr, cell.GetStrValue())
				}
			}
			out.Rows = append(out.Rows, jr)
		}
		enc := json.NewEncoder(w)
		if m == conversationview.ModeJSON {
			enc.SetIndent("", "  ")
		}
		return enc.Encode(out)
	}

	tb := table.New(w, table.Options{})
	tb.Header(headers...)
	for _, r := range resp.GetRows() {
		cells := make([]string, len(r.GetCells()))
		for i, cell := range r.GetCells() {
			col := resp.GetColumns()[i]
			switch v := cell.GetV().(type) {
			case *rafikiv1.QueryValue_IntValue:
				cells[i] = fmt.Sprintf("%d", v.IntValue)
			case *rafikiv1.QueryValue_FloatValue:
				switch col.GetFormat() {
				case "usd":
					cells[i] = fmt.Sprintf("$%.4f", v.FloatValue)
				case "pct":
					cells[i] = fmt.Sprintf("%.0f%%", v.FloatValue*100)
				default:
					cells[i] = fmt.Sprintf("%.2f", v.FloatValue)
				}
			default:
				cells[i] = cell.GetStrValue()
			}
		}
		tb.Row(cells...)
	}
	return tb.Render()
}
