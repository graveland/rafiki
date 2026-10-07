// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeSandboxManager is an in-memory SandboxManager. It records calls so a test
// can assert what the daemon adapter would have been asked to do. It enforces
// nothing — every authority rule lives on the daemon side — and, like every
// binding of this interface, it takes no caller identity in any method.
type fakeSandboxManager struct {
	created    []protocol.SandboxSpec
	createInfo protocol.SandboxInfo
	createErr  error

	list    []protocol.SandboxInfo
	listErr error

	removed   []string
	removeErr error
}

func (f *fakeSandboxManager) Create(_ context.Context, spec protocol.SandboxSpec) (protocol.SandboxInfo, error) {
	if f.createErr != nil {
		return protocol.SandboxInfo{}, f.createErr
	}
	f.created = append(f.created, spec)
	info := f.createInfo
	if info.ID == "" {
		info = protocol.SandboxInfo{ID: "sbx_1", Name: spec.Name, State: "ready"}
	}
	return info, nil
}

func (f *fakeSandboxManager) List(context.Context) ([]protocol.SandboxInfo, error) {
	return f.list, f.listErr
}

func (f *fakeSandboxManager) Remove(_ context.Context, ref string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, ref)
	return nil
}

// sandboxSchemaJSON decodes a blueprint's input schema for structural checks.
func sandboxSchemaJSON(t *testing.T, bp Tool) map[string]any {
	t.Helper()
	var got map[string]any
	assert.NewAborting(t).NoError(json.Unmarshal(bp.InputSchema().JSON(), &got), "decode %s schema", bp.Name())
	return got
}

// TestSandboxToolSchemasPinEnumsAndRequired is the schema test that would fail
// if a mount's kind stopped being required (the brief's no-unset-means-default
// rule), if an enum gained or lost a value, or if the required lists drifted.
func TestSandboxToolSchemasPinEnumsAndRequired(t *testing.T) {
	c := assert.NewCollecting(t)

	create := sandboxSchemaJSON(t, &SandboxCreateBlueprint{})
	c.Contains(create["required"].([]any), "name", "sandbox_create must require name")
	createProps := create["properties"].(map[string]any)

	// mounts: an array whose items require target and kind, with kind an enum.
	mounts := createProps["mounts"].(map[string]any)
	items := mounts["items"].(map[string]any)
	itemReq := toStrings(items["required"].([]any))
	c.EqDiff([]string{"target", "kind"}, itemReq, "mounts items required")
	itemProps := items["properties"].(map[string]any)
	kind := itemProps["kind"].(map[string]any)
	c.EqDiff([]string{"ro", "rw", "ephemeral"}, toStrings(kind["enum"].([]any)), "mount kind enum")

	// network enum.
	network := createProps["network"].(map[string]any)
	c.EqDiff([]string{"egress", "none"}, toStrings(network["enum"].([]any)), "network enum")

	// remove requires ref.
	remove := sandboxSchemaJSON(t, &SandboxRemoveBlueprint{})
	c.EqDiff([]string{"ref"}, toStrings(remove["required"].([]any)), "sandbox_remove required")

	// list takes no arguments.
	list := sandboxSchemaJSON(t, &SandboxListBlueprint{})
	c.Eq("object", list["type"], "sandbox_list type")
	_, hasProps := list["properties"]
	c.False(hasProps, "sandbox_list must take no arguments, got properties %v", list["properties"])
}

func toStrings(in []any) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, v.(string))
	}
	return out
}

// TestSandboxToolsDeclineWithoutManager pins the nil-means-decline rule: with
// no sandbox manager (a DB-less daemon) none of the three materializes.
func TestSandboxToolsDeclineWithoutManager(t *testing.T) {
	c := assert.NewAborting(t)
	for _, bp := range []Tool{&SandboxCreateBlueprint{}, &SandboxListBlueprint{}, &SandboxRemoveBlueprint{}} {
		tool, err := bp.(Materializer).Materialize(ToolOpts{})
		c.NoError(err, "%s materialize", bp.Name())
		c.Nil(tool, "%s materialized with a nil manager", bp.Name())
	}
}

