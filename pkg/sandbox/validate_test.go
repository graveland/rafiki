// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func baseCfg() Config {
	return Config{
		Image:         "base:img",
		Network:       protocol.NetworkEgress,
		TTL:           168 * time.Hour,
		MaxTTL:        720 * time.Hour,
		MaxPerOwner:   8,
		SweepInterval: 60 * time.Second,
	}
}

// namedSpec is a minimal valid named sandbox.
func namedSpec() protocol.SandboxSpec {
	return protocol.SandboxSpec{Name: "sbx-1", Image: "img:1"}
}

// spawnSpec is a minimal valid spawn-block sandbox.
func spawnSpec() protocol.SandboxSpec {
	return protocol.SandboxSpec{Image: "img:1", Scope: protocol.ScopeSelf}
}

func requireErr(t *testing.T, err error, want string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.Error(err, "Validate = nil error, want one mentioning %q", want)
	c.StrContains(err.Error(), want, "Validate error must name")
}

func TestValidateNamedNameRequired(t *testing.T) {
	s := namedSpec()
	s.Name = ""
	_, err := Validate(s, baseCfg(), nil, Caller{}, true)
	requireErr(t, err, "name is required")
}

func TestValidateNamedNameMachineName(t *testing.T) {
	s := namedSpec()
	s.Name = "bad name"
	_, err := Validate(s, baseCfg(), nil, Caller{}, true)
	requireErr(t, err, "name")
}

func TestValidateNamedScopeRefused(t *testing.T) {
	s := namedSpec()
	s.Scope = protocol.ScopeSelf
	_, err := Validate(s, baseCfg(), nil, Caller{}, true)
	requireErr(t, err, "scope is only valid on a spawn block")
}

func TestValidateSpawnNameRefused(t *testing.T) {
	s := spawnSpec()
	s.Name = "sbx"
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	requireErr(t, err, "name is only valid for a named sandbox")
}

func TestValidateSpawnScopeRequired(t *testing.T) {
	s := spawnSpec()
	s.Scope = ""
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	requireErr(t, err, "scope is required")
}

func TestValidateSpawnScopeInvalid(t *testing.T) {
	s := spawnSpec()
	s.Scope = "everyone"
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	requireErr(t, err, "scope must be")
}

func TestValidateSpawnTTLRefused(t *testing.T) {
	s := spawnSpec()
	s.TTL = time.Hour
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	requireErr(t, err, "ttl is only valid for a named sandbox")
}

func TestValidateImageRequired(t *testing.T) {
	s := namedSpec()
	s.Image = ""
	cfg := baseCfg()
	cfg.Image = ""
	_, err := Validate(s, cfg, nil, Caller{}, true)
	c := assert.NewAborting(t)
	c.EqualError(err, "sandbox: image is required: set image in the spec or RAFIKI_SANDBOX_IMAGE on the daemon")
}

func TestValidateImageFromConfig(t *testing.T) {
	s := namedSpec()
	s.Image = ""
	got, err := Validate(s, baseCfg(), nil, Caller{}, true)
	assert.NewAborting(t).NoError(err, "Validate")
	assert.NewAborting(t).Eq("base:img", got.Image, "Image resolved from config")
}

func TestValidateImageFromSpecWins(t *testing.T) {
	s := namedSpec()
	s.Image = "spec:img"
	got, err := Validate(s, baseCfg(), nil, Caller{}, true)
	assert.NewAborting(t).NoError(err, "Validate")
	assert.NewAborting(t).Eq("spec:img", got.Image, "Image from spec")
}

func TestValidateNamedTTLDefault(t *testing.T) {
	got, err := Validate(namedSpec(), baseCfg(), nil, Caller{}, true)
	c := assert.NewAborting(t)
	c.NoError(err, "Validate")
	c.Eq(168*time.Hour, got.TTL, "TTL defaulted to cfg.TTL")
}

