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
	}
	cmd.AddCommand(newProvidersBansCmd(), newProvidersBanCmd(), newProvidersUnbanCmd())
	return cmd
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
		Args:  cobra.NoArgs,
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

The provider is OpenRouter's slug ("fireworks", "open-inference"); a display
name is lowercased with spaces turned into dashes. Requires an admin user
credential, or the local socket.`,
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
		Args:  cobra.ExactArgs(1),
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
