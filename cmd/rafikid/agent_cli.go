// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/agentcli/local"
	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/providers"
	"go.graveland.dev/rafiki/pkg/store"
)

// dbDSNDefault returns the default DSN for agent subcommands, checking
// RAFIKI_DB first then RAFIKI_TEST_DSN.
func dbDSNDefault() string {
	def := os.Getenv("RAFIKI_DB")
	if def == "" {
		def = os.Getenv("RAFIKI_TEST_DSN")
	}
	return def
}

// connectPool opens a pool against db, erroring with the flag/env hint a
// caller needs when neither was set.
func connectPool(ctx context.Context, db string) (*pgxpool.Pool, error) {
	if db == "" {
		return nil, errors.New("--db (or RAFIKI_DB, or RAFIKI_TEST_DSN) is required")
	}
	return pgxpool.New(ctx, db)
}

// jsonMode maps the -j/-J flag pair onto the render mode shared with
// `rafiki conversations`; neither flag means the human-first tables.
func jsonMode(indent, compact bool) agentcli.Mode {
	switch {
	case indent:
		return agentcli.ModeJSON
	case compact:
		return agentcli.ModeJSONCompact
	default:
		return agentcli.ModeTable
	}
}

// jsonModeFromCmd reads --json/-j and --json-compact/-J from cmd.
func jsonModeFromCmd(cmd *cobra.Command) agentcli.Mode {
	indent, _ := cmd.Flags().GetBool("json")
	compact, _ := cmd.Flags().GetBool("json-compact")
	return jsonMode(indent, compact)
}

// dbFromCmd reads --db from cmd (persistent flag).
func dbFromCmd(cmd *cobra.Command) string {
	db, _ := cmd.Flags().GetString("db")
	return db
}

// registerFilterFlags registers the filter flags shared by stats/search onto fs.
func registerFilterFlags(fs *pflag.FlagSet) {
	fs.String("since", "", "RFC3339 timestamp or duration like 24h")
	fs.String("until", "", "RFC3339 timestamp or duration like 24h")
	fs.String("owner", "", "filter by owner username")
	fs.String("persona", "", "filter by persona")
	fs.String("source", "", "filter by source")
	fs.String("model", "", "filter by model")
	fs.String("path", "", "filter by path (proxy or direct)")
}

// filterValsFromCmd reads filter flag values from cmd into an agentcli.FilterVals.
func filterValsFromCmd(cmd *cobra.Command) *agentcli.FilterVals {
	v := &agentcli.FilterVals{}
	v.Since, _ = cmd.Flags().GetString("since")
	v.Until, _ = cmd.Flags().GetString("until")
	v.Owner, _ = cmd.Flags().GetString("owner")
	v.Persona, _ = cmd.Flags().GetString("persona")
	v.Source, _ = cmd.Flags().GetString("source")
	v.Model, _ = cmd.Flags().GetString("model")
	v.Path, _ = cmd.Flags().GetString("path")
	return v
}

// newAgentStatsCmd returns `rafikid agent stats [conv-id]`.
func newAgentStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats [conv-id]",
		Short: "Show aggregate conversation stats, or a single conversation's stats",
		RunE: func(cmd *cobra.Command, args []string) error {
			db := dbFromCmd(cmd)
			jm := jsonModeFromCmd(cmd)

			ctx := context.Background()
			pool, err := connectPool(ctx, db)
			if err != nil {
				return err
			}
			defer pool.Close()
			// No Pricer here: see the original comment in agent_cli.go.
			b := local.New(local.Options{Pool: pool})

			if len(args) > 0 {
				st, err := b.ConversationStats(ctx, args[0])
				if err != nil {
					return err
				}
				return agentcli.Render(os.Stdout, st, jm, agentcli.RenderStats)
			}

			v := filterValsFromCmd(cmd)
			f, err := agentcli.BindStatsFilter(*v)
			if err != nil {
				return err
			}
			st, err := b.Stats(ctx, f)
			if err != nil {
				return err
			}
			return agentcli.Render(os.Stdout, st, jm, agentcli.RenderStats)
		},
	}
	registerFilterFlags(cmd.Flags())
	return cmd
}

// newAgentSearchCmd returns `rafikid agent search`.
func newAgentSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Full-text search over conversations with filters",
		RunE: func(cmd *cobra.Command, args []string) error {
			db := dbFromCmd(cmd)
			jm := jsonModeFromCmd(cmd)
			v := filterValsFromCmd(cmd)
			v.Status, _ = cmd.Flags().GetString("status")
			v.MinTokens, _ = cmd.Flags().GetInt64("min-tokens")
			v.Text, _ = cmd.Flags().GetString("text")
			v.Limit, _ = cmd.Flags().GetInt("limit")

			ctx := context.Background()
			pool, err := connectPool(ctx, db)
			if err != nil {
				return err
			}
			defer pool.Close()
			b := local.New(local.Options{Pool: pool})

			f, err := agentcli.BindSearchFilter(*v)
			if err != nil {
				return err
			}
			rows, err := b.Search(ctx, f)
			if err != nil {
				return err
			}
			return agentcli.Render(os.Stdout, rows, jm, agentcli.RenderSearch)
		},
	}
	registerFilterFlags(cmd.Flags())
	cmd.Flags().String("status", "", "filter by status")
	cmd.Flags().Int64("min-tokens", 0, "minimum total tokens")
	cmd.Flags().String("text", "", "full-text search over first messages")
	cmd.Flags().Int("limit", 0, "max results (0 = default)")
	return cmd
}

