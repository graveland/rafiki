// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// executorCacheTTL mirrors modelCacheTTL: an executor's live/eligible status
// changes on connect/disconnect, so completion answers are cached briefly
// rather than forever.
const executorCacheTTL = modelCacheTTL

// completeExecutor returns tab-completion candidates for the --executor flag,
// offering machine names first (what a human actually types) and raw ids for
// executors with no machine label.
func completeExecutor(cmd *cobra.Command, kind, toComplete string) []string {
	rows, err := executorRows(cmd, kind)
	if err != nil {
		return nil
	}
	refs := make([]string, 0, len(rows))
	for _, r := range rows {
		if m := r.GetMachine(); m != "" {
			refs = append(refs, m)
		} else {
			refs = append(refs, r.GetId())
		}
	}
	return filterByPrefix(refs, toComplete)
}

// executorRows asks the daemon for its live executor rows, scoped by kind,
// cached like modelIDs.
func executorRows(cmd *cobra.Command, kind string) ([]*rafikiv1.ExecutorRow, error) {
	cacheKind := "executors-" + kind
	var refs []string
	// The cache only stores refs (strings), matching modelIDs' shape; a
	// completion candidate list does not need the full row.
	if cacheRead(cacheKind, completionEndpointKey(cmd), executorCacheTTL, &refs) {
		rows := make([]*rafikiv1.ExecutorRow, 0, len(refs))
		for _, ref := range refs {
			rows = append(rows, &rafikiv1.ExecutorRow{Machine: ref})
		}
		return rows, nil
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionDeadline)
	defer cancel()
	resp, err := ep.control().ListExecutors(ctx, connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: kind}))
	if err != nil {
		return nil, err
	}
	rows := resp.Msg.GetRows()
	refs = make([]string, 0, len(rows))
	for _, r := range rows {
		if m := r.GetMachine(); m != "" {
			refs = append(refs, m)
		} else {
			refs = append(refs, r.GetId())
		}
	}
	cacheWrite(cacheKind, completionEndpointKey(cmd), refs)
	return rows, nil
}

// refFor is the human-readable ref a resolved row should be remembered/passed
// back as: the machine label when the row has one, else the raw id.
func refFor(row *rafikiv1.ExecutorRow) string {
	if m := row.GetMachine(); m != "" {
		return m
	}
	return row.GetId()
}

// resolveLaunchExecutor picks an executor for a kind that must be LAUNCHED
// (anything but fundi) when the caller named neither --executor nor
// --executor-selector. It is the client-side half of
// docs/plans/2026-09-06-executor-selection-design.md §1: rather than let the
// daemon silently pick among several candidates, this asks first via
// ListExecutors and only proceeds when the answer is unambiguous.
//
// An empty return with a nil error means "leave it to the daemon" — either no
// executor supports the kind at all (Spawn's own refusal explains that more
// clearly than duplicating it here) or exactly one is eligible and does not
// need explicit naming... no: exactly one IS returned. Empty+nil means ZERO
// eligible executors.
func resolveLaunchExecutor(ctx context.Context, cmd *cobra.Command, profileName, kind string) (string, error) {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return "", err
	}
	resp, err := ep.control().ListExecutors(ctx, connect.NewRequest(&rafikiv1.ListExecutorsRequest{Kind: kind}))
	if err != nil {
		return "", fmt.Errorf("listing executors for --kind %s: %w", kind, err)
	}
	var eligible []*rafikiv1.ExecutorRow
	for _, row := range resp.Msg.GetRows() {
		if row.GetEligible() {
			eligible = append(eligible, row)
		}
	}

	remembered := clientstate.LastExecutorFor(profileName, kind)
	for _, row := range eligible {
		if refFor(row) == remembered {
			return remembered, nil
		}
	}

	switch len(eligible) {
	case 0:
		return "", nil
	case 1:
		return refFor(eligible[0]), nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "multiple executors can launch a %q child; pass --executor to pick one:\n", kind)
		for _, row := range eligible {
			fmt.Fprintf(&b, "  %-20s  %s\n", refFor(row), row.GetId())
		}
		return "", errors.New(b.String())
	}
}
