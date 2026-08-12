// Package executors provides the executor domain types, label selector
// parsing and matching, and the narrow operation that enforces the
// confidentiality boundary between a parent agent and its children.
package executors

import "time"

// Executor is a live machine that runs children's filesystem and shell tools.
// The database row is authoritative for identity and trust labels, not the
// credential the executor presents — which is what makes relabelling and
// revocation row updates that need no reissue and no machine access.
type Executor struct {
	ID            string            `json:"id"`
	DisplayName   string            `json:"displayName"`
	Labels        map[string]string `json:"labels"`
	SelfReported  map[string]string `json:"selfReported,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	Roots         []string          `json:"roots"`
	Isolation     string            `json:"isolation"`
	WorkspaceMode string            `json:"workspaceMode"`
	Admits        string            `json:"admits"` // executor-side admission selector
	Enabled       bool              `json:"enabled"`
	EnrolledAt    time.Time         `json:"enrolledAt"`
	LastSeenAt    time.Time         `json:"lastSeenAt,omitempty"`
}
