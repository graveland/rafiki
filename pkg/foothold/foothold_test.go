// SPDX-License-Identifier: Apache-2.0

package foothold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/sandbox"
)

// fakeEngine is a recording fake implementing Engine. It models one container
// slot: present/running/labels, mutated by create/start/stop/remove so a
// converged foothold is observable.
type fakeEngine struct {
	mu sync.Mutex

	imageExists bool
	imageID     string
	pullErr     error

	present bool
	running bool
	labels  map[string]string

	calls         []string
	createCount   int
	createName    string
	createBody    []byte
	removeForce   bool
	removeVolumes bool
}

func (e *fakeEngine) rec(s string) { e.calls = append(e.calls, s) }

func (e *fakeEngine) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *fakeEngine) resetCalls() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = nil
}

func (e *fakeEngine) setImageID(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.imageID = id
}

func (e *fakeEngine) setRunning(r bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = r
}

func (e *fakeEngine) ImageExists(_ context.Context, ref string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec("ImageExists " + ref)
	return e.imageExists, nil
}

func (e *fakeEngine) PullImage(_ context.Context, ref string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec("PullImage " + ref)
	return e.pullErr
}

func (e *fakeEngine) ImageID(_ context.Context, ref string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec("ImageID " + ref)
	return e.imageID, nil
}

func (e *fakeEngine) InspectContainer(_ context.Context, id string) (sandbox.ContainerInfo, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec("InspectContainer " + id)
	if !e.present {
		return sandbox.ContainerInfo{}, false, nil
	}
	return sandbox.ContainerInfo{ID: "cid", Running: e.running, Labels: e.labels}, true, nil
}

func (e *fakeEngine) CreateContainer(_ context.Context, name string, body []byte) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec("CreateContainer " + name)
	e.createCount++
	e.createName = name
	e.createBody = body
	var b createBody
	_ = json.Unmarshal(body, &b)
	e.present = true
	e.running = false
	e.labels = b.Labels
	return "cid", nil
}

func (e *fakeEngine) StartContainer(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec("StartContainer " + id)
	e.running = true
	return nil
}

func (e *fakeEngine) StopContainer(_ context.Context, id string, timeoutSeconds int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec(fmt.Sprintf("StopContainer %s t=%d", id, timeoutSeconds))
	e.running = false
	return nil
}

func (e *fakeEngine) RemoveContainer(_ context.Context, id string, force, volumes bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec(fmt.Sprintf("RemoveContainer %s force=%v volumes=%v", id, force, volumes))
	e.removeForce, e.removeVolumes = force, volumes
	e.present = false
	e.labels = nil
	return nil
}

// mustSpec returns the spec hash for the given inputs.
func mustSpec(t *testing.T, key, image string, port int, imageID string) string {
	t.Helper()
	spec, _, err := New(&fakeEngine{}, key, image, port).desired(imageID)
	if err != nil {
		t.Fatalf("desired: %v", err)
	}
	return spec
}

func TestFootholdEnsureCreatesWhenMissing(t *testing.T) {
	e := &fakeEngine{imageExists: true, imageID: "sha256:aaa"}
	f := New(e, "host", "sandbox:latest", 5000)
	c := assert.NewAborting(t)
	c.NoError(f.Ensure(context.Background()), "Ensure")

	c.Eq(1, e.createCount, "one create")
	c.Eq("rafiki-foothold-host", e.createName, "container name")
	c.EqDeep([]string{
		"ImageExists sandbox:latest",
		"ImageID sandbox:latest",
		"InspectContainer rafiki-foothold-host",
		"CreateContainer rafiki-foothold-host",
		"StartContainer cid",
	}, e.snapshot(), "call order")

	var b createBody
	c.NoError(json.Unmarshal(e.createBody, &b), "create body decodes")
	c.Eq("sandbox:latest", b.Image, "image")
	c.EqDeep([]string{"rafiki"}, b.Entrypoint, "entrypoint")
	c.EqDeep([]string{"executor", "bridge", "--listen", "/relay", "--dial", "host.docker.internal:5000"}, b.Cmd, "cmd")
	c.Eq("0", b.User, "runs as root so it can create the socket in the fresh root-owned volume")
	c.Eq("host", b.Labels[LabelFoothold], "foothold label")
	c.NotEmpty(b.Labels[LabelSpec], "spec label present in the sent body")
	c.EqDeep([]mount{{Type: "volume", Source: "rafiki.relay.host", Target: "/relay"}}, b.HostConfig.Mounts, "volume mount")
	c.Eq("bridge", b.HostConfig.NetworkMode, "network mode")
	c.True(b.HostConfig.ReadonlyRootfs, "readonly rootfs")
	c.EqDeep([]string{"ALL"}, b.HostConfig.CapDrop, "cap drop")
	c.EqDeep([]string{"no-new-privileges"}, b.HostConfig.SecurityOpt, "security opt")
	c.Eq("unless-stopped", b.HostConfig.RestartPolicy.Name, "restart policy")
}

