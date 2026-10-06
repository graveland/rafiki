// SPDX-License-Identifier: Apache-2.0

package protocol

import "time"

// Sandbox is a container running `rafiki executor serve`, chosen with its own
// mounts, network and resource limits, and bound to a child or reached
// directly. These are the pure data shapes; the daemon owns creation and the
// launcher (pkg/sandbox) owns the engine calls.

// MountKind is how a sandbox mount is backed.
type MountKind string

const (
	// MountRO is a read-only bind of a host path or named volume.
	MountRO MountKind = "ro"
	// MountRW is a read-write bind of a host path or named volume; with no
	// source it is an anonymous volume removed with the container.
	MountRW MountKind = "rw"
	// MountEphemeral is a tmpfs, and forbids a source.
	MountEphemeral MountKind = "ephemeral"
)

// NetworkMode is a sandbox's network reach.
type NetworkMode string

const (
	// NetworkEgress reaches the network (the default).
	NetworkEgress NetworkMode = "egress"
	// NetworkNone reaches nothing.
	NetworkNone NetworkMode = "none"
)

// SandboxScope is how far a spawn-block sandbox's executor is offered.
type SandboxScope string

const (
	// ScopeSelf offers the sandbox only to the owning child itself.
	ScopeSelf SandboxScope = "self"
	// ScopeSubtree offers it to the owning child and its descendants.
	ScopeSubtree SandboxScope = "subtree"
)

// SandboxMount is one mount in a sandbox.
//
// Exactly one of HostPath / Volume backs a ro or rw mount; rw may have
// neither (an anonymous volume). An ephemeral mount has neither (it is a
// tmpfs).
type SandboxMount struct {
	Target   string
	Kind     MountKind
	HostPath string
	Volume   string
}

// SandboxSpec is the typed request to create a sandbox.
type SandboxSpec struct {
	Name           string // named sandboxes only
	Launcher       string // executor ref/selector; "" = the sole docker launcher in scope
	Image          string
	Mounts         []SandboxMount
	Workdir        string
	Network        NetworkMode
	ReadOnlyRootfs bool
	Env            map[string]string
	User           string
	MemoryBytes    int64
	CPUs           float64
	PidsLimit      int64
	Labels         map[string]string
	TTL            time.Duration // named only
	Scope          SandboxScope  // spawn block only
}

// SandboxInfo is a sandbox as reported to a caller.
type SandboxInfo struct {
	ID, Name, ExecutorID, Launcher, ContainerID, Image string
	Network                                            NetworkMode
	State                                              string // "creating" | "ready" | "lost" | "removing"
	Connected                                          bool
	CreatedBy                                          string // creating child id, "" for an operator
	OwnerChild                                         string // spawn-block owner, "" for a named sandbox
	Scope                                              SandboxScope
	Labels                                             map[string]string
	CreatedAt                                          time.Time
	ExpiresAt                                          *time.Time
}