func TestValidateNamedTTLKept(t *testing.T) {
	s := namedSpec()
	s.TTL = 2 * time.Hour
	got, err := Validate(s, baseCfg(), nil, Caller{}, true)
	c := assert.NewAborting(t)
	c.NoError(err, "Validate")
	c.Eq(2*time.Hour, got.TTL, "TTL kept")
}

func TestValidateNamedTTLNegative(t *testing.T) {
	s := namedSpec()
	s.TTL = -time.Hour
	_, err := Validate(s, baseCfg(), nil, Caller{}, true)
	requireErr(t, err, "ttl")
}

func TestValidateNamedTTLExceedsMax(t *testing.T) {
	s := namedSpec()
	s.TTL = 1000 * time.Hour
	_, err := Validate(s, baseCfg(), nil, Caller{}, true)
	requireErr(t, err, EnvMaxTTL)
}

func TestValidateMountsTarget(t *testing.T) {
	tests := []struct {
		subtest string
		mounts  []protocol.SandboxMount
		want    string
	}{
		{"relative", []protocol.SandboxMount{{Target: "data", Kind: protocol.MountEphemeral}}, "target must be an absolute path"},
		{"unclean", []protocol.SandboxMount{{Target: "/data/", Kind: protocol.MountEphemeral}}, "target must be a clean path"},
		{"with dotdot", []protocol.SandboxMount{{Target: "/a/../b", Kind: protocol.MountEphemeral}}, "target must be a clean path"},
		{"duplicate", []protocol.SandboxMount{
			{Target: "/data", Kind: protocol.MountEphemeral},
			{Target: "/data", Kind: protocol.MountEphemeral},
		}, "duplicated"},
	}
	for _, tt := range tests {
		t.Run(tt.subtest, func(t *testing.T) {
			s := spawnSpec()
			s.Mounts = tt.mounts
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			requireErr(t, err, tt.want)
		})
	}
}

func TestValidateMountKind(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := spawnSpec()
		s.Mounts = []protocol.SandboxMount{{Target: "/data"}}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		requireErr(t, err, "mounts[0].kind is required")
	})
	t.Run("unknown", func(t *testing.T) {
		s := spawnSpec()
		s.Mounts = []protocol.SandboxMount{{Target: "/data", Kind: "zfs"}}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		requireErr(t, err, "kind must be")
	})
}

func TestValidateMountSources(t *testing.T) {
	roots := []string{"/srv/repos"}
	tests := []struct {
		subtest string
		mount   protocol.SandboxMount
		want    string
	}{
		{"ro both sources", protocol.SandboxMount{Target: "/d", Kind: protocol.MountRO, HostPath: "/srv/repos/a", Volume: "v"}, "exactly one"},
		{"ro no source", protocol.SandboxMount{Target: "/d", Kind: protocol.MountRO}, "(ro) requires"},
		{"ephemeral with host", protocol.SandboxMount{Target: "/d", Kind: protocol.MountEphemeral, HostPath: "/srv/repos/a"}, "must not set a source"},
		{"ephemeral with volume", protocol.SandboxMount{Target: "/d", Kind: protocol.MountEphemeral, Volume: "v"}, "must not set a source"},
	}
	for _, tt := range tests {
		t.Run(tt.subtest, func(t *testing.T) {
			s := spawnSpec()
			s.Mounts = []protocol.SandboxMount{tt.mount}
			_, err := Validate(s, baseCfg(), roots, Caller{}, false)
			requireErr(t, err, tt.want)
		})
	}
	t.Run("rw no source is an anonymous volume", func(t *testing.T) {
		s := spawnSpec()
		s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW}}
		_, err := Validate(s, baseCfg(), roots, Caller{}, false)
		assert.NewAborting(t).NoError(err, "rw with neither source")
	})
}

