// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newModelsRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route MODEL[SPEC]",
		Short: "Show where a model's requests would be served under a routing spec",
		Long: `Show every endpoint OpenRouter could serve a model from, and which of them a
routing spec admits. The daemon answers the question; this verb only renders it.

MODEL is an OpenRouter model id, optionally carrying a routing bracket in the
same grammar a spawn or a preset uses — the server parses it, so it reaches the
daemon verbatim:

  rafiki models route deepseek/deepseek-v4.1-flash[sort=price,quant=fp8+]
  rafiki models route 'openrouter/z-ai/glm-5.3[only=fireworks|deepinfra]'

Eligible endpoints are listed in the try-order the spec predicts, each with its
quantization, price, measured throughput/latency and uptime; rows the spec
excludes are hidden unless --all is given. The header names the model and the
canonical spec the daemon resolved ("none" when the model carries no bracket),
and a stale endpoint list or an unavailable stats scrape is called out rather
than silently rendered as missing data.

Query flags:
  --sort F[:asc|:desc]  order the table client-side; F is tps (default),
                        latency, price or uptime. An endpoint whose value is
                        unknown sorts LAST in both directions — an absent
                        measurement is no answer, never the smallest.
  --all                 include endpoints the spec excludes

The table is the default output; -j/-J/-o json|jsonl emit the daemon's response
as protojson instead, which --sort and --all do not affect.`,
		Args: cobra.ExactArgs(1),
		RunE: runModelsRoute,
	}
	cmd.Flags().String("sort", "tps:desc", "Order the table: tps|latency|price|uptime[:asc|:desc]")
	cmd.Flags().Bool("all", false, "Include endpoints the spec excludes")
	_ = cmd.RegisterFlagCompletionFunc("sort", cobra.FixedCompletions(
		[]string{"tps", "tps:asc", "latency", "latency:asc", "price", "price:asc", "uptime", "uptime:asc"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func runModelsRoute(cmd *cobra.Command, args []string) error {
	// Parse --sort BEFORE the round trip: a typo is user input and must fail
	// fast, not after a stats scrape.
	key, err := modelsRouteSortKeyFromFlags(cmd)
	if err != nil {
		return err
	}
	all, _ := cmd.Flags().GetBool("all")

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	// The argument is passed through VERBATIM — the daemon parses the routing
	// bracket, and a client that pre-split it would be a second parser to keep
	// in sync with pkg/routing's grammar.
	resp, err := ep.control().ModelRoutes(cmdCtx(cmd),
		connect.NewRequest(&rafikiv1.ModelRoutesRequest{Model: args[0]}))
	if err != nil {
		return err
	}
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return emitModelRoutes(cmd.OutOrStdout(), cmd.ErrOrStderr(), resp.Msg, key, all, mode, useColor)
}

// emitModelRoutes renders ModelRoutes' response. The stats note is advisory and
// always goes to stderr so a piped table or JSON stays clean; -j is the whole
// response's canonical protojson and -J one endpoint per line. --sort/--all
// shape the table only.
func emitModelRoutes(w, errW io.Writer, resp *rafikiv1.ModelRoutesResponse, key modelsRouteSortKey, all bool, mode outputMode, useColor bool) error {
	if note := resp.GetStatsNote(); note != "" {
		fmt.Fprintf(errW, "stats unavailable: %s\n", note)
	}
	switch mode {
	case outputJSON:
		return emitProto(w, resp, mode)
	case outputJSONL:
		return emitProtoRows(w, resp.GetEndpoints(), mode)
	}
	return renderModelRoutesTable(w, resp, key, all, useColor)
}

// renderModelRoutesTable renders the eligible-first endpoint table at an
// explicit writer. The sorted slice is a copy: the wire response's order is
// what JSON output and a subsequent caller see, never a display sort.
func renderModelRoutesTable(w io.Writer, resp *rafikiv1.ModelRoutesResponse, key modelsRouteSortKey, all bool, useColor bool) error {
	rows := sortedRouteEndpoints(resp.GetEndpoints(), key)

	shown := make([]*rafikiv1.RouteEndpoint, 0, len(rows))
	excluded := 0
	for _, e := range rows {
		if !e.GetEligible() {
			excluded++
			if !all {
				continue
			}
		}
		shown = append(shown, e)
	}

	// The header line names the model the daemon resolved (bracket stripped)
	// and the canonical spec it applied; "none" is the daemon's own wording for
	// an empty spec, not a missing answer.
	routing := resp.GetRouting()
	if routing == "" {
		routing = "none"
	}
	header := resp.GetModel() + "  spec: " + routing
	if resp.GetStale() {
		header += " (cached list is stale)"
	}
	fmt.Fprintln(w, header)

	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "PROVIDER", "QUANT", "TPS", "P90", "LAT ms", "IN $/M", "OUT $/M", "UPTIME %", "#", "NOTE")...)
	for _, e := range shown {
		tb.Row(
			dashWhen(e.GetProvider()),
			dashWhen(e.GetQuantization()),
			routeIntCell(e.P50TokensPerSec),
			routeIntCell(e.P90TokensPerSec),
			routeIntCell(e.P50LatencyMs),
			routePriceCell(e.PromptUsdPerMtok),
			routePriceCell(e.CompletionUsdPerMtok),
			routeUptimeCell(e.Uptime_30M),
			routeRankCell(e),
			routeNoteCell(e),
		)
	}
	if err := tb.Render(); err != nil {
		return err
	}
	if !all && excluded > 0 {
		fmt.Fprintf(w, "%d endpoint(s) excluded by this spec (--all to show)\n", excluded)
	}
	return nil
}

// routeIntCell renders an optional measured value as an integer — throughput in
// tokens/sec, latency in ms. Absent is an em dash, never a 0 that would read as
// a measured stall.
func routeIntCell(v *float64) string {
	if v == nil {
		return "—"
	}
	return strconv.Itoa(int(math.Round(*v)))
}

// routePriceCell renders a USD-per-million price. The wire already carries
// per-million units, so this is no perMillion: multiplying again would be a
// million-fold lie.
func routePriceCell(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

// routeUptimeCell renders the uptime fraction OpenRouter reports as a
// percentage with one decimal. Absent stays absent.
func routeUptimeCell(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *v*100)
}

// routeRankCell is the 1-based try-order rank among eligible endpoints; an
// excluded endpoint has no rank and renders "-" (distinct from the "—" used for
// a genuinely unmeasured value).
func routeRankCell(e *rafikiv1.RouteEndpoint) string {
	if !e.GetEligible() {
		return "-"
	}
	return strconv.Itoa(int(e.GetRank()))
}

// routeNoteCell names why an endpoint is interesting: "preferred" for a slug
// the spec's prefer= names, otherwise the exclusion reason.
func routeNoteCell(e *rafikiv1.RouteEndpoint) string {
	if e.GetPreferred() {
		return "preferred"
	}
	return dashWhen(e.GetExcludedReason())
}

// ─── client-side sort ────────────────────────────────────────────────────────

// modelsRouteSortField is the endpoint stat a --sort key orders by. Its zero
// value is tps, which is also the default key — not a trap here because the key
// only ever arrives through modelsRouteSortKeyFromFlags.
type modelsRouteSortField int

const (
	routeSortTPS modelsRouteSortField = iota
	routeSortLatency
	routeSortPrice
	routeSortUptime
)

// value reads the field off an endpoint and reports whether it is present. The
// presence flag is what makes absence outrank direction in the comparator.
func (f modelsRouteSortField) value(e *rafikiv1.RouteEndpoint) (float64, bool) {
	switch f {
	case routeSortLatency:
		return e.GetP50LatencyMs(), e.P50LatencyMs != nil
	case routeSortPrice:
		return e.GetPromptUsdPerMtok(), e.PromptUsdPerMtok != nil
	case routeSortUptime:
		return e.GetUptime_30M(), e.Uptime_30M != nil
	default:
		return e.GetP50TokensPerSec(), e.P50TokensPerSec != nil
	}
}

type modelsRouteSortKey struct {
	field modelsRouteSortField
	desc  bool
}

// modelsRouteSortKeyFromFlags parses --sort F[:asc|:desc]. An omitted direction
// is ascending (the models query's rule); the flag's default value spells
// tps:desc, which is what a bare `rafiki models route` uses.
func modelsRouteSortKeyFromFlags(cmd *cobra.Command) (modelsRouteSortKey, error) {
	s, _ := cmd.Flags().GetString("sort")
	field, dir := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 {
		field, dir = s[:i], s[i+1:]
	}
	desc := false
	switch dir {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return modelsRouteSortKey{}, fmt.Errorf("unknown sort direction %q (try :asc or :desc)", dir)
	}
	var f modelsRouteSortField
	switch field {
	case "tps":
		f = routeSortTPS
	case "latency":
		f = routeSortLatency
	case "price":
		f = routeSortPrice
	case "uptime":
		f = routeSortUptime
	default:
		return modelsRouteSortKey{}, fmt.Errorf("unknown sort field %q (try tps, latency, price, uptime)", field)
	}
	return modelsRouteSortKey{field: f, desc: desc}, nil
}

// sortedRouteEndpoints orders a copy of eps by key, stably: ties keep the
// daemon's response order (its predicted try-order), and an endpoint whose
// value is absent sorts LAST IN BOTH DIRECTIONS — an absent measurement is not
// the smallest, it is no answer (modelquery.Compare's rule).
func sortedRouteEndpoints(eps []*rafikiv1.RouteEndpoint, key modelsRouteSortKey) []*rafikiv1.RouteEndpoint {
	out := append([]*rafikiv1.RouteEndpoint(nil), eps...)
	sort.SliceStable(out, func(i, j int) bool {
		av, aok := key.field.value(out[i])
		bv, bok := key.field.value(out[j])
		switch {
		case !aok && !bok:
			return false
		case !aok:
			return false // a is absent: it stays last
		case !bok:
			return true // b is absent: it goes last
		case av == bv:
			return false
		case key.desc:
			return av > bv
		default:
			return av < bv
		}
	})
	return out
}
