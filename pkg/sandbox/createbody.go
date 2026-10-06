// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// CreateInputs is the daemon-supplied context for one container create. Every
// value here is written by the daemon from something it verified (or from the
// launcher's own reported config); nothing the container reports feeds back.
type CreateInputs struct {
	SandboxID      string
	OwnerChild     string // "" for a named sandbox
	Credential     string // the durable executor credential
	RelayHostDir   string // the launcher's reported --relay-dir
	OwnerVolumeKey string // short stable owner key used to prefix named volumes
}

// The create body's fixed pieces. The entrypoint runs `rafiki executor serve`
// bound to the relay socket the launcher mounts at ContainerRelayDir.
const relaySocketPath = ContainerRelayDir + "/daemon.sock"

// createBody is the JSON sent to POST /containers/create. Every field is
// spelled out so the emitted set is exactly the fields below: no Privileged,
// CapAdd, Devices, PidMode, SecurityOpt or Binds is ever expressible.
type createBody struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	User       string            `json:"User,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	Mounts         []mount       `json:"Mounts,omitempty"`
	NetworkMode    string        `json:"NetworkMode"`
	ReadonlyRootfs bool          `json:"ReadonlyRootfs,omitempty"`
	Memory         int64         `json:"Memory,omitempty"`
	NanoCpus       int64         `json:"NanoCpus,omitempty"`
	PidsLimit      int64         `json:"PidsLimit,omitempty"`
	RestartPolicy  restartPolicy `json:"RestartPolicy"`
}

type mount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source,omitempty"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}

// CreateBody renders the Docker create body for a validated sandbox. r must be
// the output of Validate, so its Image, Network and limits are already
// resolved. in carries the daemon-owned identity: the sandbox id, the owning
// child (empty for a named sandbox), the executor credential, the launcher's
// relay directory, and the owner key named volumes are prefixed with.
//
// A mount naming a volume requires an OwnerVolumeKey; without one the volume's
// name would not be owner-scoped, so the request is refused.
func CreateBody(r Resolved, in CreateInputs) ([]byte, error) {
	body := createBody{
		Image:      r.Image,
		Entrypoint: []string{"rafiki"},
		Cmd:        []string{"executor", "serve", "--connect-socket", relaySocketPath},
		WorkingDir: r.Workdir,
		User:       r.User,
		Labels:     labelsFor(r, in),
	}

	env, err := envFor(r, in)
	if err != nil {
		return nil, err
	}
	body.Env = env

	mounts, err := mountsFor(r, in)
	if err != nil {
		return nil, err
	}
	// The relay mount is always present: the container's `rafiki executor
	// serve` connects to the launcher's socket through it. Connecting to a
	// unix socket works on a read-only bind.
	mounts = append(mounts, mount{
		Type:     "bind",
		Source:   in.RelayHostDir,
		Target:   ContainerRelayDir,
		ReadOnly: true,
	})

	body.HostConfig = hostConfig{
		Mounts:         mounts,
		NetworkMode:    networkMode(r.Network),
		ReadonlyRootfs: r.ReadOnlyRootfs,
		Memory:         r.MemoryBytes,
		NanoCpus:       nanoCPUs(r.CPUs),
		PidsLimit:      r.PidsLimit,
		// The env credential makes a restart rejoin the daemon without a new
		// create, so a sandbox survives a host reboot.
		RestartPolicy: restartPolicy{Name: "unless-stopped"},
	}
	return json.Marshal(body)
}

// labelsFor merges the caller's labels with the daemon-owned sandbox labels.
// The daemon's labels win; Validate already refused user labels in the
// rafiki/ and rafiki. namespaces, so this is defence in depth.
func labelsFor(r Resolved, in CreateInputs) map[string]string {
	labels := make(map[string]string, len(r.Labels)+2)
	for k, v := range r.Labels {
		labels[k] = v
	}
	labels[DockerLabelSandbox] = in.SandboxID
	if in.OwnerChild != "" {
		labels[DockerLabelChild] = in.OwnerChild
	}
	return labels
}

// envFor renders the spec's env as sorted K=V pairs and appends the executor
// credential. Validate refused any RAFIKI_-prefixed spec key, so the appended
// credential cannot collide with one.
func envFor(r Resolved, in CreateInputs) ([]string, error) {
	if len(r.Env) == 0 && in.Credential == "" {
		return nil, nil
	}
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		env = append(env, k+"="+r.Env[k])
	}
	env = append(env, CredentialEnv+"="+in.Credential)
	return env, nil
}

// mountsFor renders one Docker mount per spec mount.
func mountsFor(r Resolved, in CreateInputs) ([]mount, error) {
	out := make([]mount, 0, len(r.Mounts))
	for i, m := range r.Mounts {
		switch m.Kind {
		case protocol.MountEphemeral:
			out = append(out, mount{Type: "tmpfs", Target: m.Target})
		case protocol.MountRO, protocol.MountRW:
			readOnly := m.Kind == protocol.MountRO
			switch {
			case m.HostPath != "":
				out = append(out, mount{Type: "bind", Source: m.HostPath, Target: m.Target, ReadOnly: readOnly})
			case m.Volume != "":
				if in.OwnerVolumeKey == "" {
					return nil, errors.New("sandbox: a named volume mount requires an owner volume key")
				}
				out = append(out, mount{
					Type:     "volume",
					Source:   "rafiki-" + in.OwnerVolumeKey + "-" + m.Volume,
					Target:   m.Target,
					ReadOnly: readOnly,
				})
			default:
				// rw with neither source is an anonymous volume, removed with
				// the container; it carries no Source.
				out = append(out, mount{Type: "volume", Target: m.Target})
			}
		default:
			return nil, fmt.Errorf("sandbox: mounts[%d] has unknown kind %q", i, m.Kind)
		}
	}
	return out, nil
}

// networkMode maps a resolved network to Docker's mode. egress is the
// host-reachable bridge; none severs the network entirely.
func networkMode(n protocol.NetworkMode) string {
	if n == protocol.NetworkNone {
		return "none"
	}
	return "bridge"
}

// nanoCPUs converts whole and fractional CPUs to Docker's integer nanocpu
// count; a zero stays zero (meaning "no limit") so the field is omitted.
func nanoCPUs(cpus float64) int64 {
	if cpus == 0 {
		return 0
	}
	return int64(cpus * 1e9)
}
