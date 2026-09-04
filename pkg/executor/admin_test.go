// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

func TestDescribeAdvertisesLaunchKinds(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), LaunchKinds: []string{"claude"}})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !slices.Contains(resp.Msg.GetLaunchKinds(), "claude") {
		t.Errorf("launch_kinds = %v, want claude", resp.Msg.GetLaunchKinds())
	}
}

// An executor with no --launch flag hosts nothing. The default must be empty
// rather than "claude": a machine volunteering to host other people's children
// because someone forgot a flag is the self-report-gates-placement shape the
// isolation and workspace_mode rules exist to forbid.
func TestDescribeAdvertisesNoLaunchKindsByDefault(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir()})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := resp.Msg.GetLaunchKinds(); len(got) != 0 {
		t.Errorf("launch_kinds = %v, want empty", got)
	}
}
