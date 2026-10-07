// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"

	"github.com/multigres/testkit/assert"
)

// createBody builds a container-create body carrying exactly the given
// HostConfig.Mounts (and nothing else that the guard reads).
func createBody(t *testing.T, mounts ...map[string]string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"Mounts": mounts},
	})
	assert.NewAborting(t).NoError(err, "marshal body")
	return b
}

func bindMount(src, target string) map[string]string {
	return map[string]string{"Type": "bind", "Source": src, "Target": target}
}

func TestDockerGuardAllowsBindUnderRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	sub := filepath.Join(root, "data")
	c.Require().NoError(os.Mkdir(sub, 0o755), "mkdir")
	c.NoError(checkCreateBody(createBody(t, bindMount(sub, "/data")), []string{root}, ""), "bind under root")
}

func TestDockerGuardRefusesBindOutsideRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	outside := t.TempDir()
	err := checkCreateBody(createBody(t, bindMount(outside, "/data")), []string{root}, "")
	c.Require().Error(err, "bind outside root")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardRefusesSymlinkEscapingRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	c.Require().NoError(os.Symlink(outside, link), "symlink")
	err := checkCreateBody(createBody(t, bindMount(link, "/data")), []string{root}, "")
	c.Require().Error(err, "symlink escaping root")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardRefusesDotDotLexically(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	outside := t.TempDir()
	// <root>/../<base(outside)> resolves, lexically and via symlinks, to a
	// directory outside root.
	src := filepath.Join(root, "..", filepath.Base(outside))
	err := checkCreateBody(createBody(t, bindMount(src, "/data")), []string{root}, "")
	c.Require().Error(err, "dot-dot source")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardRefusesBinds(t *testing.T) {
	c := assert.NewCollecting(t)
	body, err := json.Marshal(map[string]any{
		"HostConfig": map[string]any{"Binds": []string{"/etc:/host-etc"}},
	})
	c.Require().NoError(err, "marshal")
	err = checkCreateBody(body, nil, "")
	c.Require().Error(err, "non-empty Binds")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardAllowsRelayDir(t *testing.T) {
	c := assert.NewCollecting(t)
	relay := t.TempDir()
	c.NoError(checkCreateBody(createBody(t, bindMount(relay, "/relay")), nil, relay), "bind exactly the relay dir")
}

// TestDockerGuardRefusesSocketUnderRoot: a bind of a unix socket still permits
// connect(), so a socket under a declared root (docker.sock, a controller or
// executor socket) is a full escape and must be refused by FILE TYPE, not path.
func TestDockerGuardRefusesSocketUnderRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	// A unix socket path must stay under the ~104-byte sun_path limit; on macOS
	// the default temp dir resolves through /private/var/folders/…, so use /tmp.
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = os.TempDir()
	}
	root, err := os.MkdirTemp(base, "rafiki-guard-")
	c.Require().NoError(err, "mkdirtemp")
	t.Cleanup(func() { os.RemoveAll(root) })
	sock := filepath.Join(root, "docker.sock")
	ln, err := net.Listen("unix", sock)
	c.Require().NoError(err, "listen on a unix socket under the root")
	t.Cleanup(func() { _ = ln.Close() })

	err = checkCreateBody(createBody(t, bindMount(sock, "/sock")), []string{root}, "")
	c.Require().Error(err, "a socket bind source must be refused")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "socket", "the refusal names the file type: %v", err)
}

// TestDockerGuardAllowsDirectoryAndRegularFileUnderRoot: the type check refuses
// only the irregular shapes; the ordinary directory and file binds still pass.
func TestDockerGuardAllowsDirectoryAndRegularFileUnderRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	root := t.TempDir()
	sub := filepath.Join(root, "data")
	c.Require().NoError(os.Mkdir(sub, 0o755), "mkdir")
	file := filepath.Join(root, "notes.txt")
	c.Require().NoError(os.WriteFile(file, []byte("hi"), 0o600), "write file")

	c.NoError(checkCreateBody(createBody(t, bindMount(sub, "/data")), []string{root}, ""),
		"a directory source is allowed")
	c.NoError(checkCreateBody(createBody(t, bindMount(file, "/notes")), []string{root}, ""),
		"a regular file source is allowed")
}

func TestDockerGuardAllowsVolumeAndTmpfs(t *testing.T) {
	c := assert.NewCollecting(t)
	body := createBody(t,
		map[string]string{"Type": "volume", "Source": "vol", "Target": "/v"},
		map[string]string{"Type": "tmpfs", "Source": "", "Target": "/t"},
	)
	c.NoError(checkCreateBody(body, nil, ""), "volume and tmpfs need no root")
}

