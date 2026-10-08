// SPDX-License-Identifier: Apache-2.0

package executor_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

// TestDescribeAdvertisesTreeSync pins that Describe self-reports tree_sync, so
// the daemon knows this executor accepts ReadTree/WriteTree and the Git* RPCs.
func TestDescribeAdvertisesTreeSync(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := executor.NewServer(executor.Options{Root: t.TempDir(), Concurrency: 6, Version: "test"})
	client := newTestClient(t, srv)

	resp, err := client.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	c.Require().NoError(err, "Describe")
	c.True(resp.Msg.GetTreeSync(), "Describe must advertise tree_sync")
}
