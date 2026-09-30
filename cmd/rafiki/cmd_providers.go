// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "Exclude OpenRouter providers from routing at runtime",
		Long: `Operator controls over which OpenRouter providers serve requests: ban a
provider outright, or set per-model-line routing policy. Both reach running
children — a ban or a row written now governs the daemon's next OpenRouter
request, whoever sends it.

Subcommands:
  bans          what is excluded right now, and why
  ban/unban     exclude a provider for every model, until lifted or --for
  route         per-model-line specs: sort, quantization floor, provider
                allowlist, data policy`,
	}
	cmd.AddCommand(newProvidersBansCmd(), newProvidersBanCmd(), newProvidersUnbanCmd(), newProvidersRouteCmd())
	return cmd
}

// ─── routing policy rows ─────────────────────────────────────────────────────

func newProvidersRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Routing-policy rows: the OpenRouter provider spec each model line resolves to",
		Long: `Per-model-line routing defaults. Resolution is per key: a row fills the
keys a spawn/preset spec leaves unset (a spawn's "[...]" beats a preset's,
which beats these rows), and nodata/zdr ALWAYS hold — they reach running
children too, so a row set after a spawn governs that child's later requests.

A policy line is a "-" prefix FAMILY: a row's line matches a model id equal to
it or extending it with "-" — "z-ai/glm-5.3" also governs "z-ai/glm-5.3-flash"
and stamped releases — unlike the cache guard's stamp-exact model lines. A line
names at most <model>/<id>: the provider segment is not part of a policy line,
and the store refuses longer shapes. Reading the rows is open to any caller;
writing one (set, delete) requires a user credential or the local socket.

The SPEC grammar (comma-separated, order-free):
  sort=price|throughput|latency|balanced
  quant=<floor>+           e.g. fp8+ — admit that tier and every tier above
  quant=a|b|c              an explicit list instead; floors cannot join a list
  only=slug|slug           serve only these providers
  nodata, zdr              bare flags: no prompt-retaining hosts / ZDR only
Resolution is per key: a spawn/preset spec beats a row key by key, and
nodata/zdr are also merged monotonically — a row can tighten them but no
spawn can loosen them.

Example:
  rafiki providers route set z-ai/glm-5.3-flash 'quant=fp8+'
      Serve the model only at 8-bit weights or better: every 4-bit
      quantization (int4, fp4, mxfp4, nvfp4) and fp6 is excluded, as are
      hosts OpenRouter has not labelled. To keep 6-bit hosts too, spell the
      list instead: 'quant=fp6|int8|fp8|mxfp8|fp16|bf16|fp32'.

  rafiki providers route set '*' 'sort=price,nodata'
      Global default: cheapest eligible provider first, and no provider
      that may store or train on prompts. Quote "*" — the shell would
      glob it.

  rafiki providers route set deepseek/deepseek-v4 'only=fireworks|deepinfra'
      Restrict a model line to named providers. Quote the spec: "|" is a
      shell pipe.`,
	}
	cmd.AddCommand(newProvidersRouteSetCmd(), newProvidersRouteListCmd(), newProvidersRouteDeleteCmd())
	return cmd
}

func newProvidersRouteSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <line|*> <spec>",
		Short: "Set the routing spec a model line resolves to",
		Long: `Set the routing spec a model line resolves to, replacing any previous row
for the line (a set is an append — the store keeps the history). SPEC is a
routing spec in the model string's bracket grammar, e.g.
"sort=price,quant=fp8+"; the empty string stores the zero spec (no opinion).
The daemon validates SPEC and refuses what it cannot parse.

Example:
  rafiki providers route set z-ai/glm-5.3-flash 'quant=fp8+'
  rafiki providers route set '*' 'sort=throughput'
  rafiki providers route set z-ai/glm-5.3-flash ''   # clear back to no opinion`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().SetRoute(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.SetRouteRequest{ModelLine: args[0], Spec: args[1]}))
			if err != nil {
				return err
			}
			row := resp.Msg.GetRow()
			if row.GetSpec() == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "set %s to the zero spec (no routing opinion)\n", row.GetModelLine())
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s to %s\n", row.GetModelLine(), row.GetSpec())
			return nil
		},
	}
}

func newProvidersRouteListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the live routing-policy rows: each model line and its spec",
		Long: `List the live routing-policy rows — the newest row per model line, tombstoned
ones hidden — with when each was written. This is the view the resolver
serves from; rows are appended on write, never edited in place.

Example:
  rafiki providers route list
  rafiki providers route list -J`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListRoutes(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListRoutesRequest{}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitProviderRoutes(cmd.OutOrStdout(), resp.Msg, mode, useColor)
		},
	}
}

func newProvidersRouteDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <line|*>",
		Short: "Remove the routing-policy row for a model line",
		Long: `Remove the routing-policy row for a model line. The removal is appended as
a tombstone — the history is kept, like every row in the log. Refused with
not_found when the line has no live row (never set, or already deleted).

Example:
  rafiki providers route delete z-ai/glm-5.3-flash`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			if _, err := ep.control().DeleteRoute(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.DeleteRouteRequest{ModelLine: args[0]})); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted the route for %s\n", args[0])
			return nil
		},
	}
}

// emitProviderRoutes writes ListRoutes' output in the resolved mode: -j is
// the response's canonical protojson (the rows envelope), -J one compact row
// per line, and the table is MODEL LINE, SPEC, SINCE.
func emitProviderRoutes(w io.Writer, resp *rafikiv1.ListRoutesResponse, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return emitProto(w, resp, mode)
	case outputJSONL:
		return emitProtoRows(w, resp.GetRows(), mode)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "MODEL LINE", "SPEC", "SINCE")...)
		for _, row := range resp.GetRows() {
			tb.Row(row.GetModelLine(), defaultDash(row.GetSpec()), routeSince(row.GetCreatedAt()))
		}
		return tb.Render()
	}
}

// routeSince renders a row's created_at — an RFC3339 string on the wire — the
// way the bans table renders its stamps: local, date+time. Anything that does
// not parse is passed through untouched rather than blanked.
func routeSince(raw string) string {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return t.Local().Format(time.DateTime)
}

// providerBanView is a ban as the CLI prints it in JSON: absolute times, and
// expires_at absent for a ban that lasts until lifted.
type providerBanView struct {
	Provider  string     `json:"provider"`
	ModelLine string     `json:"model_line"`
	Reason    string     `json:"reason"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Note      string     `json:"note,omitempty"`
}

func providerBanViewOf(b *rafikiv1.ProviderBan) providerBanView {
	v := providerBanView{
		Provider:  b.GetProvider(),
		ModelLine: b.GetModelLine(),
		Reason:    b.GetReason(),
		CreatedAt: time.Unix(b.GetCreatedAt(), 0),
		Note:      b.GetNote(),
	}
	if b.ExpiresAt != nil {
		t := time.Unix(b.GetExpiresAt(), 0)
		v.ExpiresAt = &t
	}
	return v
}

func newProvidersBansCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bans",
		Short: "List excluded providers: operator bans and the cache guard's ejections",
		Long: `List every provider currently excluded from routing: your bans (reason
"operator") and the cache guard's automatic ejections (e.g. "no_cache" for a
provider that stopped serving prompt-cache hits). An ejection expires on its
own TTL and does not respond to ` + "`unban`" + `; only an operator ban does.