// TestSandboxToolCreateCallsManagerAndPropagatesError pins that sandbox_create
// maps the flat input onto the domain spec verbatim and hands it to the bound
// manager, and that a manager failure is returned as an ERROR — never the
// error's text as a successful result.
func TestSandboxToolCreateCallsManagerAndPropagatesError(t *testing.T) {
	c := assert.NewAborting(t)
	mgr := &fakeSandboxManager{}
	tool, err := (&SandboxCreateBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr})
	c.NoError(err, "materialize")

	in := `{"name":"build-box","image":"rafiki/sandbox:1","launcher":"greyshift",` +
		`"workdir":"/work","network":"none","ttl":"72h","memory_bytes":2147483648,` +
		`"cpus":2,"pids_limit":512,"read_only_rootfs":true,` +
		`"mounts":[{"target":"/work","kind":"ro","host_path":"/srv/repos/app"},` +
		`{"target":"/scratch","kind":"rw"},{"target":"/tmp","kind":"ephemeral"}]}`
	res, err := tool.Execute(context.Background(), ToolInput(in))
	c.NoError(err, "sandbox_create")
	c.StrContains(res.Text, "sbx_1", "result should name the created sandbox")

	c.Require().Len(mgr.created, 1, "want 1 create, got %d", len(mgr.created))
	spec := mgr.created[0]
	c.Eq("build-box", spec.Name, "Name")
	c.Eq("rafiki/sandbox:1", spec.Image, "Image")
	c.Eq("greyshift", spec.Launcher, "Launcher")
	c.Eq("/work", spec.Workdir, "Workdir")
	c.Eq(protocol.NetworkNone, spec.Network, "Network")
	c.Eq(72*time.Hour, spec.TTL, "TTL")
	c.Eq(int64(2147483648), spec.MemoryBytes, "MemoryBytes")
	c.Eq(2.0, spec.CPUs, "CPUs")
	c.Eq(int64(512), spec.PidsLimit, "PidsLimit")
	c.True(spec.ReadOnlyRootfs, "ReadOnlyRootfs")
	c.Len(spec.Mounts, 3, "Mounts")
	c.EqDeep(protocol.SandboxMount{Target: "/work", Kind: protocol.MountRO, HostPath: "/srv/repos/app"}, spec.Mounts[0], "mount 0")
	c.EqDeep(protocol.SandboxMount{Target: "/scratch", Kind: protocol.MountRW}, spec.Mounts[1], "mount 1")
	c.EqDeep(protocol.SandboxMount{Target: "/tmp", Kind: protocol.MountEphemeral}, spec.Mounts[2], "mount 2")

	// A manager failure is an ERROR, not a successful result carrying the text.
	mgr2 := &fakeSandboxManager{createErr: errors.New("no docker launcher is in scope")}
	tool2, err := (&SandboxCreateBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr2})
	c.NoError(err, "materialize")
	if _, err := tool2.Execute(context.Background(), ToolInput(`{"name":"x"}`)); err == nil {
		t.Fatal("sandbox_create swallowed the manager's error as a success")
	} else if !strings.Contains(err.Error(), "no docker launcher is in scope") {
		t.Fatalf("error = %v, want it to wrap the manager's error", err)
	}
}

