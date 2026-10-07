// SPDX-License-Identifier: Apache-2.0

// Package foothold converges the per-launcher foothold container: a small
// bridge, created by the launcher executor inside the docker host's kernel,
// that relays a unix socket in a named volume to the launcher's loopback TCP
// relay. A sandbox on a docker host in another kernel (a macOS VM, a remote
// context) cannot connect to a host-bind unix socket, so the foothold holds
// the socket in a volume the sandbox mounts read-only and splices it to the
// launcher over TCP.
//
// The package is pgx-free (stdlib plus pkg/sandbox) so the executor plane never
// links postgres.
package foothold

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/sandbox"
)

const (
	// LabelFoothold marks a foothold container; the value is its key.
	LabelFoothold = "rafiki/foothold"
	// LabelSpec records the hash of the create body the container was built
	// from, so a drift in what we ask docker to run is detectable.
	LabelSpec = "rafiki/foothold-spec"
	// ContainerRelayDir is where the relay volume is mounted inside the
	// foothold container; the bridge listens on a socket in this directory.
	ContainerRelayDir = "/relay"
	// ContainerPrefix prefixes every foothold container name.
	ContainerPrefix = "rafiki-foothold-"
	// VolumePrefix prefixes the relay volume name. The '.' after "rafiki"
	// can never collide with an owner-scoped sandbox volume.
	VolumePrefix = "rafiki.relay."
)

// Key reduces a host name to [a-z0-9]; an empty result becomes "host". One key
// is one docker host, so two launchers on one machine share (and fight over) a
// foothold — an operator error, not something to encode here.
func Key(hostname string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(hostname) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "host"
	}
	return b.String()
}

// VolumeName is the relay volume for key.
func VolumeName(key string) string { return VolumePrefix + key }

// ContainerName is the foothold container name for key.
func ContainerName(key string) string { return ContainerPrefix + key }

// Engine is the subset of *sandbox.Engine the foothold uses, so tests can fake
// it. *sandbox.Engine satisfies it.
type Engine interface {
	ImageExists(ctx context.Context, ref string) (bool, error)
	PullImage(ctx context.Context, ref string) error
	ImageID(ctx context.Context, ref string) (string, error)
	InspectContainer(ctx context.Context, id string) (sandbox.ContainerInfo, bool, error)
	CreateContainer(ctx context.Context, name string, body []byte) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeoutSeconds int) error
	RemoveContainer(ctx context.Context, id string, force, volumes bool) error
}

// Foothold converges one foothold container from a desired spec. Its fields are
// fixed at construction; Ensure is the only method and is safe for concurrent
// callers.
type Foothold struct {
	engine Engine
	key    string
	image  string
	port   int

	mu sync.Mutex
}

// New builds a Foothold that converges the container for key, running image,
// bridging to the launcher relay at 127.0.0.1:port. It never panics.
func New(engine Engine, key, image string, port int) *Foothold {
	return &Foothold{engine: engine, key: key, image: image, port: port}
}

// Ensure converges the foothold container on its desired spec. Safe for
// concurrent callers; serialised by an internal mutex.
func (f *Foothold) Ensure(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	exists, err := f.engine.ImageExists(ctx, f.image)
	if err != nil {
		return fmt.Errorf("foothold: checking image %s: %w", f.image, err)
	}
	if !exists {
		if err := f.engine.PullImage(ctx, f.image); err != nil {
			return fmt.Errorf("foothold: pulling image %s: %w", f.image, err)
		}
	}
	imageID, err := f.engine.ImageID(ctx, f.image)
	if err != nil {
		return fmt.Errorf("foothold: resolving image %s: %w", f.image, err)
	}

	spec, body, err := f.desired(imageID)
	if err != nil {
		return fmt.Errorf("foothold: building spec: %w", err)
	}

	name := ContainerName(f.key)
	info, ok, err := f.engine.InspectContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("foothold: inspecting container %s: %w", name, err)
	}
	if ok && info.Labels[LabelSpec] == spec {
		if info.Running {
			return nil
		}
		if err := f.engine.StartContainer(ctx, info.ID); err != nil {
			return fmt.Errorf("foothold: starting container %s: %w", name, err)
		}
		return nil
	}
	if ok {
		if err := f.engine.StopContainer(ctx, info.ID, 5); err != nil {
			return fmt.Errorf("foothold: stopping stale container %s: %w", name, err)
		}
		if err := f.engine.RemoveContainer(ctx, info.ID, true, false); err != nil {
			return fmt.Errorf("foothold: removing stale container %s: %w", name, err)
		}
	}
	id, err := f.engine.CreateContainer(ctx, name, body)
	if err != nil {
		return fmt.Errorf("foothold: creating container %s: %w", name, err)
	}
	if err := f.engine.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("foothold: starting container %s: %w", name, err)
	}
	return nil
}

// desired builds the create body actually sent and the spec hash its
// LabelSpec carries. The hash covers the body WITHOUT the spec label plus the
// resolved image id, so the container's label is a pure function of what we ask
// docker to run — never of the spec label itself.
func (f *Foothold) desired(imageID string) (string, []byte, error) {
	b := createBody{
		Image:      f.image,
		Entrypoint: []string{"rafiki"},
		Cmd:        []string{"executor", "bridge", "--listen", ContainerRelayDir, "--dial", f.dialTarget()},
		User:       "0",
		Labels:     map[string]string{LabelFoothold: f.key},
		HostConfig: hostConfig{
			Mounts: []mount{{
				Type:   "volume",
				Source: VolumeName(f.key),
				Target: ContainerRelayDir,
			}},
			NetworkMode:    "bridge",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			RestartPolicy:  restartPolicy{Name: "unless-stopped"},
		},
	}

	base, err := json.Marshal(b)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.New()
	sum.Write(base)
	sum.Write([]byte{0})
	sum.Write([]byte(imageID))
	spec := hex.EncodeToString(sum.Sum(nil))

	// The spec label rides the sent body but not the hash input.
	b.Labels[LabelSpec] = spec
	sent, err := json.Marshal(b)
	if err != nil {
		return "", nil, err
	}
	return spec, sent, nil
}

// dialTarget is where the foothold's bridge dials the launcher relay: the
// docker host's loopback, reached from inside a container by name.
func (f *Foothold) dialTarget() string {
	return fmt.Sprintf("host.docker.internal:%d", f.port)
}

// createBody is the desired Docker create body. It is private to this package
// and marshalled from structs (and a sorted map) so the bytes — and therefore
// the spec hash — are deterministic.
type createBody struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	User       string            `json:"User"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	Mounts         []mount       `json:"Mounts"`
	NetworkMode    string        `json:"NetworkMode"`
	ReadonlyRootfs bool          `json:"ReadonlyRootfs"`
	CapDrop        []string      `json:"CapDrop"`
	SecurityOpt    []string      `json:"SecurityOpt"`
	RestartPolicy  restartPolicy `json:"RestartPolicy"`
}

type mount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	Target string `json:"Target"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}
