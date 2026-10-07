// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// bodyResolved is a minimal resolved sandbox for create-body tests. It is built
// directly (not through Validate) so a test can exercise CreateBody in
// isolation.
func bodyResolved() Resolved {
	return Resolved{SandboxSpec: protocol.SandboxSpec{
		Image:   "img:1",
		Network: protocol.NetworkEgress,
	}}
}

func bodyInputs() CreateInputs {
	return CreateInputs{
		SandboxID:      "sbx-1",
		Credential:     "cred",
		RelayHostDir:   "/host/relay",
		OwnerVolumeKey: "ownerk",
	}
}

func decodeBody(t *testing.T, data []byte) createBody {
	t.Helper()
	var b createBody
	assert.NewAborting(t).NoError(json.Unmarshal(data, &b), "decode create body")
	return b
}

func TestCreateBodyImageEntrypointCmd(t *testing.T) {
	data, err := CreateBody(bodyResolved(), bodyInputs())
	c := assert.NewAborting(t)
	c.NoError(err, "CreateBody")
	b := decodeBody(t, data)
	c.Eq("img:1", b.Image, "Image")
	c.EqDeep([]string{"rafiki"}, b.Entrypoint, "Entrypoint")
	c.EqDeep([]string{"executor", "serve", "--connect-socket", ContainerRelayDir + "/daemon.sock"}, b.Cmd, "Cmd")
}

func TestCreateBodyWorkingDir(t *testing.T) {
	r := bodyResolved()
	r.Workdir = "/work"
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	assert.NewAborting(t).Eq("/work", b.WorkingDir, "WorkingDir")

	// Unset means absent.
	b2 := decodeBody(t, mustBody(t, bodyResolved(), bodyInputs()))
	assert.NewAborting(t).Eq("", b2.WorkingDir, "WorkingDir omitted when unset")
}

func TestCreateBodyEnvSorted(t *testing.T) {
	r := bodyResolved()
	r.Env = map[string]string{"B": "2", "A": "1"}
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	assert.NewAborting(t).EqDeep(
		[]string{"A=1", "B=2", CredentialEnv + "=cred"},
		b.Env, "Env sorted with the credential appended")
}

func TestCreateBodyLabels(t *testing.T) {
	r := bodyResolved()
	r.Labels = map[string]string{"team": "x"}
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	c := assert.NewAborting(t)
	c.Eq("x", b.Labels["team"], "caller label kept")
	c.Eq("sbx-1", b.Labels[DockerLabelSandbox], "sandbox label")
	_, hasChild := b.Labels[DockerLabelChild]
	c.False(hasChild, "no child label for a named sandbox")

	t.Run("owner child label added", func(t *testing.T) {
		in := bodyInputs()
		in.OwnerChild = "child-7"
		b := decodeBody(t, mustBody(t, r, in))
		assert.NewAborting(t).Eq("child-7", b.Labels[DockerLabelChild], "child label")
	})

	t.Run("daemon labels win", func(t *testing.T) {
		r := bodyResolved()
		r.Labels = map[string]string{DockerLabelSandbox: "forged", DockerLabelChild: "forged"}
		in := bodyInputs()
		in.OwnerChild = "child-7"
		b := decodeBody(t, mustBody(t, r, in))
		c := assert.NewAborting(t)
		c.Eq("sbx-1", b.Labels[DockerLabelSandbox], "sandbox label overrides the caller's")
		c.Eq("child-7", b.Labels[DockerLabelChild], "child label overrides the caller's")
	})
}

func TestCreateBodyMountsBind(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{
		{Target: "/ro", Kind: protocol.MountRO, HostPath: "/host/ro"},
		{Target: "/rw", Kind: protocol.MountRW, HostPath: "/host/rw"},
	}
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	c := assert.NewAborting(t)
	c.Len(b.HostConfig.Mounts, 3, "two spec mounts plus the relay mount")
	c.EqDeep(mount{Type: "bind", Source: "/host/ro", Target: "/ro", ReadOnly: true}, b.HostConfig.Mounts[0], "ro bind")
	c.EqDeep(mount{Type: "bind", Source: "/host/rw", Target: "/rw", ReadOnly: false}, b.HostConfig.Mounts[1], "rw bind")
}