// newAgentExportCmd returns `rafikid agent export <conv-id>`.
func newAgentExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <conv-id>",
		Short: "Export a single conversation transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db := dbFromCmd(cmd)
			jm := jsonModeFromCmd(cmd)

			ctx := context.Background()
			pool, err := connectPool(ctx, db)
			if err != nil {
				return err
			}
			defer pool.Close()
			b := local.New(local.Options{Pool: pool})

			tr, err := b.Export(ctx, args[0])
			if err != nil {
				return err
			}
			return agentcli.Render(os.Stdout, tr, jm, agentcli.RenderTranscriptMD)
		},
	}
}

// newAgentAnalyzeCmd returns `rafikid agent analyze <conv-id>... | --corpus DIR`.
func newAgentAnalyzeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "analyze <conv-id>...",
		Short: "LLM-driven skill-gap detector over stored conversations or a --corpus dir",
		Long: `Runs the analyze pipeline (compact → detect → rank → draft) over stored
conversation ids or exported transcripts in --corpus.

Use --compact, --detect, or --rank to stop early; --draft (the default)
runs the full pipeline.

With --compare, sweep the detector over --corpus once per model and
compare findings.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := parseAnalyzeArgsFromCmd(cmd, args)
			if err != nil {
				return err
			}
			return runAnalyze(a)
		},
	}
	registerAnalyzeFlags(cmd.Flags())
	return cmd
}

// parseAnalyzeArgsFromCmd reads and validates `agent analyze` flags from a cobra
// command, returning the validated analyzeArgs. The positional conv-ids come
// from args.
func parseAnalyzeArgsFromCmd(cmd *cobra.Command, args []string) (analyzeArgs, error) {
	corpus, _ := cmd.Flags().GetString("corpus")
	compare, _ := cmd.Flags().GetString("compare")
	compactStage, _ := cmd.Flags().GetBool("compact")
	detectStage, _ := cmd.Flags().GetBool("detect")
	rankStage, _ := cmd.Flags().GetBool("rank")
	draftStage, _ := cmd.Flags().GetBool("draft")
	profile, _ := cmd.Flags().GetString("profile")
	model, _ := cmd.Flags().GetString("model")
	analyzerDir, _ := cmd.Flags().GetString("analyzer-dir")
	force, _ := cmd.Flags().GetBool("force")
	limit, _ := cmd.Flags().GetInt("limit")
	out, _ := cmd.Flags().GetString("out")
	repo, _ := cmd.Flags().GetString("repo")
	noStore, _ := cmd.Flags().GetBool("no-store")
	proxyURL, _ := cmd.Flags().GetString("proxy-url")
	proxyTok, _ := cmd.Flags().GetString("proxy-token")
	indent, _ := cmd.Flags().GetBool("json")
	compactJSON, _ := cmd.Flags().GetBool("json-compact")

	stages := map[string]bool{"compact": compactStage, "detect": detectStage, "rank": rankStage, "draft": draftStage}
	var stopAfter string
	for name, set := range stages {
		if !set {
			continue
		}
		if stopAfter != "" {
			return analyzeArgs{}, errors.New("agent analyze: --compact, --detect, --rank and --draft are mutually exclusive")
		}
		stopAfter = name
	}
	if stopAfter == "draft" {
		stopAfter = ""
	}

	ids := args
	if corpus != "" && len(ids) > 0 {
		return analyzeArgs{}, errors.New("agent analyze: --corpus and conversation ids are mutually exclusive")
	}
	if corpus == "" && len(ids) == 0 {
		return analyzeArgs{}, errors.New("agent analyze: requires conversation ids or --corpus <dir>")
	}

	var compareModels []string
	if compare != "" {
		if corpus == "" {
			return analyzeArgs{}, errors.New("agent analyze: --compare requires --corpus (re-analyzing a stored population per model would thrash the skip key)")
		}
		for _, m := range strings.Split(compare, ",") {
			if m = strings.TrimSpace(m); m != "" {
				compareModels = append(compareModels, m)
			}
		}
	}

	return analyzeArgs{
		ConversationIDs: ids,
		CorpusDir:       corpus,
		StopAfter:       stopAfter,
		Compare:         compareModels,
		DB:              dbFromCmd(cmd),
		Profile:         profile,
		Model:           model,
		AnalyzerDir:     analyzerDir,
		Force:           force,
		Limit:           limit,
		Out:             out,
		Repo:            repo,
		NoStore:         noStore,
		Indent:          indent,
		Compact:         compactJSON,
		ProxyURL:        proxyURL,
		ProxyTok:        proxyTok,
	}, nil
}

// newAgentFindingsCmd returns `rafikid agent findings [--axis ...] [--skill ...] [--status ...]`.
func newAgentFindingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "findings",
		Short: "List and triage analysis findings",
		Long: `List analysis findings with optional filters, or update finding status.

Subcommands:
  dismiss <id>    Mark a finding as dismissed.
  action  <id>    Mark a finding as actioned.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("agent findings: unknown command %q (want dismiss or action)", args[0])
			}
			return runFindingsList(cmd)
		},
	}

	cmd.Flags().String("axis", "", "filter by axis")
	cmd.Flags().String("skill", "", "filter by skill name")
	cmd.Flags().String("status", "", "filter by status (default: open)")

	cmd.AddCommand(newAgentFindingsDismissCmd())
	cmd.AddCommand(newAgentFindingsActionCmd())

	return cmd
}