func TestValidateMountHostPathRoots(t *testing.T) {
	roots := []string{"/srv/repos"}
	ok := []string{"/srv/repos", "/srv/repos/a/b"}
	for _, p := range ok {
		t.Run("ok "+p, func(t *testing.T) {
			s := spawnSpec()
			s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRO, HostPath: p}}
			_, err := Validate(s, baseCfg(), roots, Caller{}, false)
			assert.NewAborting(t).NoError(err, "host path %q under root", p)
		})
	}
	bad := []string{"/srv/reposX", "/srv", "/etc"}
	for _, p := range bad {
		t.Run("bad "+p, func(t *testing.T) {
			s := spawnSpec()
			s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRO, HostPath: p}}
			_, err := Validate(s, baseCfg(), roots, Caller{}, false)
			requireErr(t, err, "not under any launcher mount root")
		})
	}
	t.Run("unclean host path", func(t *testing.T) {
		s := spawnSpec()
		s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRO, HostPath: "/srv/repos/../etc"}}
		_, err := Validate(s, baseCfg(), roots, Caller{}, false)
		requireErr(t, err, "host_path must be an absolute clean path")
	})
}

func TestValidateMountHostPathNoRoots(t *testing.T) {
	s := spawnSpec()
	s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRO, HostPath: "/srv/repos/a"}}
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	requireErr(t, err, "--sandbox-mount-root")
}

func TestValidateMountVolumeName(t *testing.T) {
	s := spawnSpec()
	s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, Volume: "my_vol.1"}}
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	assert.NewAborting(t).NoError(err, "valid volume name")

	for _, v := range []string{"-bad", "bad/slash", "a b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		t.Run("bad "+v, func(t *testing.T) {
			s := spawnSpec()
			s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, Volume: v}}
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			requireErr(t, err, "volume")
		})
	}
}

func TestValidateWorkdir(t *testing.T) {
	for _, w := range []string{"", "/w", "/a/b"} {
		t.Run("ok "+w, func(t *testing.T) {
			s := spawnSpec()
			s.Workdir = w
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			assert.NewAborting(t).NoError(err, "workdir %q", w)
		})
	}
	for _, w := range []string{"w", "/w/", "/a/../b"} {
		t.Run("bad "+w, func(t *testing.T) {
			s := spawnSpec()
			s.Workdir = w
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			requireErr(t, err, "workdir")
		})
	}
}

func TestValidateNetwork(t *testing.T) {
	t.Run("empty resolves to config", func(t *testing.T) {
		s := spawnSpec()
		s.Network = ""
		got, err := Validate(s, baseCfg(), nil, Caller{}, false)
		c := assert.NewAborting(t)
		c.NoError(err, "Validate")
		c.Eq(protocol.NetworkEgress, got.Network, "Network from config")
	})
	t.Run("none kept", func(t *testing.T) {
		s := spawnSpec()
		s.Network = protocol.NetworkNone
		got, err := Validate(s, baseCfg(), nil, Caller{}, false)
		c := assert.NewAborting(t)
		c.NoError(err, "Validate")
		c.Eq(protocol.NetworkNone, got.Network, "Network none")
	})
	t.Run("invalid", func(t *testing.T) {
		s := spawnSpec()
		s.Network = "bridge"
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		requireErr(t, err, "network must be")
	})
}

func TestValidateEnv(t *testing.T) {
	t.Run("empty key", func(t *testing.T) {
		s := spawnSpec()
		s.Env = map[string]string{"": "x"}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		requireErr(t, err, "empty key")
	})
	t.Run("RAFIKI_ prefix refused", func(t *testing.T) {
		s := spawnSpec()
		s.Env = map[string]string{"RAFIKI_X": "1"}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		requireErr(t, err, "reserved")
	})
	t.Run("ordinary key ok", func(t *testing.T) {
		s := spawnSpec()
		s.Env = map[string]string{"PATH": "/usr/bin"}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		assert.NewAborting(t).NoError(err, "ordinary env key")
	})
}