func TestFootholdEnsureNoopWhenSpecMatches(t *testing.T) {
	e := &fakeEngine{imageExists: true, imageID: "sha256:aaa"}
	f := New(e, "host", "sandbox:latest", 5000)
	c := assert.NewAborting(t)
	c.NoError(f.Ensure(context.Background()), "first Ensure")
	c.Eq(1, e.createCount, "created once")

	e.resetCalls()
	c.NoError(f.Ensure(context.Background()), "second Ensure")
	c.Eq(1, e.createCount, "no second create")
	c.EqDeep([]string{
		"ImageExists sandbox:latest",
		"ImageID sandbox:latest",
		"InspectContainer rafiki-foothold-host",
	}, e.snapshot(), "a matching running foothold is left alone: no create/stop/remove/start")
}

func TestFootholdEnsureStartsStoppedMatching(t *testing.T) {
	e := &fakeEngine{imageExists: true, imageID: "sha256:aaa"}
	f := New(e, "host", "sandbox:latest", 5000)
	c := assert.NewAborting(t)
	c.NoError(f.Ensure(context.Background()), "first Ensure")

	e.setRunning(false)
	e.resetCalls()
	c.NoError(f.Ensure(context.Background()), "second Ensure")
	c.Eq(1, e.createCount, "no re-create of a stopped-but-matching foothold")
	c.EqDeep([]string{
		"ImageExists sandbox:latest",
		"ImageID sandbox:latest",
		"InspectContainer rafiki-foothold-host",
		"StartContainer cid",
	}, e.snapshot(), "start the stopped matching container")
}

func TestFootholdEnsureReplacesOnImageIDChange(t *testing.T) {
	e := &fakeEngine{imageExists: true, imageID: "sha256:aaa"}
	f := New(e, "host", "sandbox:latest", 5000)
	c := assert.NewAborting(t)
	c.NoError(f.Ensure(context.Background()), "first Ensure")

	e.setImageID("sha256:bbb")
	e.resetCalls()
	c.NoError(f.Ensure(context.Background()), "second Ensure")
	c.EqDeep([]string{
		"ImageExists sandbox:latest",
		"ImageID sandbox:latest",
		"InspectContainer rafiki-foothold-host",
		"StopContainer cid t=5",
		"RemoveContainer cid force=true volumes=false",
		"CreateContainer rafiki-foothold-host",
		"StartContainer cid",
	}, e.snapshot(), "stale foothold replaced in order")
	c.Eq(2, e.createCount, "recreated")
	c.False(e.removeVolumes, "the named volume is kept (volumes=false)")
}

func TestFootholdEnsureReplacesOnPortChange(t *testing.T) {
	e := &fakeEngine{imageExists: true, imageID: "sha256:aaa"}
	c := assert.NewAborting(t)
	c.NoError(New(e, "host", "sandbox:latest", 5000).Ensure(context.Background()), "first Ensure")

	e.resetCalls()
	c.NoError(New(e, "host", "sandbox:latest", 6000).Ensure(context.Background()), "new port")
	c.Eq(2, e.createCount, "a new relay port invalidates the spec")
	c.EqDeep([]string{
		"ImageExists sandbox:latest",
		"ImageID sandbox:latest",
		"InspectContainer rafiki-foothold-host",
		"StopContainer cid t=5",
		"RemoveContainer cid force=true volumes=false",
		"CreateContainer rafiki-foothold-host",
		"StartContainer cid",
	}, e.snapshot(), "replaced in order")

	var b createBody
	c.NoError(json.Unmarshal(e.createBody, &b), "create body decodes")
	c.Contains(b.Cmd, "host.docker.internal:6000", "new dial target")
}