func TestCreateBodyMountsVolume(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{
		{Target: "/ro", Kind: protocol.MountRO, Volume: "data"},
		{Target: "/rw", Kind: protocol.MountRW, Volume: "work"},
	}
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	c := assert.NewAborting(t)
	c.EqDeep(mount{Type: "volume", Source: "rafiki-ownerk-data", Target: "/ro", ReadOnly: true}, b.HostConfig.Mounts[0], "ro volume")
	c.EqDeep(mount{Type: "volume", Source: "rafiki-ownerk-work", Target: "/rw", ReadOnly: false}, b.HostConfig.Mounts[1], "rw volume")
}

func TestCreateBodyMountsAnonymousVolume(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{{Target: "/scratch", Kind: protocol.MountRW}}
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	c := assert.NewAborting(t)
	c.EqDeep(mount{Type: "volume", Target: "/scratch"}, b.HostConfig.Mounts[0], "anonymous volume has no Source")
	c.Eq("", b.HostConfig.Mounts[0].Source, "no source")
}

func TestCreateBodyMountsEphemeral(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{{Target: "/tmp", Kind: protocol.MountEphemeral}}
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	assert.NewAborting(t).EqDeep(mount{Type: "tmpfs", Target: "/tmp"}, b.HostConfig.Mounts[0], "tmpfs")
}

func TestCreateBodyRelayMount(t *testing.T) {
	b := decodeBody(t, mustBody(t, bodyResolved(), bodyInputs()))
	c := assert.NewAborting(t)
	c.NotEmpty(b.HostConfig.Mounts, "relay mount present even with no spec mounts")
	last := b.HostConfig.Mounts[len(b.HostConfig.Mounts)-1]
	c.EqDeep(mount{Type: "bind", Source: "/host/relay", Target: ContainerRelayDir, ReadOnly: true}, last, "relay mount")
}

func TestCreateBodyNetwork(t *testing.T) {
	t.Run("egress is bridge", func(t *testing.T) {
		r := bodyResolved()
		r.Network = protocol.NetworkEgress
		b := decodeBody(t, mustBody(t, r, bodyInputs()))
		assert.NewAborting(t).Eq("bridge", b.HostConfig.NetworkMode, "NetworkMode")
	})
	t.Run("none is none", func(t *testing.T) {
		r := bodyResolved()
		r.Network = protocol.NetworkNone
		b := decodeBody(t, mustBody(t, r, bodyInputs()))
		assert.NewAborting(t).Eq("none", b.HostConfig.NetworkMode, "NetworkMode")
	})
}

func TestCreateBodyReadonlyRootfs(t *testing.T) {
	r := bodyResolved()
	r.ReadOnlyRootfs = true
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	assert.NewAborting(t).True(b.HostConfig.ReadonlyRootfs, "ReadonlyRootfs")

	b2 := decodeBody(t, mustBody(t, bodyResolved(), bodyInputs()))
	assert.NewAborting(t).False(b2.HostConfig.ReadonlyRootfs, "ReadonlyRootfs false when unset")
}

func TestCreateBodyLimits(t *testing.T) {
	r := bodyResolved()
	r.MemoryBytes = 512 << 20
	r.CPUs = 1.5
	r.PidsLimit = 256
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	c := assert.NewAborting(t)
	c.Eq(int64(512<<20), b.HostConfig.Memory, "Memory")
	c.Eq(int64(1_500_000_000), b.HostConfig.NanoCpus, "NanoCpus")
	c.Eq(int64(256), b.HostConfig.PidsLimit, "PidsLimit")

	// Zero means unset (omitted).
	b2 := decodeBody(t, mustBody(t, bodyResolved(), bodyInputs()))
	c2 := assert.NewAborting(t)
	c2.Eq(int64(0), b2.HostConfig.Memory, "Memory unset")
	c2.Eq(int64(0), b2.HostConfig.NanoCpus, "NanoCpus unset")
	c2.Eq(int64(0), b2.HostConfig.PidsLimit, "PidsLimit unset")
}

func TestCreateBodyRestartPolicy(t *testing.T) {
	b := decodeBody(t, mustBody(t, bodyResolved(), bodyInputs()))
	assert.NewAborting(t).EqDeep(restartPolicy{Name: "unless-stopped"}, b.HostConfig.RestartPolicy, "RestartPolicy")
}

func TestCreateBodyUser(t *testing.T) {
	r := bodyResolved()
	r.User = "1000:1000"
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	assert.NewAborting(t).Eq("1000:1000", b.User, "User")

	b2 := decodeBody(t, mustBody(t, bodyResolved(), bodyInputs()))
	assert.NewAborting(t).Eq("", b2.User, "User omitted when unset")
}

