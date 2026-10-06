package protocol_test

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestSandboxConstants pins the exact wire strings for the sandbox enums. A
// change here is a wire-breaking change.
func TestSandboxConstants(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("ro", string(protocol.MountRO), "MountRO")
	c.Eq("rw", string(protocol.MountRW), "MountRW")
	c.Eq("ephemeral", string(protocol.MountEphemeral), "MountEphemeral")
	c.Eq("egress", string(protocol.NetworkEgress), "NetworkEgress")
	c.Eq("none", string(protocol.NetworkNone), "NetworkNone")
	c.Eq("self", string(protocol.ScopeSelf), "ScopeSelf")
	c.Eq("subtree", string(protocol.ScopeSubtree), "ScopeSubtree")
}

// TestSandboxZeroValues pins that the zero value of each sandbox enum is the
// empty string — the "unset" sentinel the validator refuses — rather than a
// meaningful default reachable by an ordinary Go mistake.
func TestSandboxZeroValues(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("", string(protocol.MountKind("")), "zero MountKind")
	c.Eq("", string(protocol.NetworkMode("")), "zero NetworkMode")
	c.Eq("", string(protocol.SandboxScope("")), "zero SandboxScope")

	var spec protocol.SandboxSpec
	c.Eq("", spec.Name, "zero SandboxSpec.Name")
	c.Eq(protocol.MountKind(""), (protocol.SandboxMount{}).Kind, "zero spec mount kind")

	var info protocol.SandboxInfo
	c.Eq("", info.State, "zero SandboxInfo.State")
	c.Eq(false, info.Connected, "zero SandboxInfo.Connected")
	c.Nil(info.ExpiresAt, "zero SandboxInfo.ExpiresAt")
}

// TestSandboxStructFields pins that the shapes carry the fields the daemon and
// CLI exchange, including a duration TTL and a pointer ExpiresAt (absent for a
// spawn-block sandbox, which never expires on its own).
func TestSandboxStructFields(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Now()
	exp := now.Add(time.Hour)

	spec := protocol.SandboxSpec{
		Name:           "sbx",
		Launcher:       "docker@home",
		Image:          "img:1",
		Mounts:         []protocol.SandboxMount{{Target: "/data", Kind: protocol.MountRW, Volume: "vol"}},
		Workdir:        "/w",
		Network:        protocol.NetworkNone,
		ReadOnlyRootfs: true,
		Env:            map[string]string{"A": "B"},
		User:           "1000:1000",
		MemoryBytes:    1 << 30,
		CPUs:           2.5,
		PidsLimit:      128,
		Labels:         map[string]string{"team": "x"},
		TTL:            24 * time.Hour,
		Scope:          protocol.ScopeSubtree,
	}
	c.Eq("sbx", spec.Name, "Name")
	c.Eq(24*time.Hour, spec.TTL, "TTL is a Duration")
	c.Eq(2.5, spec.CPUs, "CPUs")
	c.Eq(protocol.ScopeSubtree, spec.Scope, "Scope")

	info := protocol.SandboxInfo{
		ID:          "id",
		Name:        "sbx",
		ExecutorID:  "ex",
		Launcher:    "docker@home",
		ContainerID: "c",
		Image:       "img:1",
		Network:     protocol.NetworkEgress,
		State:       "ready",
		Connected:   true,
		CreatedBy:   "child-1",
		OwnerChild:  "child-1",
		Scope:       protocol.ScopeSelf,
		Labels:      map[string]string{"team": "x"},
		CreatedAt:   now,
		ExpiresAt:   &exp,
	}
	c.Eq("ready", info.State, "State")
	c.Eq("child-1", info.OwnerChild, "OwnerChild")
	c.NotNil(info.ExpiresAt, "ExpiresAt")
	c.Eq(exp, *info.ExpiresAt, "ExpiresAt value")
}