func TestValidateUser(t *testing.T) {
	for _, u := range []string{"", "1000", "1000:1000", "root", "a.b-c_d"} {
		t.Run("ok "+u, func(t *testing.T) {
			s := spawnSpec()
			s.User = u
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			assert.NewAborting(t).NoError(err, "user %q", u)
		})
	}
	for _, u := range []string{"1000:", ":1000", "a b", "a/b"} {
		t.Run("bad "+u, func(t *testing.T) {
			s := spawnSpec()
			s.User = u
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			requireErr(t, err, "user")
		})
	}
}

func TestValidateLimitsNegative(t *testing.T) {
	tests := []struct {
		subtest string
		mut     func(*protocol.SandboxSpec)
		want    string
	}{
		{"memory", func(s *protocol.SandboxSpec) { s.MemoryBytes = -1 }, "memory_bytes"},
		{"cpus", func(s *protocol.SandboxSpec) { s.CPUs = -0.5 }, "cpus"},
		{"pids", func(s *protocol.SandboxSpec) { s.PidsLimit = -1 }, "pids_limit"},
	}
	for _, tt := range tests {
		t.Run(tt.subtest, func(t *testing.T) {
			s := spawnSpec()
			tt.mut(&s)
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			requireErr(t, err, tt.want)
		})
	}
}

func TestValidateLimitsOperatorNotClamped(t *testing.T) {
	cfg := baseCfg()
	cfg.ChildMaxMemoryBytes = 1 << 30
	cfg.ChildMaxCPUs = 2
	cfg.ChildMaxPids = 100
	s := spawnSpec() // all limits zero
	got, err := Validate(s, cfg, nil, Caller{Child: false}, false)
	c := assert.NewAborting(t)
	c.NoError(err, "Validate")
	c.Eq(int64(0), got.MemoryBytes, "operator memory unclamped")
	c.Eq(0.0, got.CPUs, "operator cpus unclamped")
	c.Eq(int64(0), got.PidsLimit, "operator pids unclamped")
}

func TestValidateLimitsChildClamped(t *testing.T) {
	cfg := baseCfg()
	cfg.ChildMaxMemoryBytes = 1 << 30
	cfg.ChildMaxCPUs = 2
	cfg.ChildMaxPids = 100

	t.Run("zero set to cap", func(t *testing.T) {
		got, err := Validate(spawnSpec(), cfg, nil, Caller{Child: true}, false)
		c := assert.NewAborting(t)
		c.NoError(err, "Validate")
		c.Eq(int64(1<<30), got.MemoryBytes, "memory set to cap")
		c.Eq(2.0, got.CPUs, "cpus set to cap")
		c.Eq(int64(100), got.PidsLimit, "pids set to cap")
	})
	t.Run("below cap kept", func(t *testing.T) {
		s := spawnSpec()
		s.MemoryBytes = 1 << 20
		s.CPUs = 1
		s.PidsLimit = 10
		got, err := Validate(s, cfg, nil, Caller{Child: true}, false)
		c := assert.NewAborting(t)
		c.NoError(err, "Validate")
		c.Eq(int64(1<<20), got.MemoryBytes, "memory kept")
		c.Eq(1.0, got.CPUs, "cpus kept")
		c.Eq(int64(10), got.PidsLimit, "pids kept")
	})
	t.Run("above cap refused", func(t *testing.T) {
		s := spawnSpec()
		s.MemoryBytes = 2 << 30
		_, err := Validate(s, cfg, nil, Caller{Child: true}, false)
		requireErr(t, err, EnvChildMaxMemory)
	})
	t.Run("cpus above cap refused", func(t *testing.T) {
		s := spawnSpec()
		s.CPUs = 4
		_, err := Validate(s, cfg, nil, Caller{Child: true}, false)
		requireErr(t, err, EnvChildMaxCPUs)
	})
	t.Run("pids above cap refused", func(t *testing.T) {
		s := spawnSpec()
		s.PidsLimit = 200
		_, err := Validate(s, cfg, nil, Caller{Child: true}, false)
		requireErr(t, err, EnvChildMaxPids)
	})
}