// runFindingsList runs the read-only findings list path.
func runFindingsList(cmd *cobra.Command) error {
	db := dbFromCmd(cmd)
	jm := jsonModeFromCmd(cmd)
	axis, _ := cmd.Flags().GetString("axis")
	skill, _ := cmd.Flags().GetString("skill")
	status, _ := cmd.Flags().GetString("status")

	ctx := context.Background()
	pool, err := connectPool(ctx, db)
	if err != nil {
		return err
	}
	defer pool.Close()
	b := local.New(local.Options{Pool: pool})

	rows, err := b.Findings(ctx, store.FindingFilter{Axis: axis, Skill: skill, Status: status})
	if err != nil {
		return err
	}
	return agentcli.Render(os.Stdout, rows, jm, agentcli.RenderFindings)
}

// newAgentFindingsDismissCmd returns `rafikid agent findings dismiss <id>`.
func newAgentFindingsDismissCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dismiss <id>",
		Short: "Mark a finding as dismissed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFindingsSetStatus(cmd, args[0], "dismissed")
		},
	}
}

// newAgentFindingsActionCmd returns `rafikid agent findings action <id>`.
func newAgentFindingsActionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "action <id>",
		Short: "Mark a finding as actioned",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFindingsSetStatus(cmd, args[0], "actioned")
		},
	}
}

// runFindingsSetStatus performs the dismiss/action mutation.
func runFindingsSetStatus(cmd *cobra.Command, id, status string) error {
	db := dbFromCmd(cmd)
	jm := jsonModeFromCmd(cmd)

	ctx := context.Background()
	pool, err := connectPool(ctx, db)
	if err != nil {
		return err
	}
	defer pool.Close()
	b := local.New(local.Options{Pool: pool})

	row, err := b.SetFindingStatus(ctx, id, status)
	if err != nil {
		return err
	}
	return agentcli.Render(os.Stdout, row, jm,
		func(w io.Writer, r store.FindingRow) error { return agentcli.RenderFindings(w, []store.FindingRow{r}) })
}

// analyzeArgs is the parsed, validated result of `rafikid agent analyze`'s
// arguments: exactly one population selector (ConversationIDs xor
// CorpusDir), and at most one stage-stop flag.
type analyzeArgs struct {
	ConversationIDs []string
	CorpusDir       string
	StopAfter       string // "" (full pipeline) | compact | detect | rank

	Compare []string // model ids to sweep, valid only with CorpusDir set

	DB          string
	Profile     string
	Model       string
	AnalyzerDir string

	Force    bool
	Limit    int
	Out      string
	Repo     string
	NoStore  bool
	Indent   bool
	Compact  bool
	ProxyURL string
	ProxyTok string
}

// registerAnalyzeFlags registers `rafikid agent analyze`'s flags on fs.
func registerAnalyzeFlags(fs *pflag.FlagSet) {
	fs.String("corpus", "", "run over exported *.json transcripts in this directory instead of a DSN population")
	fs.String("compare", "", "comma-separated model ids to sweep over --corpus, comparing detector output per model (requires --corpus)")
	fs.Bool("compact", false, "stop after the compact stage")
	fs.Bool("detect", false, "stop after the detect stage")
	fs.Bool("rank", false, "stop after the rank stage")
	fs.Bool("draft", false, "run the full pipeline through draft (default)")
	fs.String("profile", "", "named profile from --analyzer-dir")
	fs.String("model", "", "override the detector/rank/draft model")
	fs.String("analyzer-dir", os.Getenv("RAFIKI_ANALYZER_DIR"), "analyzer directory (profiles.yaml + detector.md/draft.md); default RAFIKI_ANALYZER_DIR")
	fs.Bool("force", false, "re-analyze even if already analyzed under this exact configuration")
	fs.Int("limit", 0, "cap how many conversations this run analyzes (0 = profile default)")
	fs.String("out", "", "write per-conversation JSON+markdown artifacts to this directory")
	fs.String("repo", "", "repo root to resolve current skill files from, for --draft edits")
	fs.Bool("no-store", false, "don't persist analysis rows (still ranks/drafts in-memory)")
	fs.String("proxy-url", os.Getenv("RAFIKI_URL"), "route LLM calls through this rafiki proxy URL instead of ANTHROPIC_API_KEY")
	fs.String("proxy-token", os.Getenv("RAFIKI_TOKEN"), "bearer token for --proxy-url")
}