Example:
  rafiki providers bans
  rafiki providers bans -J`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListProviderBans(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListProviderBansRequest{}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			if !resp.Msg.GetPersistent() {
				fmt.Fprintln(os.Stderr, "note: the daemon has no ejection log; bans last only until it restarts")
			}
			return emitProviderBans(os.Stdout, resp.Msg.GetBans(), mode, useColor, time.Now())
		},
	}
}

// emitProviderBans writes bans' output in the resolved mode. The table is
// PROVIDER, MODELS, REASON, SINCE, EXPIRES, NOTE.
func emitProviderBans(w io.Writer, bans []*rafikiv1.ProviderBan, mode outputMode, useColor bool, now time.Time) error {
	views := make([]providerBanView, 0, len(bans))
	for _, b := range bans {
		views = append(views, providerBanViewOf(b))
	}
	switch mode {
	case outputJSON:
		return writeJSON(w, map[string]any{"rows": views})
	case outputJSONL:
		anyRows := make([]any, len(views))
		for i, v := range views {
			anyRows[i] = v
		}
		return writeJSONL(w, anyRows)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "PROVIDER", "MODELS", "REASON", "SINCE", "EXPIRES", "NOTE")...)
		for _, v := range views {
			tb.Row(v.Provider, banScope(v.ModelLine), v.Reason,
				v.CreatedAt.Local().Format(time.DateTime), banExpiry(v.ExpiresAt, now), defaultDash(v.Note))
		}
		return tb.Render()
	}
}

func banScope(modelLine string) string {
	if modelLine == "*" {
		return "all"
	}
	return modelLine
}

func banExpiry(at *time.Time, now time.Time) string {
	if at == nil {
		return "when lifted"
	}
	return fmt.Sprintf("%s (in %s)", at.Local().Format(time.DateTime), at.Sub(now).Round(time.Minute))
}

func newProvidersBanCmd() *cobra.Command {
	var (
		dur  time.Duration
		note string
	)
	cmd := &cobra.Command{
		Use:   "ban <provider-slug>",
		Short: "Exclude an OpenRouter provider from every model, until lifted or for --for",
		Long: `Exclude an OpenRouter provider from routing for every model. The ban
takes effect on the daemon's next OpenRouter request, including for children
that are already running, and survives a daemon restart. Banning a provider
that is already banned replaces its expiry and note.

The provider is OpenRouter's slug ("fireworks", "open-inference") or its
display name ("OpenInference"), as the daemon's upstream_provider log shows
it; the daemon resolves either through OpenRouter's provider directory and
refuses a provider it does not list. Requires an admin user credential, or
the local socket.

Example:
  rafiki providers ban open-inference                # until lifted
  rafiki providers ban fireworks --for 6h --note '5xx storm'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &rafikiv1.BanProviderRequest{Provider: args[0], Note: note}
			if cmd.Flags().Changed("for") {
				if dur < time.Second {
					return fmt.Errorf("--for must be at least 1s, got %s (omit it to ban until lifted)", dur)
				}
				secs := int64(dur / time.Second)
				req.DurationSeconds = &secs
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().BanProvider(cmdCtx(cmd), connect.NewRequest(req))
			if err != nil {
				return err
			}
			v := providerBanViewOf(resp.Msg.GetBan())
			fmt.Printf("banned %s from all models until %s\n", v.Provider, banUntil(v.ExpiresAt))
			if !resp.Msg.GetPersistent() {
				fmt.Fprintln(os.Stderr, "warning: the daemon has no ejection log; this ban is lost when it restarts")
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&dur, "for", 0, "ban duration (e.g. 6h); omit to ban until lifted")
	cmd.Flags().StringVar(&note, "note", "", "why, shown in `rafiki providers bans`")
	return cmd
}

func banUntil(at *time.Time) string {
	if at == nil {
		return "lifted"
	}
	return at.Local().Format(time.DateTime)
}

func newProvidersUnbanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unban <provider-slug>",
		Short: "Lift an operator ban (the cache guard's own ejections expire on their own)",
		Long: `Lift an operator ban. The cache guard's own ejections are not operator bans —
they expire on their own, and unban is refused (not_found) for a provider with
no live operator ban, including one ejected only by the guard.

Example:
  rafiki providers unban open-inference`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			if _, err := ep.control().UnbanProvider(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.UnbanProviderRequest{Provider: args[0]})); err != nil {
				return err
			}
			fmt.Printf("lifted the ban on %s\n", args[0])
			return nil
		},
	}
}