// TestSandboxToolCreateRejectsBadInput pins the tool-side validation: an
// omitted mount kind is an error (not a silent rw), a bad network or ttl is an
// error, and a missing name is an error — none reaches the manager.
func TestSandboxToolCreateRejectsBadInput(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSandboxManager{}
	tool, err := (&SandboxCreateBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr})
	c.Require().NoError(err, "materialize")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"missing name", `{"image":"i"}`, "name is required"},
		{"omitted mount kind", `{"name":"x","mounts":[{"target":"/d"}]}`, "kind is required"},
		{"unknown mount kind", `{"name":"x","mounts":[{"target":"/d","kind":"rwx"}]}`, "kind must be ro, rw or ephemeral"},
		{"bad network", `{"name":"x","network":"wan"}`, "network must be"},
		{"bad ttl", `{"name":"x","ttl":"soon"}`, "ttl"},
	}
	for _, tc := range cases {
		_, err := tool.Execute(context.Background(), ToolInput(tc.in))
		if err == nil {
			t.Errorf("%s: want an error, got nil", tc.name)
			continue
		}
		c.StrContains(err.Error(), tc.want, "%s: error", tc.name)
	}
	c.Empty(mgr.created, "a rejected create must not reach the manager, got %d", len(mgr.created))
}

// TestSandboxToolListAndRemoveCallManager pins that list and remove delegate to
// the bound manager and render/return the result, and that a failure is an
// error.
func TestSandboxToolListAndRemoveCallManager(t *testing.T) {
	c := assert.NewAborting(t)
	expires := time.Now().Add(time.Hour)
	mgr := &fakeSandboxManager{list: []protocol.SandboxInfo{{
		ID: "sbx_1", Name: "build-box", State: "ready", Network: protocol.NetworkEgress,
		Connected: true, ExpiresAt: &expires,
	}}}

	listTool, err := (&SandboxListBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr})
	c.NoError(err, "materialize list")
	res, err := listTool.Execute(context.Background(), ToolInput(`{}`))
	c.NoError(err, "sandbox_list")
	c.StrContains(res.Text, "build-box", "list should name the sandbox")
	c.StrContains(res.Text, "egress", "list should show the network")

	removeTool, err := (&SandboxRemoveBlueprint{}).Materialize(ToolOpts{Sandboxes: mgr})
	c.NoError(err, "materialize remove")
	if _, err := removeTool.Execute(context.Background(), ToolInput(`{"ref":"build-box"}`)); err != nil {
		t.Fatalf("sandbox_remove: %v", err)
	}
	c.EqDiff([]string{"build-box"}, mgr.removed, "remove ref")

	// Empty ref is refused at the tool.
	if _, err := removeTool.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("sandbox_remove accepted an empty ref")
	}

	// A manager failure is an error, not a success.
	badList, _ := (&SandboxListBlueprint{}).Materialize(ToolOpts{Sandboxes: &fakeSandboxManager{listErr: errors.New("boom")}})
	if _, err := badList.Execute(context.Background(), ToolInput(`{}`)); err == nil {
		t.Fatal("sandbox_list swallowed the manager's error")
	}
	badRemove, _ := (&SandboxRemoveBlueprint{}).Materialize(ToolOpts{Sandboxes: &fakeSandboxManager{removeErr: errors.New("boom")}})
	if _, err := badRemove.Execute(context.Background(), ToolInput(`{"ref":"x"}`)); err == nil {
		t.Fatal("sandbox_remove swallowed the manager's error")
	}
}

// TestSandboxToolsAreRegisteredAndDaemonTier pins that all three blueprints
// self-register (so the fundi runtime's MaterializeAll picks them up) and are
// classified TierDaemon.
func TestSandboxToolsAreRegisteredAndDaemonTier(t *testing.T) {
	c := assert.NewCollecting(t)
	registered := map[string]bool{}
	for _, bp := range DefaultBlueprint.All() {
		registered[bp.Name()] = true
	}
	for _, name := range []string{"sandbox_create", "sandbox_list", "sandbox_remove"} {
		c.True(registered[name], "%s is not registered in DefaultBlueprint", name)
		tier, ok := TierOf(name)
		c.True(ok, "%s has no tier", name)
		c.Eq(TierDaemon, tier, "%s tier", name)
	}
}