// parseAnalyzeArgs parses `rafikid agent analyze`'s flags and positional
// conversation ids, validating the mutual exclusions the brief requires:
// exactly one stage-stop flag among --compact/--detect/--rank/--draft, and
// exactly one population selector among conversation ids and --corpus.
func parseAnalyzeArgs(args []string) (analyzeArgs, error) {
	fs := pflag.NewFlagSet("agent analyze", pflag.ContinueOnError)
	db := fs.String("db", dbDSNDefault(), "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")
	corpus := fs.String("corpus", "", "run over exported *.json transcripts in this directory instead of a DSN population")
	compare := fs.String("compare", "", "comma-separated model ids to sweep over --corpus, comparing detector output per model (requires --corpus)")
	compactStage := fs.Bool("compact", false, "stop after the compact stage")
	detectStage := fs.Bool("detect", false, "stop after the detect stage")
	rankStage := fs.Bool("rank", false, "stop after the rank stage")
	draftStage := fs.Bool("draft", false, "run the full pipeline through draft (default)")
	profile := fs.String("profile", "", "named profile from --analyzer-dir")
	model := fs.String("model", "", "override the detector/rank/draft model")
	analyzerDir := fs.String("analyzer-dir", os.Getenv("RAFIKI_ANALYZER_DIR"), "analyzer directory (profiles.yaml + detector.md/draft.md); default RAFIKI_ANALYZER_DIR")
	force := fs.Bool("force", false, "re-analyze even if already analyzed under this exact configuration")
	limit := fs.Int("limit", 0, "cap how many conversations this run analyzes (0 = profile default)")
	out := fs.String("out", "", "write per-conversation JSON+markdown artifacts to this directory")
	repo := fs.String("repo", "", "repo root to resolve current skill files from, for --draft edits")
	noStore := fs.Bool("no-store", false, "don't persist analysis rows (still ranks/drafts in-memory)")
	proxyURL := fs.String("proxy-url", os.Getenv("RAFIKI_URL"), "route LLM calls through this rafiki proxy URL instead of ANTHROPIC_API_KEY")
	proxyTok := fs.String("proxy-token", os.Getenv("RAFIKI_TOKEN"), "bearer token for --proxy-url")
	indent := fs.BoolP("json", "j", false, "JSON output, indented")
	compactJSON := fs.BoolP("json-compact", "J", false, "JSON output, compact")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return analyzeArgs{}, err
	}

	stages := map[string]bool{"compact": *compactStage, "detect": *detectStage, "rank": *rankStage, "draft": *draftStage}
	var stopAfter string
	for name, set := range stages {
		if !set {
			continue
		}
		if stopAfter != "" {
			return analyzeArgs{}, errors.New("agent analyze: --compact, --detect, --rank and --draft are mutually exclusive")
		}
		stopAfter = name
	}
	if stopAfter == "draft" {
		stopAfter = "" // draft is the full pipeline; no distinct StopAfter value for it
	}

	ids := fs.Args()
	if *corpus != "" && len(ids) > 0 {
		return analyzeArgs{}, errors.New("agent analyze: --corpus and conversation ids are mutually exclusive")
	}
	if *corpus == "" && len(ids) == 0 {
		return analyzeArgs{}, errors.New("agent analyze: requires conversation ids or --corpus <dir>")
	}

	var compareModels []string
	if *compare != "" {
		if *corpus == "" {
			return analyzeArgs{}, errors.New("agent analyze: --compare requires --corpus (re-analyzing a stored population per model would thrash the skip key)")
		}
		for _, m := range strings.Split(*compare, ",") {
			if m = strings.TrimSpace(m); m != "" {
				compareModels = append(compareModels, m)
			}
		}
	}

	return analyzeArgs{
		ConversationIDs: ids,
		CorpusDir:       *corpus,
		StopAfter:       stopAfter,
		Compare:         compareModels,
		DB:              *db,
		Profile:         *profile,
		Model:           *model,
		AnalyzerDir:     *analyzerDir,
		Force:           *force,
		Limit:           *limit,
		Out:             *out,
		Repo:            *repo,
		NoStore:         *noStore,
		Indent:          *indent,
		Compact:         *compactJSON,
		ProxyURL:        *proxyURL,
		ProxyTok:        *proxyTok,
	}, nil
}

// errNoDetectorModel is returned by resolveProfile when neither --model nor
// --profile/--analyzer-dir resolved a model to run the detector with.
var errNoDetectorModel = errors.New("no detector model: pass --model, --profile, or --analyzer-dir")

// resolveProfile resolves --analyzer-dir/--profile/--model into a real
// *analyze.Profile, applying the brief's precedence: --model overrides all
// three stage models regardless of what --analyzer-dir/--profile resolved;
// --profile selects a named profile from --analyzer-dir (unknown name is an
// error listing what's available); with neither, the profile named "default"
// if the dir has one; with no analyzer dir and no --model, an actionable
// error. The analyzer dir's detector.md/draft.md base prompts are attached
// onto the resolved profile here — LoadAnalyzerDir deliberately leaves that
// to its caller.
// profileNames returns cfg's profile names, sorted, for an
// unknown/missing-profile error to enumerate.
func profileNames(cfg *analyze.AnalyzerConfig) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func resolveProfile(analyzerDir, profileName, model string, needModel bool) (*analyze.Profile, error) {
	var (
		p   analyze.Profile
		cfg *analyze.AnalyzerConfig
	)

	if analyzerDir != "" {
		var err error
		cfg, err = analyze.LoadAnalyzerDir(analyzerDir)
		if err != nil {
			return nil, err
		}
		switch {
		case profileName != "":
			found, ok := cfg.Profiles[profileName]
			if !ok {
				return nil, fmt.Errorf("agent analyze: unknown profile %q (available: %s)", profileName, strings.Join(profileNames(cfg), ", "))
			}
			p = found
		default:
			// No --profile given: fall back to a "default" profile if the
			// analyzer dir has one. If it doesn't, error rather than silently
			// running with a zero-value profile — a caller who pointed
			// --analyzer-dir at a real config almost certainly expects one of
			// its profiles to apply (models, filters, compact policy, ...),
			// even when --model also happens to be set: --model only covers
			// the three model fields, so silently dropping the rest of the
			// profile is a worse surprise than an actionable error naming
			// what's available. (Detector/draft PROMPT BASES are unaffected
			// either way — they come from cfg.DetectorBase/DraftBase below,
			// independent of which profile, if any, was selected.)
			found, ok := cfg.Profiles["default"]
			if !ok {
				return nil, fmt.Errorf("agent analyze: --analyzer-dir %q has no %q profile and no --profile was given (available: %s)", analyzerDir, "default", strings.Join(profileNames(cfg), ", "))
			}
			p = found
		}
		p.DetectorPromptBase = cfg.DetectorBase
		p.DraftPromptBase = cfg.DraftBase
	} else if profileName != "" {
		return nil, fmt.Errorf("agent analyze: --profile %q given without --analyzer-dir", profileName)
	}

	if model != "" {
		p.DetectorModel = model
		p.RankModel = model
		p.DraftModel = model
	}
	// The compact stage makes no LLM call, so it does not need a model —
	// only the CompactPolicy the profile (or Defaults) supplies.
	if needModel && p.DetectorModel == "" {
		return nil, errNoDetectorModel
	}
	p.Defaults()
	return &p, nil
}

