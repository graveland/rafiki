package main

import (
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// resolveExecutor decides where a child's machine-local tools run.
//
// A nil client with a nil error means "no executor", which after plan 09 means
// the child has no workspace tier at all rather than running the tools in this
// process.
func (c *Controller) resolveExecutor(req protocol.SpawnRequest, ownerName string) (tools.ExecutorClient, error) {
	if req.ExecutorSelector == "" {
		return nil, nil
	}
	return c.selectExecutor(req, ownerName)
}
