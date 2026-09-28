// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/routepolicy"
)

// connectRoutes adapts routepolicy's store and in-memory policy to
// connectapi.RouteManager. Every write goes to the append-only store first,
// then reloads the Policy from the store's Active rows, so the resolver the
// request paths consult reflects the write immediately — the same
// write-then-refresh contract the provider cache guard serves its bans with.
type connectRoutes struct {
	store  routepolicy.Store
	policy *routepolicy.Policy
}

// newConnectRoutes builds the adapter over store and policy. Neither is
// constructed here: main.go owns their lifetime (task 3.1 wires them), and
// this file deliberately constructs nothing global.
func newConnectRoutes(store routepolicy.Store, policy *routepolicy.Policy) connectRoutes {
	return connectRoutes{store: store, policy: policy}
}

func (c connectRoutes) ListRoutes(context.Context) ([]connectapi.RouteRow, error) {
	rows := c.policy.Rows()
	out := make([]connectapi.RouteRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, routeRowOf(r))
	}
	return out, nil
}

func (c connectRoutes) SetRoute(ctx context.Context, line, spec string) (connectapi.RouteRow, error) {
	if err := c.store.Set(ctx, line, spec); err != nil {
		return connectapi.RouteRow{}, routeStoreErr(err)
	}
	rows, err := c.reload(ctx)
	if err != nil {
		return connectapi.RouteRow{}, err
	}
	// Answer with the row as stored — Active's created_at, not a local
	// time.Now() guess at what the database stamped.
	for _, r := range rows {
		if r.ModelLine == line {
			return routeRowOf(r), nil
		}
	}
	return connectapi.RouteRow{}, errors.New("routepolicy: written row for " + line + " did not appear in Active")
}

func (c connectRoutes) DeleteRoute(ctx context.Context, line string) error {
	if err := c.store.Delete(ctx, line); err != nil {
		return routeStoreErr(err)
	}
	_, err := c.reload(ctx)
	return err
}

// reload refreshes the in-memory Policy from the store's Active rows and
// returns what was loaded. Load replaces the whole view atomically and parses
// every row first, so a failed load leaves the previous view serving.
func (c connectRoutes) reload(ctx context.Context) ([]routepolicy.Row, error) {
	rows, err := c.store.Active(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.policy.Load(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func routeRowOf(r routepolicy.Row) connectapi.RouteRow {
	return connectapi.RouteRow{ModelLine: r.ModelLine, Spec: r.Spec, CreatedAt: r.CreatedAt}
}

// routeStoreErr translates routepolicy's sentinel to connectapi's, so the
// handler can map it to NotFound without importing routepolicy — whose pgx
// store must never link into the client, which links connectapi.
func routeStoreErr(err error) error {
	if errors.Is(err, routepolicy.ErrNotFound) {
		return fmt.Errorf("%w: %v", connectapi.ErrRouteNotFound, err)
	}
	return err
}
