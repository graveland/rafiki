// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

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
	}
	for _, p := range skip {
		c.False(isContainersCreate(p), "%q must not be guarded", p)
	}
}

func TestDockerGuardRefusesVolumeOptions(t *testing.T) {
	c := assert.NewCollecting(t)
	// A "volume" whose local driver binds /etc — the exact bypass body.
	body := []byte(`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"x","Target":"/v","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"type":"none","o":"bind","device":"/etc"}}}}]}}`)
	err := checkCreateBody(body, []string{t.TempDir()}, "")
	c.Require().Error(err, "volume with VolumeOptions must be refused")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "code")
}

func TestDockerGuardAllowsEmptyVolumeOptions(t *testing.T) {
	c := assert.NewCollecting(t)
	// Explicit null VolumeOptions is still "no options" and passes.
	body := []byte(`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"vol","Target":"/v","VolumeOptions":null}]}}`)
	c.NoError(checkCreateBody(body, nil, ""), "null VolumeOptions")
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
