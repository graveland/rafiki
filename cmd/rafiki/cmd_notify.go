// SPDX-License-Identifier: Apache-2.0

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

func newNotifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify <agent> [field=value ...]",
		Short: "Deliver a third-party notification to a top-level agent",
		Long: `Deliver a notification from a third-party integration to one of your
top-level agents, by NAME. The daemon resolves the name among your live
top-level agents and hands the agent a prompt describing the notification;
it answers not_found when no live top-level agent carries that name.

AGENT is a top-level agent name, sent as-is: the daemon resolves it, so this
verb works with a notify-scoped token that cannot list children. Fields are
name=value pairs, split on the FIRST "=" — so "title=a=b" sets title to
"a=b". A value that is awkward to quote can come from a file instead, with
--field-file name=path; a path of "-" reads stdin, and at most one field may
use it. File content is used verbatim, unlike a positional value.

Name a field twice — across positionals and --field-file alike — and the verb
refuses rather than silently choosing one. The daemon validates the rest of
the payload (field-name syntax, UTF-8, size limits) and refuses an invalid
one with invalid_argument, never by truncating.

Example:
  rafiki notify ops-bot --source github --key repos/acme/rafiki/push \
      event=push branch=main
  rafiki notify ops-bot --source sentry --field-file body=payload.json
  echo '{"alert":"disk"}' | rafiki notify ops-bot --source pagerduty \
      --field-file body=-

The notified agent's child id prints on success; -j/-J emit the daemon's
response as protojson instead. A refusal prints on stderr and exits
non-zero.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runNotify,
		// No candidates on purpose: completing an agent name means listing
		// children, which a notify-scoped token is not allowed to do.
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().String("source", "", "integration that sent the notification (required)")
	cmd.Flags().String("key", "", "the integration's own notification key")
	cmd.Flags().StringArray("field-file", nil, "name=path: field value read from a file; path - reads stdin (may be used once)")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}

func runNotify(cmd *cobra.Command, args []string) error {
	source, _ := cmd.Flags().GetString("source")
	key, _ := cmd.Flags().GetString("key")
	fieldFiles, _ := cmd.Flags().GetStringArray("field-file")

	req, err := buildNotifyRequest(args[0], source, key, args[1:], fieldFiles, os.Stdin)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().Notify(cmdCtx(cmd), connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	if mode == outputJSON || mode == outputJSONL {
		return emitProto(cmd.OutOrStdout(), resp.Msg, mode)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "notified %s\n", resp.Msg.GetChildId())
	return err
}

// buildNotifyRequest turns the verb's arguments and flags into the wire
// request: positional field=value pairs first (split on the FIRST "=", so a
// value may itself contain "="), then --field-file entries, whose content is
// read verbatim. stdin backs a path of "-", and at most one field may use it.
//
// stdin is a parameter rather than os.Stdin, so the builder does no daemon
// work and its stdin half is testable without a terminal. The name/UTF-8/size
// rules are the
// daemon's to enforce (pkg/notify): the client only refuses what it cannot
// even shape into a map — a pair without "=" and a name given twice.
func buildNotifyRequest(target, source, key string, positional, fieldFiles []string, stdin io.Reader) (*rafikiv1.NotifyRequest, error) {
	if source == "" {
		return nil, fmt.Errorf("--source is required")
	}
	fields := make(map[string]string, len(positional)+len(fieldFiles))
	seen := make(map[string]struct{}, len(positional)+len(fieldFiles))

	add := func(name, value string) error {
		if _, dup := seen[name]; dup {
			return fmt.Errorf("field %q given twice", name)
		}
		seen[name] = struct{}{}
		fields[name] = value
		return nil
	}

	for _, arg := range positional {
		name, value, ok := strings.Cut(arg, "=")
		if !ok {
			return nil, fmt.Errorf("field %q is not name=value", arg)
		}
		if err := add(name, value); err != nil {
			return nil, err
		}
	}

	usedStdin := false
	for _, pair := range fieldFiles {
		name, path, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("field %q is not name=value", pair)
		}
		var (
			content []byte
			err     error
		)
		if path == "-" {
			if usedStdin {
				return nil, fmt.Errorf("at most one field may read stdin (%q)", name)
			}
			usedStdin = true
			content, err = io.ReadAll(stdin)
		} else {
			content, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		if err := add(name, string(content)); err != nil {
			return nil, err
		}
	}

	return &rafikiv1.NotifyRequest{
		Target: target,
		Source: source,
		Key:    key,
		Fields: fields,
	}, nil
}
