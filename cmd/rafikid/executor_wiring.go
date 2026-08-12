package main

import (
	"context"
	"fmt"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// resolveExecutor decides where a child's machine-local tools run.
//
// This is the seam phase 07's registry replaces: the signature stays, the body
// grows a selector path. Keep it a single function with a single return —
// every caller must get its executor from exactly one place, or a later
// selector path and this static path will disagree about which one won.
//
// A nil client, nil error means "everything runs in-process", which is the
// default and preserves the pre-executor behaviour exactly.
func (c *Controller) resolveExecutor(req protocol.SpawnRequest) (tools.ExecutorClient, error) {
	// A label selector wins over a named socket: it is the path the daemon
	// can audit, and a request carrying both is asking for the audited one.
	if req.ExecutorSelector != "" {
		return c.selectExecutor(req)
	}
	if req.ExecutorSocket == "" {
		return nil, nil
	}
	cl, err := executorclient.Dial(req.ExecutorSocket)
	if err != nil {
		return nil, fmt.Errorf("executor at %s is unreachable: %w", req.ExecutorSocket, err)
	}
	if err := cl.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("executor at %s is unreachable: %w", req.ExecutorSocket, err)
	}
	return cl, nil
}
