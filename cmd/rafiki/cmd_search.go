package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
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

	c := mustDial(cmd)
	defer c.Close()

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

	req := protocol.SearchRequest{
		Type:    protocol.TypeCtrlSearch,
		Query:   args[0],
		Regex:   regex,
		Limit:   limit,
		Context: context,
	}
	if cwd != "" || name != "" || len(labelFilter) > 0 || len(hasLabels) > 0 {
		req.SessionFilter = &protocol.SearchSessionFilter{
			CwdContains:  cwd,
			NameContains: name,
			Labels:       labelFilter,
			HasLabel:     hasLabels,
		}
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_search: %s", client.FormatError(resp))
	}

	switch mode {
	case outputJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(resp.Data))
	case outputJSONL:
		var raw struct {
			Hits []json.RawMessage `json:"hits"`
		}
		if err := json.Unmarshal(resp.Data, &raw); err != nil {
			return fmt.Errorf("decode search response: %w", err)
		}
		return renderSearchJSONL(os.Stdout, raw.Hits)
	default:
		var data protocol.SearchResponseData
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return fmt.Errorf("decode search response: %w", err)
		}
		return renderSearchText(os.Stdout, data.Hits)
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
func renderSearchText(w io.Writer, hits []protocol.SearchHit) error {
	var order []string
	groups := make(map[string][]protocol.SearchHit)
	for _, h := range hits {
		if _, ok := groups[h.ChildID]; !ok {
			order = append(order, h.ChildID)
		}
		groups[h.ChildID] = append(groups[h.ChildID], h)
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
			snippet := strings.TrimRight(h.Snippet, "\r\n")
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
func searchGroupLabel(h protocol.SearchHit) string {
	if h.SessionName != "" {
		return h.SessionName
	}
	return h.SessionFile
}

// renderSearchJSONL writes one hit object per line. Hits pass through as
// received rather than being decoded and re-encoded, so a field this client
// binary does not know about still reaches the consumer.
func renderSearchJSONL(w io.Writer, hits []json.RawMessage) error {
	rows := make([]any, len(hits))
	for i, h := range hits {
		rows[i] = h
	}
	return writeJSONL(w, rows)
}