// isSlashOrTildeModelID reports whether model uses OpenRouter-native id
// syntax (a "provider/model" slash id, or a "~"-prefixed catalog alias) —
// ids that direct-to-Anthropic can never serve, only a rafiki proxy can.
func isSlashOrTildeModelID(model string) bool {
	return strings.Contains(model, "/") || strings.HasPrefix(model, "~")
}

// checkModelServable fails fast when proxied is false (a bare
// ANTHROPIC_API_KEY, no rafiki proxy) but the profile's DetectorModel,
// RankModel, or DraftModel is a slash/tilde OpenRouter-native id: direct
// Anthropic cannot serve those ids at all, so every per-conversation
// Detect/Draft call would otherwise fail identically. Caught here, before any
// per-conversation work, rather than as a wall of identical failures.
func checkModelServable(p *analyze.Profile, proxied bool) error {
	if proxied {
		return nil
	}
	for _, f := range []struct{ name, model string }{
		{"detector_model", p.DetectorModel},
		{"rank_model", p.RankModel},
		{"draft_model", p.DraftModel},
	} {
		if isSlashOrTildeModelID(f.model) {
			return fmt.Errorf("agent analyze: %s %q needs OpenRouter routing, which direct-to-Anthropic can't provide; configure --proxy-url/--proxy-token (or RAFIKI_URL/RAFIKI_TOKEN)", f.name, f.model)
		}
	}
	return nil
}

// checkModelsServable is checkModelServable's --compare counterpart: it
// preflights every model in the sweep against proxied, rather than a single
// profile's DetectorModel, so an unservable model fails before any
// per-conversation work rather than mid-sweep.
func checkModelsServable(models []string, proxied bool) error {
	if proxied {
		return nil
	}
	for _, m := range models {
		if isSlashOrTildeModelID(m) {
			return fmt.Errorf("agent analyze --compare: model %q needs OpenRouter routing, which direct-to-Anthropic can't provide; configure --proxy-url/--proxy-token (or RAFIKI_URL/RAFIKI_TOKEN)", m)
		}
	}
	return nil
}

// compareRunJSON is --compare's -j/-J view of an agentcli.CompareRun.
// CompareRun.Err is an error, which encoding/json marshals uselessly (an
// empty "{}", or silently vanishes under omitempty) — this substitutes a
// plain string so a failed model in the sweep is still visible in JSON
// output. Analyses (the raw per-conversation analyze.Analysis values) are
// left out of the JSON view same as RenderCompare's table leaves them out of
// the human view; --out already captures them as artifacts when set.
type compareRunJSON struct {
	Model    string            `json:"model"`
	Summary  *agentcli.Summary `json:"summary,omitempty"`
	Err      string            `json:"error,omitempty"`
	Analyzed int               `json:"analyzed"`
	Failed   int               `json:"failed"`
}

// compareRunsJSON converts a --compare sweep's runs to their JSON view.
func compareRunsJSON(runs []agentcli.CompareRun) []compareRunJSON {
	out := make([]compareRunJSON, len(runs))
	for i, r := range runs {
		out[i] = compareRunJSON{Model: r.Model, Summary: r.Summary, Analyzed: r.Analyzed, Failed: r.Failed}
		if r.Err != nil {
			out[i].Err = r.Err.Error()
		}
	}
	return out
}

// cliDefaultModel is the model an analyze run falls back to when neither the
// request nor the profile names one (Profile.Defaults() deliberately leaves
// DetectorModel/DraftModel unset). llm.NewClient invents no default of its
// own, so every caller that wants one supplies it — for this CLI that keeps
// the historical behaviour of defaulting to haiku.
const cliDefaultModel = "haiku-latest"

