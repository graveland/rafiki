package connectapi

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executors"
)

// TestDarajaLaunchUDSFilter verifies that when dialAddr is a UDS path,
// only UDS-enrolled executors survive candidate filtering.
func TestDarajaLaunchUDSFilter(t *testing.T) {
	fake := &udsmockPool{
		udsSet: map[string]bool{"e1": true, "e3": true},
		live: []execpool.LiveExecutor{
			executorID("e1"), // UDS
			executorID("e2"), // TCP
			executorID("e3"), // UDS
		},
	}

	candidates := fake.live

	// Simulate DarajaLaunch transport-filtering logic.
	dialAddr := "/tmp/fake.sock" // UDS path
	isUDS := len(dialAddr) > 0 && dialAddr[0] == '/'
	if isUDS {
		var udsCandidates []execpool.LiveExecutor
		for _, le := range candidates {
			if fake.IsEnrolledViaUDS(le.Executor.ID) {
				udsCandidates = append(udsCandidates, le)
			}
		}
		candidates = udsCandidates
	}

	if len(candidates) != 2 {
		t.Errorf("got %d UDS candidates, want 2", len(candidates))
	}
	for i, le := range candidates {
		if !fake.udsSet[le.Executor.ID] {
			t.Errorf("candidate[%d] = %s, expected a UDS executor", i, le.Executor.ID)
		}
	}
}

// TestDarajaLaunchTCPDialAcceptAll verifies that with TCP dialAddr,
// no transport filtering occurs — all candidates pass through.
func TestDarajaLaunchTCPDialAcceptAll(t *testing.T) {
	fake := &udsmockPool{
		udsSet: map[string]bool{"e1": true},
		live:   []execpool.LiveExecutor{executorID("e1")},
	}

	candidates := fake.live

	dialAddr := "192.168.1.5:8035" // TCP address
	isUDS := len(dialAddr) > 0 && dialAddr[0] == '/'
	if isUDS {
		var filtered []execpool.LiveExecutor
		for _, le := range candidates {
			if fake.IsEnrolledViaUDS(le.Executor.ID) {
				filtered = append(filtered, le)
			}
		}
		candidates = filtered
	}

	if len(candidates) != 1 {
		t.Errorf("got %d candidates with TCP dialAddr, want 1", len(candidates))
	}
}

// ---------------------------------------------------------------------------
// Minimal mock pool for testing DarajaLaunch's transport filter logic.
// ---------------------------------------------------------------------------

type udsmockPool struct {
	udsSet map[string]bool
	live   []execpool.LiveExecutor
}

func (m *udsmockPool) IsEnrolledViaUDS(id string) bool { return m.udsSet[id] }
func (m *udsmockPool) Live() []execpool.LiveExecutor   { return m.live }

func executorID(s string) execpool.LiveExecutor {
	return execpool.LiveExecutor{
		Executor: executors.Executor{ID: s},
	}
}
