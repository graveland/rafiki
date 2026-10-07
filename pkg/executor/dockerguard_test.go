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