// resolveUpstream builds the llm.Client Analyze runs against, per the
// brief's upstream rules: a rafiki proxy (--proxy-url/-token or
// RAFIKI_URL/RAFIKI_TOKEN) wins when set, registered as BOTH
// anthropic and openrouter (the proxy does the OpenRouter routing itself);
// else ANTHROPIC_API_KEY direct to Anthropic, registered as Anthropic only.
// Returns whether the resolved upstream is proxied, for checkModelServable's
// slash/tilde preflight.
func resolveUpstream(proxyURL, proxyToken string) (*llm.Client, bool, error) {
	if proxyURL != "" {
		// anthropic.NewClient prepends option.DefaultClientOptions(), which — if
		// ANTHROPIC_API_KEY is set in the environment, which it commonly is for
		// direct-to-Anthropic use — turns that into an implicit
		// option.WithAPIKey(...) request option setting the X-Api-Key header.
		// That is a DIFFERENT header from the Authorization: Bearer one
		// WithAuthToken sets below, so without the WithAPIKey("") override here,
		// every --proxy-url request would carry the developer's real Anthropic
		// key to whatever host --proxy-url points at (including a third party)
		// alongside the intended proxy bearer token — a credential leak. Do NOT
		// remove this as dead-looking "clear an empty value" cleanup: it is the
		// fix. Request options apply in the order given and
		// http.Header.Set is last-writer-wins, so appending this after
		// WithAuthToken guarantees it wins regardless of DefaultClientOptions'
		// position in anthropic.NewClient's own option list.
		sdk := anthropic.NewClient(option.WithBaseURL(proxyURL), option.WithAuthToken(proxyToken), option.WithAPIKey(""))
		sender := llm.FromSDK(sdk)
		client, err := llm.NewClient(
			llm.WithProviders(providers.Default()),
			llm.WithProviderSender("anthropic", sender),
			llm.WithProviderSender("openrouter", sender),
			llm.WithDefaultModel(cliDefaultModel),
		)
		return client, true, err
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, false, errors.New("agent analyze: no --proxy-url/RAFIKI_URL configured and ANTHROPIC_API_KEY unset")
	}
	client, err := llm.NewClient(
		llm.WithProviders(providers.Default()),
		llm.WithProviderSender("anthropic", llm.FromSDK(anthropic.NewClient(option.WithAPIKey(key)))),
		llm.WithDefaultModel(cliDefaultModel),
	)
	return client, false, err
}

// loadSkillFiles walks repo's two conventional skill layouts
// (.claude/skills/*/SKILL.md and skills/*/SKILL.md) into the
// []analyze.SkillFile candidate list agentcli.AnalyzeRequest.SkillFiles
// expects — the local backend's currentSkillFiles matches drafts against it
// by path substring/exact match, not by a per-name lookup. An empty repo
// returns (nil, nil): --repo is optional.
func loadSkillFiles(repo string) ([]analyze.SkillFile, error) {
	if repo == "" {
		return nil, nil
	}
	var out []analyze.SkillFile
	for _, sub := range []string{".claude/skills", "skills"} {
		root := filepath.Join(repo, sub)
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("agent analyze --repo: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			skillPath := filepath.Join(root, e.Name(), "SKILL.md")
			content, err := os.ReadFile(skillPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("agent analyze --repo: %w", err)
			}
			rel, err := filepath.Rel(repo, skillPath)
			if err != nil {
				rel = skillPath
			}
			out = append(out, analyze.SkillFile{Path: filepath.ToSlash(rel), Content: string(content)})
		}
	}
	return out, nil
}

// agentAnalyzeCmd runs `rafikid agent analyze <conv-id>... | --corpus DIR`.
// Deprecated: the cobra path (newAgentAnalyzeCmd) is preferred. This wrapper
// exists for test backward compatibility.
func agentAnalyzeCmd(args []string) error {
	a, err := parseAnalyzeArgs(args)
	if err != nil {
		return err
	}
	return runAnalyze(a)
}