func TestValidateLabels(t *testing.T) {
	t.Run("empty key", func(t *testing.T) {
		s := spawnSpec()
		s.Labels = map[string]string{"": "x"}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		requireErr(t, err, "empty key")
	})
	for _, k := range []string{"rafiki/x", "rafiki.x", "fundi/x", "owner", "machine"} {
		t.Run("reserved "+k, func(t *testing.T) {
			s := spawnSpec()
			s.Labels = map[string]string{k: "v"}
			_, err := Validate(s, baseCfg(), nil, Caller{}, false)
			requireErr(t, err, "reserved")
		})
	}
	t.Run("ordinary key ok", func(t *testing.T) {
		s := spawnSpec()
		s.Labels = map[string]string{"team": "x", "rafikian": "y"}
		_, err := Validate(s, baseCfg(), nil, Caller{}, false)
		assert.NewAborting(t).NoError(err, "ordinary label keys")
	})
}

func TestValidateMountsMayBeEmpty(t *testing.T) {
	s := spawnSpec()
	s.Mounts = nil
	_, err := Validate(s, baseCfg(), nil, Caller{}, false)
	assert.NewAborting(t).NoError(err, "no mounts is valid")
}

func TestValidateDoesNotMutateInput(t *testing.T) {
	cfg := baseCfg()
	cfg.ChildMaxMemoryBytes = 1 << 30
	cfg.ChildMaxCPUs = 2
	cfg.ChildMaxPids = 100

	s := spawnSpec()
	s.Mounts = []protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, Volume: "v"}}
	s.Env = map[string]string{"A": "B"}
	s.Labels = map[string]string{"team": "x"}

	got, err := Validate(s, cfg, nil, Caller{Child: true}, false)
	c := assert.NewAborting(t)
	c.NoError(err, "Validate")
	// The resolved copy carries the clamp...
	c.Eq(int64(1<<30), got.MemoryBytes, "resolved memory clamped")
	c.Eq(2.0, got.CPUs, "resolved cpus clamped")
	c.Eq(int64(100), got.PidsLimit, "resolved pids clamped")
	c.Eq(protocol.NetworkEgress, got.Network, "resolved network filled")
	c.Eq(time.Duration(0), got.TTL, "spawn-block resolved ttl stays zero")

	// ...while the caller's spec is untouched.
	c.Eq(int64(0), s.MemoryBytes, "input memory unmutated")
	c.Eq(0.0, s.CPUs, "input cpus unmutated")
	c.Eq(int64(0), s.PidsLimit, "input pids unmutated")
	c.Eq("", string(s.Network), "input network unmutated")
	c.EqDiff(map[string]string{"A": "B"}, s.Env, "input env unmutated")
	c.EqDiff(map[string]string{"team": "x"}, s.Labels, "input labels unmutated")
	c.EqDiff([]protocol.SandboxMount{{Target: "/d", Kind: protocol.MountRW, Volume: "v"}}, s.Mounts, "input mounts unmutated")
}

func TestValidateNamedHappyPath(t *testing.T) {
	s := namedSpec()
	s.Launcher = "docker@home"
	s.Mounts = []protocol.SandboxMount{{Target: "/data", Kind: protocol.MountRW, Volume: "v"}}
	s.Workdir = "/w"
	s.User = "1000:1000"
	got, err := Validate(s, baseCfg(), nil, Caller{}, true)
	c := assert.NewAborting(t)
	c.NoError(err, "Validate")
	c.Eq("sbx-1", got.Name, "Name")
	c.Eq("img:1", got.Image, "Image")
	c.Eq(168*time.Hour, got.TTL, "TTL defaulted")
	c.Eq(protocol.NetworkEgress, got.Network, "Network defaulted")
	c.Eq("", string(got.Scope), "Scope empty for named")
}