func TestFootholdSpecHashIsDeterministic(t *testing.T) {
	c := assert.NewAborting(t)
	_, body1, err1 := New(&fakeEngine{}, "host", "img:1", 1234).desired("sha256:abc")
	_, body2, err2 := New(&fakeEngine{}, "host", "img:1", 1234).desired("sha256:abc")
	c.NoError(err1, "desired")
	c.NoError(err2, "desired")
	spec1 := mustSpec(t, "host", "img:1", 1234, "sha256:abc")
	spec2 := mustSpec(t, "host", "img:1", 1234, "sha256:abc")
	c.Eq(spec1, spec2, "same inputs, identical spec label")
	c.NotEmpty(spec1, "spec is non-empty")
	c.EqDeep(body1, body2, "identical bodies")
}

func TestFootholdSpecChangesWithEachInput(t *testing.T) {
	base := mustSpec(t, "host", "img:1", 1000, "sha256:aaa")
	tests := []struct {
		name    string
		image   string
		port    int
		imageID string
	}{
		{"image-id", "img:1", 1000, "sha256:bbb"},
		{"port", "img:1", 1001, "sha256:aaa"},
		{"image-ref", "img:2", 1000, "sha256:aaa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			got := mustSpec(t, "host", tt.image, tt.port, tt.imageID)
			c.NotEq(base, got, "%s change alters the spec", tt.name)
		})
	}
}

func TestFootholdSpecExcludesSpecLabel(t *testing.T) {
	f := New(&fakeEngine{}, "host", "img:1", 1000)
	c := assert.NewAborting(t)

	spec, sent, err := f.desired("sha256:abc")
	c.NoError(err, "desired")

	var b createBody
	c.NoError(json.Unmarshal(sent, &b), "sent body decodes")
	c.Eq(spec, b.Labels[LabelSpec], "the sent body carries the spec label")

	// The hash is recomputable from the body WITHOUT the spec label.
	delete(b.Labels, LabelSpec)
	base, err := json.Marshal(b)
	c.NoError(err, "re-marshal without the spec label")
	sum := sha256.Sum256(append(append(append([]byte(nil), base...), 0), []byte("sha256:abc")...))
	c.Eq(hex.EncodeToString(sum[:]), spec, "hash covers the body without the spec label")

	// Hashing the sent body (spec label included) would be different.
	sentSum := sha256.Sum256(append(append(append([]byte(nil), sent...), 0), []byte("sha256:abc")...))
	c.NotEq(hex.EncodeToString(sentSum[:]), spec, "the spec label itself is not in the hash input")
}

func TestFootholdPullsWhenImageMissing(t *testing.T) {
	e := &fakeEngine{imageExists: false, imageID: "sha256:aaa"}
	f := New(e, "host", "img:1", 1000)
	c := assert.NewAborting(t)
	c.NoError(f.Ensure(context.Background()), "Ensure")

	calls := e.snapshot()
	c.EqDeep([]string{
		"ImageExists img:1",
		"PullImage img:1",
		"ImageID img:1",
		"InspectContainer rafiki-foothold-host",
		"CreateContainer rafiki-foothold-host",
		"StartContainer cid",
	}, calls, "pulled before resolving the id")
}

func TestFootholdEnsureIsSerialised(t *testing.T) {
	e := &fakeEngine{imageExists: true, imageID: "sha256:aaa"}
	f := New(e, "host", "img:1", 1000)

	const n = 8
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = f.Ensure(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	c := assert.NewAborting(t)
	for i, err := range errs {
		c.NoError(err, "concurrent Ensure %d", i)
	}
	// The stale→fresh transition is guarded by the mutex: without it several
	// callers would inspect an absent container and each create one.
	c.Eq(1, e.createCount, "exactly one create under concurrency")
}

func TestFootholdKeySanitises(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("greyshiftlocal", Key("Grey-Shift.local"), "lowercase, non-alnum stripped")
	c.Eq("host", Key(""), "empty becomes host")
	c.Eq("host", Key("---"), "all-unsafe becomes host")
	c.Eq("rafiki.relay.host", VolumeName("host"), "volume name")
	c.Eq("rafiki-foothold-host", ContainerName("host"), "container name")
}
