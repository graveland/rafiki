package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "search <query>",
		Aliases: []string{"find"},
		Short:   "Search across live children's events",
		Args:    cobra.ExactArgs(1),
		RunE:    runSearch,
	}
	cmd.Flags().Bool("regex", false, "Treat query as a regular expression")
	cmd.Flags().IntP("limit", "l", 50, "Maximum hits to return")
	cmd.Flags().Int("context", 2, "Context lines around each hit")
	cmd.Flags().String("cwd-contains", "", "Restrict to children whose cwd contains this")
	cmd.Flags().String("name-contains", "", "Restrict to children whose name contains this")
	cmd.Flags().StringArray("label", nil, "AND-match session label k=v (repeatable)")
	cmd.Flags().StringArray("has-label", nil, "Restrict to sessions that have this label key (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("label", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelPairs(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("has-label", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelKeys(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func runSearch(cmd *cobra.Command, args []string) error {
	// Resolve the mode before dialing: a malformed combination (-j -J) is a
	// user-input error and must not cost a connection — or, on the verbs that
	// change state, an action.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}

	regex, _ := cmd.Flags().GetBool("regex")
	limit, _ := cmd.Flags().GetInt("limit")
	context, _ := cmd.Flags().GetInt("context")
	cwd, _ := cmd.Flags().GetString("cwd-contains")
	name, _ := cmd.Flags().GetString("name-contains")

	// Optional label filters for session scoping.
	var labelFilter map[string]string
	if labelPairs, _ := cmd.Flags().GetStringArray("label"); len(labelPairs) > 0 {
		var err error
		labelFilter, err = parseLabelPairs(labelPairs)
		if err != nil {
			return fmt.Errorf("--label: %w", err)
		}
	}
	hasLabels, _ := cmd.Flags().GetStringArray("has-label")

	req := &rafikiv1.SearchRequest{
		Query:   args[0],
		Regex:   regex,
		Limit:   int32(limit),
		Context: int32(context),
	}
	if cwd != "" || name != "" || len(labelFilter) > 0 || len(hasLabels) > 0 {
		req.SessionFilter = &rafikiv1.SearchRequest_SearchSessionFilter{
			CwdContains:  cwd,
			NameContains: name,
			Labels:       labelFilter,
			HasLabel:     hasLabels,
		}
	}

	resp, err := ep.control().Search(cmdCtx(cmd), connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	switch mode {
	case outputJSON, outputJSONL:
		// The canonical protojson of the Search response, pretty or — for a
		// piped consumer — one hit object per line, unwrapped.
		if mode == outputJSONL {
			return emitProtoRows(os.Stdout, resp.Msg.GetHits(), mode)
		}
		return emitProto(os.Stdout, resp.Msg, mode)
	default:
		return renderSearchText(os.Stdout, resp.Msg.GetHits())
	}
}

// renderSearchText writes the hits grouped by child, grep-style: a
// `== <childID> <label>` header per child, then each hit as
// `<ordinal>: <line>` with any further snippet lines indented two spaces as
// context. Ordinals are 1-based and restart within each group, mirroring
// grep's per-file line numbers. The hit the ordinal names is the snippet's
// first line — the daemon serves one event blob per hit (clipped to 256
// bytes) and its MatchStart is an offset into the full event, so there is no
// reliable way to locate the match inside the snippet. Zero hits write
// nothing, like grep.
func renderSearchText(w io.Writer, hits []*rafikiv1.SearchResponse_SearchHit) error {
	var order []string
	groups := make(map[string][]*rafikiv1.SearchResponse_SearchHit)
	for _, h := range hits {
		if _, ok := groups[h.GetChildId()]; !ok {
			order = append(order, h.GetChildId())
		}
		groups[h.GetChildId()] = append(groups[h.GetChildId()], h)
	}
	for _, id := range order {
		group := groups[id]
		header := "== " + id
		if label := searchGroupLabel(group[0]); label != "" {
			header += " " + label
		}
		if _, err := fmt.Fprintln(w, header); err != nil {
			return err
		}
		for i, h := range group {
			snippet := strings.TrimRight(h.GetSnippet(), "\r\n")
			for j, line := range strings.Split(snippet, "\n") {
				var err error
				if j == 0 {
					if line == "" {
						_, err = fmt.Fprintf(w, "%d:\n", i+1)
					} else {
						_, err = fmt.Fprintf(w, "%d: %s\n", i+1, line)
					}
				} else {
					_, err = fmt.Fprintf(w, "  %s\n", line)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// searchGroupLabel picks the header's second token: the child's name when the
// hit carries one, else the session file. SearchHit has no cwd — the plan's
// `== <childID> <cwd>` becomes the identifying pair the record actually
// carries.
func searchGroupLabel(h *rafikiv1.SearchResponse_SearchHit) string {
	if h.GetSessionName() != "" {
		return h.GetSessionName()
	}
	return h.GetSessionFile()
}