// TestCreateBodyExpressesNoPrivilege pins that the emitted JSON can express no
// privilege: none of Docker's privilege keys appear anywhere.
func TestCreateBodyExpressesNoPrivilege(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, HostPath: "/host/d"}}
	data := mustBody(t, r, bodyInputs())

	var top map[string]json.RawMessage
	c := assert.NewAborting(t)
	c.NoError(json.Unmarshal(data, &top), "decode body")

	var hc map[string]json.RawMessage
	c.NoError(json.Unmarshal(top["HostConfig"], &hc), "decode HostConfig")

	for _, key := range []string{"Privileged", "CapAdd", "CapDrop", "Devices", "PidMode", "SecurityOpt", "Binds", "IPC", "UTSMode", "UsernsMode"} {
		_, ok := hc[key]
		c.False(ok, "HostConfig must not carry %q", key)
	}
	// Binds belongs on HostConfig; assert it is absent at the top level too.
	_, topBinds := top["Binds"]
	c.False(topBinds, "top level must not carry Binds")
}

func TestCreateBodyRequiresOwnerKeyForVolumes(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, Volume: "v"}}
	in := bodyInputs()
	in.OwnerVolumeKey = ""
	_, err := CreateBody(r, in)
	assert.NewAborting(t).Error(err, "a volume mount without an owner volume key is refused")
}

// TestCreateBodyTinyCPUsRefused pins that a positive cpus that truncates to
// zero nanocpus is refused rather than emitting an omitted (unlimited) limit.
func TestCreateBodyTinyCPUsRefused(t *testing.T) {
	r := bodyResolved()
	r.CPUs = 1e-10
	_, err := CreateBody(r, bodyInputs())
	c := assert.NewAborting(t)
	c.Error(err, "a cpus that rounds to zero nanocpus is refused")
	c.StrContains(err.Error(), "cpus", "error names cpus")
}

// TestCreateBodyHugeCPUsRefused pins that a cpus past the int64 nanocpu range
// is refused rather than making the conversion undefined.
func TestCreateBodyHugeCPUsRefused(t *testing.T) {
	r := bodyResolved()
	r.CPUs = 1e30
	_, err := CreateBody(r, bodyInputs())
	c := assert.NewAborting(t)
	c.Error(err, "a cpus past the nanocpu range is refused")
	c.StrContains(err.Error(), "cpus", "error names cpus")
}

// TestCreateBodyCPUsNormal pins that an ordinary fractional cpus is emitted.
func TestCreateBodyCPUsNormal(t *testing.T) {
	r := bodyResolved()
	r.CPUs = 0.5
	b := decodeBody(t, mustBody(t, r, bodyInputs()))
	assert.NewAborting(t).Eq(int64(500_000_000), b.HostConfig.NanoCpus, "NanoCpus")
}

// TestCreateBodyOwnerVolumeKeyRefused pins that a key containing the volume
// separator '-' is refused: it would let two owners collide on one volume name.
func TestCreateBodyOwnerVolumeKeyRefused(t *testing.T) {
	r := bodyResolved()
	r.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, Volume: "v"}}
	for _, key := range []string{"a-b", "a.b", "a/b", "a b"} {
		t.Run(key, func(t *testing.T) {
			in := bodyInputs()
			in.OwnerVolumeKey = key
			_, err := CreateBody(r, in)
			c := assert.NewAborting(t)
			c.Error(err, "owner volume key %q is refused", key)
			c.StrContains(err.Error(), "owner volume key", "error names the key")
		})
	}
}

// TestCreateBodyNetworkZeroRefused pins that an unresolved network is refused
// rather than silently becoming egress.
func TestCreateBodyNetworkZeroRefused(t *testing.T) {
	r := bodyResolved()
	r.Network = ""
	_, err := CreateBody(r, bodyInputs())
	c := assert.NewAborting(t)
	c.Error(err, "an unset network is refused")
	c.StrContains(err.Error(), "network", "error names network")
}

func TestCreateBodyNetworkUnknownRefused(t *testing.T) {
	r := bodyResolved()
	r.Network = "host"
	_, err := CreateBody(r, bodyInputs())
	c := assert.NewAborting(t)
	c.Error(err, "an unknown network is refused")
	c.StrContains(err.Error(), "network", "error names network")
}

// mustBody is mustBody(t, r, in) with the error asserted nil.
func mustBody(t *testing.T, r Resolved, in CreateInputs) []byte {
	t.Helper()
	data, err := CreateBody(r, in)
	assert.NewAborting(t).NoError(err, "CreateBody")
	return data
}