// runAnalyze contains the body of agentAnalyzeCmd, extracted so both the
// cobra path (newAgentAnalyzeCmd) and the raw-args path (agentAnalyzeCmd,
// for backward compat) can exercise it.
func runAnalyze(a analyzeArgs) error {
	// In --compare mode the detector model comes from the sweep list, not
	// a single resolved profile field, so resolveProfile must not demand one.
	profile, err := resolveProfile(a.AnalyzerDir, a.Profile, a.Model, a.StopAfter != "compact" && len(a.Compare) == 0)
	if err != nil {
		return err
	}

	// The compact stage runs entirely locally (Compact is a pure function),
	// so it needs neither an upstream nor credentials — the cheapest dev loop
	// is inspecting compaction output with nothing configured at all.
	var llmClient *llm.Client
	if a.StopAfter != "compact" {
		var proxied bool
		llmClient, proxied, err = resolveUpstream(a.ProxyURL, a.ProxyTok)
		if err != nil {
			return err
		}
		if len(a.Compare) > 0 {
			if err := checkModelsServable(a.Compare, proxied); err != nil {
				return err
			}
			// agentcli.Compare clones the base profile and overrides only
			// DetectorModel per sweep model — RankModel/DraftModel still come
			// from the base profile and are never checked by
			// checkModelsServable (which only preflights the sweep list), so
			// they need their own preflight here.
			if err := checkModelServable(profile, proxied); err != nil {
				return err
			}
			// a.StopAfter is "" (full pipeline, draft included), "detect", or
			// "rank" here ("compact" is excluded by the enclosing if, and
			// parseAnalyzeArgs already normalizes an explicit --draft back to
			// ""). A run that will draft needs a draft model: analyze.Draft
			// silently accepts an empty DraftModel and falls through to
			// llm.Client's own default model, drafting every skill edit
			// against a model the caller never actually chose.
			if a.StopAfter == "" && profile.DraftModel == "" {
				return errors.New("agent analyze --compare: a run through the draft stage needs a draft model; set draft_model in the profile or pass --model")
			}
		} else if err := checkModelServable(profile, proxied); err != nil {
			return err
		}
	}

	skillFiles, err := loadSkillFiles(a.Repo)
	if err != nil {
		return err
	}

	ctx := context.Background()
	var pool *pgxpool.Pool
	var closePool bool
	// Corpus-only runs need no database pool
	if a.CorpusDir != "" && a.DB == "" {
		closePool = false
	} else {
		var err error
		pool, err = connectPool(ctx, a.DB)
		if err != nil {
			return err
		}
		closePool = true
	}
	if closePool {
		defer pool.Close()
	}

	// llmClient.Catalog() is guaranteed non-nil (llm.NewClient defaults one in
	// when the caller doesn't supply one via WithCatalog), and its Pricing
	// method matches insights.Pricer's signature
	// (func(model string) (routing.ModelPricing, bool)) exactly, so it's
	// assignable directly — every Analyze run below now reports a real
	// Totals.CostUSD instead of the hardcoded 0 every local.Options{} call
	// site left it at before. llmClient is nil for the compact stage (which
	// never prices anything), so the pricer stays nil there too.
	var pricer insights.Pricer
	if llmClient != nil {
		pricer = llmClient.Catalog().Pricing
	}
	b := local.New(local.Options{Pool: pool, LLM: llmClient, Pricer: pricer})

	jsonMode := a.Indent || a.Compact

	if len(a.Compare) > 0 {
		runs, err := agentcli.Compare(ctx, b, agentcli.AnalyzeRequest{
			CorpusDir:  a.CorpusDir,
			Profile:    profile,
			StopAfter:  a.StopAfter,
			Force:      a.Force,
			Limit:      a.Limit,
			SkillFiles: skillFiles,
			NoStore:    a.NoStore,
		}, a.Compare, a.Out)
		if err != nil {
			return err
		}
		if jsonMode {
			return agentcli.RenderJSON(os.Stdout, compareRunsJSON(runs), a.Indent)
		}
		return agentcli.RenderCompare(os.Stdout, runs)
	}

	progressW := os.Stdout
	if jsonMode {
		// Keep stdout pure JSON; progress/skip/failure lines go to stderr.
		progressW = os.Stderr
	}

	if a.Out != "" {
		if err := agentcli.WritePromptsSidecar(a.Out, profile); err != nil {
			return fmt.Errorf("agent analyze --out: %w", err)
		}
	}

	events, err := b.Analyze(ctx, agentcli.AnalyzeRequest{
		ConversationIDs: a.ConversationIDs,
		CorpusDir:       a.CorpusDir,
		Profile:         profile,
		StopAfter:       a.StopAfter,
		Force:           a.Force,
		Limit:           a.Limit,
		SkillFiles:      skillFiles,
		NoStore:         a.NoStore,
	})
	if err != nil {
		return err
	}

	var summary *agentcli.Summary
	var payloads []any
	for ev := range events {
		switch ev.Kind {
		case agentcli.EventProgress:
			if err := agentcli.RenderProgress(progressW, ev.Progress); err != nil {
				return err
			}
		case agentcli.EventAnalysis:
			stage := a.StopAfter
			if stage == "" {
				stage = "detect"
			}
			switch {
			case a.Out != "":
				// --out is the durable record for this run; nothing more
				// goes to stdout for this event.
				if ev.Transcript != nil {
					if err := agentcli.WriteArtifacts(a.Out, ev.Transcript.ConversationID, "compact", ev.Transcript); err != nil {
						return err
					}
				} else if ev.Analysis != nil {
					if err := agentcli.WriteArtifacts(a.Out, ev.Analysis.ConversationID, stage, ev.Analysis); err != nil {
						return err
					}
				}
			case jsonMode:
				// Collect for the single terminal JSON object below —
				// stdout must stay pure JSON, so nothing is printed inline.
				if ev.Transcript != nil {
					payloads = append(payloads, ev.Transcript)
				} else if ev.Analysis != nil {
					payloads = append(payloads, ev.Analysis)
				}
			default:
				// No --out, human output: this is the ONLY place the
				// per-conversation payload is ever seen, so print it now —
				// previously it was silently dropped here (the bug this
				// replaces: "if a.Out == "" { continue }"). analyze.Analysis
				// has no exported markdown renderer (renderAnalysisMD is
				// unexported to agentcli), so it falls back to indented
				// JSON; the compact stage's Transcript does have one
				// (RenderTranscriptMD) and uses it.
				if ev.Transcript != nil {
					if err := agentcli.RenderTranscriptMD(os.Stdout, ev.Transcript); err != nil {
						return err
					}
				} else if ev.Analysis != nil {
					if err := agentcli.RenderJSON(os.Stdout, ev.Analysis, true); err != nil {
						return err
					}
				}
			}
		case agentcli.EventSummary:
			summary = ev.Summary
		case agentcli.EventError:
			return ev.Err
		}
	}

	// Wire up drafted skill edits: agentcli.WriteSkillEdits (already
	// implemented, just never called from this CLI) writes every ranked
	// finding's drafted file(s) under --out, in ranked order.
	var writtenFiles []string
	if a.Out != "" {
		writtenFiles, err = agentcli.WriteSkillEdits(a.Out, summary)
		if err != nil {
			return fmt.Errorf("agent analyze --out: write skill edits: %w", err)
		}
	}

	if jsonMode {
		return agentcli.RenderJSON(os.Stdout, analyzeResultJSON{
			Summary:           summary,
			Payloads:          payloads,
			WrittenSkillFiles: writtenFiles,
		}, a.Indent)
	}
	if err := agentcli.RenderAnalyzeSummary(os.Stdout, summary); err != nil {
		return err
	}
	return renderWrittenFiles(os.Stdout, writtenFiles)
}

