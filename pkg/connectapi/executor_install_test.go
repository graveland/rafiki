// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeExecutorInstaller records what each handler handed it, so the tests pin
// both halves of the seam contract: the request reaches the installer
// unchanged, and the installer's answer (or error) rides out through the
// handler's mapping.
type fakeExecutorInstaller struct {
	row          *rafikiv1.ExecutorRow
	installErr   error
	uninstallErr error

	sawInstall *rafikiv1.InstallExecutorRequest
	sawRef     string
	calls      int
}

func (f *fakeExecutorInstaller) Install(_ context.Context, req *rafikiv1.InstallExecutorRequest) (*rafikiv1.ExecutorRow, error) {
	f.calls++
	f.sawInstall = req
	return f.row, f.installErr
}

func (f *fakeExecutorInstaller) Uninstall(_ context.Context, ref string) error {
	f.calls++
	f.sawRef = ref
	return f.uninstallErr
}

// TestInstallExecutorUnwiredIsUnavailable pins the seam's Unavailable path:
// with no installer attached both handlers refuse rather than nil-panicking —
// the reason SetExecutorInstaller refuses nil instead of storing &m.
func TestInstallExecutorUnwiredIsUnavailable(t *testing.T) {
	s := NewServer(nil)
	_, err := s.InstallExecutor(context.Background(),
		connect.NewRequest(&rafikiv1.InstallExecutorRequest{Target: "ublk-s2"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "install unwired: got %v", err)

	_, err = s.UninstallExecutor(context.Background(),
		connect.NewRequest(&rafikiv1.UninstallExecutorRequest{Executor: "ublk-s2"}))
	assert.NewAborting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "uninstall unwired: got %v", err)
}

// TestInstallExecutorPassesRequestThrough pins the field pass-through and the
// row mapping: the wire request reaches the installer unchanged and the row it
// returns becomes the response's executor, installed_via/installed_target
// included.
func TestInstallExecutorPassesRequestThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorInstaller{row: &rafikiv1.ExecutorRow{
		Id: "exec-1", Machine: "ublk-s2", InstalledVia: "greyshift", InstalledTarget: "root@10.0.0.5",
	}}
	s.SetExecutorInstaller(f)
	resp, err := s.InstallExecutor(context.Background(), connect.NewRequest(&rafikiv1.InstallExecutorRequest{
		Target: "root@10.0.0.5", Via: "greyshift", Name: "ublk-s2", Root: "/srv",
		Labels: map[string]string{"rack": "r1"}, Admits: "kind=fundi",
	}))
	c.Require().NoError(err)
	row := resp.Msg.GetExecutor()
	c.False(row.GetId() != "exec-1" || row.GetInstalledVia() != "greyshift" || row.GetInstalledTarget() != "root@10.0.0.5",
		"row = %+v, want the installer's row", row)
	got := f.sawInstall
	c.False(got.GetTarget() != "root@10.0.0.5" || got.GetVia() != "greyshift" ||
		got.GetName() != "ublk-s2" || got.GetRoot() != "/srv" ||
		got.GetLabels()["rack"] != "r1" || got.GetAdmits() != "kind=fundi",
		"installer saw %+v, want the request's fields", got)
}

// TestInstallExecutorPassesModeThroughConnect pins the transport half of the
// pass-through: the wire's `mode` reaches the installer unchanged, empty
// included. The handler is a seam, not a validator — the daemon decides which
// modes exist and what each means for a target, so a check here would be a
// second rule with a second message.
func TestInstallExecutorPassesModeThroughConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, mode := range []string{"", "auto", "direct", "tunnel", "sideways"} {
		s := NewServer(nil)
		f := &fakeExecutorInstaller{row: &rafikiv1.ExecutorRow{Id: "exec-1"}}
		s.SetExecutorInstaller(f)
		_, err := s.InstallExecutor(context.Background(), connect.NewRequest(&rafikiv1.InstallExecutorRequest{
			Target: "root@10.0.0.5", Mode: mode,
		}))
		c.Require().NoError(err, "InstallExecutor(mode=%q)", mode)
		c.Require().NotNil(f.sawInstall, "the handler must reach the installer (mode=%q)", mode)
		c.Eq(mode, f.sawInstall.GetMode(), "mode %q must ride through unchanged", mode)
	}
}

// TestUninstallExecutorPassesRef pins that the handler hands the installer the
// caller's ref verbatim and answers an empty response.
func TestUninstallExecutorPassesRef(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorInstaller{}
	s.SetExecutorInstaller(f)
	_, err := s.UninstallExecutor(context.Background(),
		connect.NewRequest(&rafikiv1.UninstallExecutorRequest{Executor: "ublk-s2"}))
	c.Require().NoError(err)
	c.Eq("ublk-s2", f.sawRef, "installer saw ref")
}

// TestSetExecutorInstallerNilIsRefused pins the setter rule: Set(nil) is
// refused rather than stored, so the Unavailable path above stays reachable
// and a later handler call cannot nil-panic.
func TestSetExecutorInstallerNilIsRefused(t *testing.T) {
	s := &Server{}
	s.SetExecutorInstaller(nil)
	assert.NewAborting(t).Nil(s.execInstaller.Load(), "SetExecutorInstaller(nil) stored a pointer")
}

// TestInstallExecutorErrorKeepsItsConnectCode is the error-mapping pin: a
// refusal the installer classified as a protocol code (a child outside the
// lineage, a target that already has an executor) reaches the caller as that
// Connect code rather than collapsing into internal.
func TestInstallExecutorErrorKeepsItsConnectCode(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(nil)
	f := &fakeExecutorInstaller{installErr: &ControllerError{
		Code: protocol.ErrPermissionDenied, Message: "refused",
	}}
	s.SetExecutorInstaller(f)
	_, err := s.InstallExecutor(context.Background(),
		connect.NewRequest(&rafikiv1.InstallExecutorRequest{Target: "ublk-s2"}))
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "got %v", err)
}
