// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// ownerVolumeKeyRe is the character set an OwnerVolumeKey may use. It excludes
// '-' and '.', so the named-volume separator in mountsFor is unambiguous: key
// "a" with volume "b-c" and key "a-b" with volume "c" can no longer both
// produce "rafiki-a-b-c" and share one volume across owners.
var ownerVolumeKeyRe = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

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
// name would not be owner-scoped, so the request is refused. A non-empty key
// must match ownerVolumeKeyRe (no '-'), for the same reason.
func CreateBody(r Resolved, in CreateInputs) ([]byte, error) {
	if in.OwnerVolumeKey != "" && !ownerVolumeKeyRe.MatchString(in.OwnerVolumeKey) {
		return nil, fmt.Errorf("sandbox: owner volume key %q must match %s", in.OwnerVolumeKey, ownerVolumeKeyRe)
	}

	network, err := networkMode(r.Network)
	if err != nil {
		return nil, err
	}
	nano, err := nanoCPUs(r.CPUs)
	if err != nil {
		return nil, err
	}

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
		NetworkMode:    network,
		ReadonlyRootfs: r.ReadOnlyRootfs,
		Memory:         r.MemoryBytes,
		NanoCpus:       nano,
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
// host-reachable bridge; none severs the network entirely. An empty or unknown
// mode is an error: silently defaulting to egress (bridge) would turn the
// zero-value trap into network reach a caller never asked for.
func networkMode(n protocol.NetworkMode) (string, error) {
	switch n {
	case protocol.NetworkEgress:
		return "bridge", nil
	case protocol.NetworkNone:
		return "none", nil
	case "":
		return "", errors.New("sandbox: network is unset; validate the spec first")
	default:
		return "", fmt.Errorf("sandbox: network %q is not %q or %q", n, protocol.NetworkEgress, protocol.NetworkNone)
	}
}

// nanoCPUs converts whole and fractional CPUs to Docker's integer nanocpu
// count. A zero stays zero (meaning "no limit") so the field is omitted.
//
// A positive cpus that truncates to zero would omit the field entirely and
// leave the sandbox with NO CPU limit — a child that passed the clamp would
// escape its cap — and a value past the int64 range would make the conversion
// undefined. Both fail closed instead.
func nanoCPUs(cpus float64) (int64, error) {
	if cpus == 0 {
		return 0, nil
	}
	if cpus < 0 || math.IsNaN(cpus) || math.IsInf(cpus, 0) {
		return 0, fmt.Errorf("sandbox: cpus %g is not a finite positive number", cpus)
	}
	nanos := cpus * 1e9
	if nanos >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("sandbox: cpus %g exceeds the engine's nanocpu range", cpus)
	}
	n := int64(nanos)
	if n <= 0 {
		return 0, fmt.Errorf("sandbox: cpus %g rounds to zero nanocpus, which would leave the sandbox unlimited", cpus)
	}
	return n, nil
}