// analyzeResultJSON is `agent analyze`'s -j/-J terminal payload for a
// full/partial-pipeline run (--compare has its own compareRunJSON view
// instead): the run Summary, plus — only when --out was NOT given, so
// nothing was written to disk — every per-conversation Transcript/Analysis
// that would otherwise have been silently dropped, and — only when --out WAS
// given — the skill-edit file paths WriteSkillEdits wrote.
type analyzeResultJSON struct {
	Summary           *agentcli.Summary `json:"summary,omitempty"`
	Payloads          []any             `json:"payloads,omitempty"`
	WrittenSkillFiles []string          `json:"written_skill_files,omitempty"`
}

// renderWrittenFiles prints the paths agentcli.WriteSkillEdits wrote —
// surfacing what a drafted-skill-edit run touched on disk. No byte count is
// available (agentcli.WriteSkillEdits returns only paths), so each line is the
// path.
func renderWrittenFiles(w io.Writer, files []string) error {
	if len(files) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Written skill files:"); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := fmt.Fprintf(w, "  %s\n", f); err != nil {
			return err
		}
	}
	return nil
}

// agentFindingsCmd runs `rafikid agent findings [flags]` (the read-only list
// path) plus its `dismiss <id>`/`action <id>` sub-dispatch. --db/-j/-J are
// parsed HERE, once, before dispatch — so `rafikid agent findings --db ...
// dismiss <id>` (a flag preceding the verb) reaches dismiss/action correctly.
// Dispatching off the raw pre-parse args[0] (as this used to) only worked
// when the verb happened to be first; a flag ahead of it silently fell
// through to the read-only list path instead of performing the mutation.
func agentFindingsCmd(args []string) error {
	fs := pflag.NewFlagSet("agent findings", pflag.ExitOnError)
	fs.Usage = func() {
		out := fs.Output()
		// A --help usage message is best-effort output on its own dedicated
		// writer (fs.Output(), normally stderr) with no meaningful recovery if
		// the write itself fails — same convention as flag.FlagSet's own
		// default Usage.
		_, _ = fmt.Fprintln(out, "usage: rafikid agent findings [--axis ...] [--skill ...] [--status ...] [-j|-J]")
		_, _ = fmt.Fprintln(out, "       rafikid agent findings dismiss <id> [--db ...] [-j|-J]")
		_, _ = fmt.Fprintln(out, "       rafikid agent findings action <id> [--db ...] [-j|-J]")
		fs.PrintDefaults()
	}
	db := fs.String("db", dbDSNDefault(), "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")
	axis := fs.String("axis", "", "filter by axis (list mode only)")
	skill := fs.String("skill", "", "filter by skill name (list mode only)")
	status := fs.String("status", "", "filter by status (default: open; list mode only)")
	indent := fs.BoolP("json", "j", false, "JSON output, indented")
	compact := fs.BoolP("json-compact", "J", false, "JSON output, compact")
	fs.SetNormalizeFunc(normalizeSingleDash)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// fs.Arg(0) is non-empty only if a positional argument followed every
	// flag fs.Parse consumed, regardless of where those flags appeared —
	// this is what makes `--db ... dismiss <id>` and `dismiss --db ... <id>`
	// both reach the verb correctly. Any OTHER non-empty first positional
	// (not dismiss/action) is an unknown command, which also covers the list
	// path ever seeing leftover positional args: NArg() > 0 with an empty
	// Arg(0) is impossible for a flag.FlagSet, so this default case is
	// exactly agentExportCmd's "reject leftover args" check, spelled for a
	// verb-dispatching command instead of a fixed-arity one.
	switch verb := fs.Arg(0); verb {
	case "dismiss", "action":
		return agentFindingsSetStatusCmd(verb, *db, fs.Args()[1:], *indent, *compact)
	case "":
		// list mode — falls through below.
	default:
		return fmt.Errorf("agent findings: unknown command %q (want dismiss or action)", verb)
	}

	ctx := context.Background()
	pool, err := connectPool(ctx, *db)
	if err != nil {
		return err
	}
	defer pool.Close()
	b := local.New(local.Options{Pool: pool})

	rows, err := b.Findings(ctx, store.FindingFilter{Axis: *axis, Skill: *skill, Status: *status})
	if err != nil {
		return err
	}
	return agentcli.Render(os.Stdout, rows, jsonMode(*indent, *compact), agentcli.RenderFindings)
}

// agentFindingsSetStatusCmd performs `rafikid agent findings dismiss|action
// <id>`'s mutation. db/indent/compact are already resolved by
// agentFindingsCmd's single flag parse; ids must be exactly the one
// remaining positional argument (agentFindingsCmd's own flag parse already
// consumed --db/-j/-J and the verb itself, wherever in args they appeared).
func agentFindingsSetStatusCmd(verb, db string, ids []string, indent, compact bool) error {
	if len(ids) != 1 {
		return fmt.Errorf("usage: rafikid agent findings %s <id> [--db ...] [-j|-J]", verb)
	}

	status := "dismissed"
	if verb == "action" {
		status = "actioned"
	}

	ctx := context.Background()
	pool, err := connectPool(ctx, db)
	if err != nil {
		return err
	}
	defer pool.Close()
	b := local.New(local.Options{Pool: pool})

	row, err := b.SetFindingStatus(ctx, ids[0], status)
	if err != nil {
		return err
	}
	// The value stays a bare row, not a one-element slice: JSON output of a
	// single-finding mutation is an object, and only the table wraps it.
	return agentcli.Render(os.Stdout, row, jsonMode(indent, compact),
		func(w io.Writer, r store.FindingRow) error { return agentcli.RenderFindings(w, []store.FindingRow{r}) })
}