func TestDockerGuardRefusesUnknownMountType(t *testing.T) {
	c := assert.NewCollecting(t)
	err := checkCreateBody(createBody(t, map[string]string{"Type": "npipe", "Source": "x", "Target": "/p"}), nil, "")
	c.Require().Error(err, "unknown mount type")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardRefusesWhenNoRootsDeclared(t *testing.T) {
	c := assert.NewCollecting(t)
	src := t.TempDir()
	err := checkCreateBody(createBody(t, bindMount(src, "/data")), nil, "")
	c.Require().Error(err, "no roots declared")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardRefusesMissingSource(t *testing.T) {
	c := assert.NewCollecting(t)
	src := filepath.Join(t.TempDir(), "does-not-exist")
	err := checkCreateBody(createBody(t, bindMount(src, "/data")), []string{t.TempDir()}, "")
	c.Require().Error(err, "missing source")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

// hostConfigBody builds a create body with exactly the given HostConfig.
func hostConfigBody(t *testing.T, hc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"HostConfig": hc})
	assert.NewAborting(t).NoError(err, "marshal body")
	return b
}

func TestDockerGuardRouteMatching(t *testing.T) {
	c := assert.NewCollecting(t)
	guard := []string{
		"/containers/create",
		"/containers/create?x=1",
		"/v1/containers/create",
		"/v1.43/containers/create",
		"/v1.43.0/containers/create",
		"/v1.43./containers/create",
		"/Containers/Create",
		"/V1.43/Containers/Create",
		// A fragment or percent-escape is a path the guard cannot reason
		// about (the engine routes on the decoded path), so it is guarded.
		"/containers/create#x",
		"/containers/creat%65",
		"/%63ontainers/create",
	}
	for _, p := range guard {
		c.True(isContainersCreate(p), "%q must be guarded", p)
	}
	skip := []string{
		"/containers/json",
		"/v1.43/containers/json",
		"/volumes/create",
		"/containers/create/x",
		"/_ping",
		"/version",
		// A pull carries its percent-escaped image ref in the QUERY; the `%`/`#`
		// fail-closed test applies to the path only, so this must not be guarded.
		"/images/create?fromImage=localhost%3A5000%2Fa%2Fb&tag=latest",
	}
	for _, p := range skip {
		c.False(isContainersCreate(p), "%q must not be guarded", p)
	}
}

func TestDockerGuardRefusesVolumeOptionsKey(t *testing.T) {
	c := assert.NewCollecting(t)
	// The mount allowlist is Type/Source/Target/ReadOnly only, so the
	// VolumeOptions KEY is refused whatever its value — non-null, or null.
	for _, body := range []string{
		`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"x","Target":"/v","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"type":"none","o":"bind","device":"/etc"}}}}]}}`,
		`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"vol","Target":"/v","VolumeOptions":null}]}}`,
	} {
		err := checkCreateBody([]byte(body), nil, "")
		c.Require().Error(err, "VolumeOptions must be refused: %s", body)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
	}
}

func TestDockerGuardRefusesDuplicateTopLevelKey(t *testing.T) {
	c := assert.NewCollecting(t)
	// The guard's map decode keeps only the LAST HostConfig (NetworkMode none),
	// but the engine MERGES repeated object keys, so it also sees the first
	// HostConfig's Binds. A duplicate must be refused, not reconciled.
	body := []byte(`{"HostConfig":{"Binds":["/:/h"]},"HostConfig":{"NetworkMode":"none"}}`)
	err := checkCreateBody(body, nil, "")
	c.Require().Error(err, "duplicate HostConfig must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestDockerGuardRefusesDuplicateInnerKey(t *testing.T) {
	c := assert.NewCollecting(t)
	// The guard's map decode keeps only the second Mounts array (no
	// VolumeOptions), but the engine keeps the first array's VolumeOptions. A
	// repeated Mounts key is refused.
	body := []byte(`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"x","Target":"/v","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"o":"bind","device":"/etc"}}}}],"Mounts":[{"Type":"volume","Source":"x","Target":"/v"}]}}`)
	err := checkCreateBody(body, nil, "")
	c.Require().Error(err, "duplicate Mounts key must be refused")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestDockerGuardAllowsBodyWithNoDuplicateKeys(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"volume","Source":"vol","Target":"/v"}],"NetworkMode":"none"}}`)
	c.NoError(checkCreateBody(body, nil, ""), "a duplicate-free body must pass")
}

func TestDockerGuardRefusesUnknownHostConfigKey(t *testing.T) {
	// Denylist misses; the allowlist refuses all of these by construction.
	unlisted := map[string]any{
		"DeviceCgroupRules": []string{"b *:* rwm"},
		"Sysctls":           map[string]string{"net.ipv4.ip_forward": "1"},
		"MaskedPaths":       []string{},
		"ReadonlyPaths":     []string{},
		"Runtime":           "runsc",
		"CgroupParent":      "x",
		"CgroupnsMode":      "host",
		"DeviceRequests":    []any{},
		"VolumeDriver":      "local",
		"Isolation":         "hyperv",
	}
	for name, value := range unlisted {
		t.Run(name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := checkCreateBody(hostConfigBody(t, map[string]any{name: value}), nil, "")
			c.Require().Error(err, "%s must be refused", name)
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		})
	}
}

func TestDockerGuardRefusesUnknownTopLevelKey(t *testing.T) {
	c := assert.NewCollecting(t)
	body := []byte(`{"Image":"x","NetworkingConfig":{"EndpointsConfig":{}},"HostConfig":{}}`)
	err := checkCreateBody(body, nil, "")
	c.Require().Error(err, "unknown top-level key must be refused")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

// TestDockerGuardAcceptsDaemonCreateBody pins that the daemon's own
// CreateBody output — the only body this guard should ever see — passes end to
// end, so the allowlist matches the emitted struct exactly.
func TestDockerGuardAcceptsDaemonCreateBody(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	c.NoError(os.Mkdir(filepath.Join(root, "ro"), 0o755), "mkdir bind source")
	relay := t.TempDir()

	r := sandbox.Resolved{SandboxSpec: protocol.SandboxSpec{
		Image:          "img:1",
		Network:        protocol.NetworkEgress,
		ReadOnlyRootfs: true,
		Workdir:        "/work",
		User:           "1000:1000",
		MemoryBytes:    64 << 20,
		CPUs:           1.5,
		PidsLimit:      128,
		Env:            map[string]string{"A": "1"},
		Labels:         map[string]string{"team": "x"},
		Mounts: []protocol.SandboxMount{
			{Target: "/ro", Kind: protocol.MountRO, HostPath: filepath.Join(root, "ro")},
			{Target: "/vol", Kind: protocol.MountRW, Volume: "data"},
			{Target: "/tmp", Kind: protocol.MountEphemeral},
		},
	}}
	in := sandbox.CreateInputs{
		SandboxID:      "sbx-1",
		Credential:     "cred",
		RelayHostDir:   relay,
		OwnerVolumeKey: "ownerk",
	}
	body, err := sandbox.CreateBody(r, in)
	c.NoError(err, "CreateBody")
	c.NoError(checkCreateBody(body, []string{root}, relay), "the daemon's own body must pass the guard")
}

func TestDockerGuardRefusesPrivilegeFields(t *testing.T) {
	cases := []struct {
		name string
		hc   map[string]any
	}{
		{"Privileged", map[string]any{"Privileged": true}},
		{"Devices", map[string]any{"Devices": []any{map[string]any{"PathOnHost": "/dev/sda"}}}},
		{"VolumesFrom", map[string]any{"VolumesFrom": []string{"other"}}},
		{"CapAdd", map[string]any{"CapAdd": []string{"SYS_ADMIN"}}},
		{"CapDrop", map[string]any{"CapDrop": []string{"ALL"}}},
		{"SecurityOpt", map[string]any{"SecurityOpt": []string{"seccomp=unconfined"}}},
		{"PidMode host", map[string]any{"PidMode": "host"}},
		{"IpcMode host", map[string]any{"IpcMode": "host"}},
		{"UTSMode host", map[string]any{"UTSMode": "host"}},
		{"UsernsMode host", map[string]any{"UsernsMode": "host"}},
		{"NetworkMode host", map[string]any{"NetworkMode": "host"}},
		{"NetworkMode container", map[string]any{"NetworkMode": "container:other"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			err := checkCreateBody(hostConfigBody(t, tc.hc), nil, "")
			c.Require().Error(err, "%s must be refused", tc.name)
			c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
		})
	}
}

func TestDockerGuardAllowsBridgeAndNoneNetwork(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, mode := range []string{"", "bridge", "none"} {
		c.NoError(checkCreateBody(hostConfigBody(t, map[string]any{"NetworkMode": mode}), nil, ""), "NetworkMode %q", mode)
	}
}
