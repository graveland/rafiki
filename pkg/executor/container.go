package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"os/user"
	"strconv"
	"strings"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// containerBackend implements Backend via the docker CLI. It is always
// ephemeral — each Provision starts a new container, each Release removes it.
type containerBackend struct {
	image      string
	executorID string
	// version is this executor's own version, compared against the binary baked
	// into the image so skew is at least visible. See warnOnVersionSkew.
	version string
}

func newContainerBackend(image, executorID, version string) *containerBackend {
	return &containerBackend{image: image, executorID: executorID, version: version}
}

// workspaceImageRequirement is one thing a container workspace image must
// provide.
type workspaceImageRequirement struct {
	// name is what the failure calls the missing thing.
	name string
	// argv runs in the fresh container; exit 0 means present.
	argv []string
	// why says what breaks without it, so the message is actionable rather than
	// merely accurate.
	why string
	// reportsVersion marks the check whose stdout is a version string worth
	// comparing against ours.
	reportsVersion bool
}

// The image contract, in one place. Task-by-task discovery is how this becomes
// a list nobody can enumerate: each addition narrows what an operator may bring,
// so it is worth keeping visible and short.
var workspaceImageRequirements = []workspaceImageRequirement{
	{
		name: "rafiki-executor",
		argv: []string{"rafiki-executor", "--version"},
		why: "every tool on a container workspace runs through a tool server inside the container, " +
			"so without this binary the workspace can run nothing at all",
		reportsVersion: true,
	},
	{
		name: "rg (ripgrep)",
		argv: []string{"rg", "--version"},
		why: "the glob and grep tools shell out to rg and DECLINE when it is absent, " +
			"which removes two tools from the agent silently instead of erroring",
	},
}

// validateImage refuses a workspace whose image cannot support it.
//
// This runs at Provision rather than at the first tool call on purpose: a
// capability that goes missing silently is the failure mode here. glob and grep
// decline when ripgrep is absent, so an agent on a bad image gets "no matches"
// and cannot report anything more useful — and an image with no executor binary
// fails every call with a transport error that names nothing.
func (b *containerBackend) validateImage(ctx context.Context, id string) error {
	for _, r := range workspaceImageRequirements {
		out, err := b.probe(ctx, id, r.argv)
		if err != nil {
			return fmt.Errorf("provision: image %q cannot serve a workspace: %s is missing or unusable: %v\n"+
				"  why it is required: %s\n"+
				"  tried: %s\n"+
				"  container said: %s\n"+
				"  the reference image is `docker build --target workspace -t rafiki-workspace:dev .`",
				b.image, r.name, err, r.why, strings.Join(r.argv, " "), strings.TrimSpace(out))
		}
		if r.reportsVersion {
			b.warnOnVersionSkew(strings.TrimSpace(out))
		}
	}
	return nil
}

func (b *containerBackend) probe(ctx context.Context, id string, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", id}, argv...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// warnOnVersionSkew reports a mismatch between this executor and the binary in
// the image, and deliberately does NOT fail.
//
// Baking the binary into the image is what introduced this risk: a `docker cp`
// of the running executor's own artifact could not drift. But refusing on a
// version-string mismatch would make every iteration on the host require an
// image rebuild, and the string is not what actually has to agree — the wire
// does. Provision proves the wire by calling Describe over the stdio transport
// and fails closed if that does not answer, which is a real compatibility check
// where a string comparison is a proxy for one.
//
// If the executor protocol ever grows a version field, that belongs here as a
// hard refusal.
func (b *containerBackend) warnOnVersionSkew(imageVersion string) {
	if b.version == "" || imageVersion == "" || b.version == imageVersion {
		return
	}
	slog.Warn("container: the executor baked into the workspace image is a different build than this one; "+
		"rebuild the image if tools behave unexpectedly",
		"image", b.image, "image_executor_version", imageVersion, "our_version", b.version)
}

func (b *containerBackend) Provision(ctx context.Context, req *executorpb.ProvisionRequest) (*workspace, error) {
	// The workspace id doubles as the docker container name.
	id := "rafiki-ws-" + randomID()

	argv := b.buildRunArgv(id, req)

	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	cmd := exec.CommandContext(ctx, "docker", argv...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	slog.Debug("container: docker run", "argv", argv)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("provision: docker run: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	// From here on the container exists, so every failure path must remove it.
	// A leaked `sleep infinity` container holds its mounts and its pid slot for
	// as long as the docker daemon lives.
	if err := b.validateImage(ctx, id); err != nil {
		// Discarded deliberately: removeContainer logs its own failure, and the
		// validation error is the one worth returning — a cleanup failure on top
		// of it would bury the reason the Provision was refused.
		_ = b.removeContainer(context.WithoutCancel(ctx), id)
		return nil, err
	}

	// Collect roots — one per mount.
	var roots []string
	for _, m := range req.Mounts {
		roots = append(roots, m.ContainerPath)
	}

	ws := &workspace{
		id:        id, // docker container name
		workdir:   req.Workdir,
		roots:     roots,
		isolation: "container",
		childID:   req.ChildId,
		exec:      b.execFunc(id, req.Workdir),
	}
	return ws, nil
}

func (b *containerBackend) buildRunArgv(id string, req *executorpb.ProvisionRequest) []string {
	argv := []string{
		"run", "--detach",
		"--name", id,
		"--label", "dev.graveland.rafiki.executor=" + b.executorID,
		"--label", "dev.graveland.rafiki.child=" + req.ChildId,
		"--network", req.Network,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "512",
	}

	if req.MemoryBytes > 0 {
		argv = append(argv, "--memory", strconv.FormatInt(req.MemoryBytes, 10))
	}
	if req.Cpus > 0 {
		argv = append(argv, "--cpus", strconv.FormatFloat(req.Cpus, 'f', -1, 64))
	}

	if u, err := user.Current(); err == nil {
		argv = append(argv, "--user", u.Uid+":"+u.Gid)
	}

	for k, v := range req.Env {
		argv = append(argv, "--env", k+"="+v)
	}

	for _, m := range req.Mounts {
		spec := m.HostPath + ":" + m.ContainerPath
		if m.ReadOnly {
			spec += ":ro"
		}
		argv = append(argv, "--volume", spec)
	}

	if req.Workdir != "" {
		// Validate workdir is one of the mounts.
		valid := false
		for _, m := range req.Mounts {
			if m.ContainerPath == req.Workdir {
				valid = true
				break
			}
		}
		if !valid {
			return nil
		}
		argv = append(argv, "--workdir", req.Workdir)
	}

	argv = append(argv, b.image, "sleep", "infinity")
	return argv
}

func (b *containerBackend) execFunc(containerName, workdir string) func(ctx context.Context, argv []string) *exec.Cmd {
	return func(ctx context.Context, argv []string) *exec.Cmd {
		args := []string{"exec"}
		if workdir != "" {
			args = append(args, "--workdir", workdir)
		}
		args = append(args, containerName)
		args = append(args, argv...)
		return exec.CommandContext(ctx, "docker", args...)
	}
}

func (b *containerBackend) Release(ctx context.Context, ws *workspace) error {
	return b.removeContainer(ctx, ws.id)
}

// removeContainer is the single teardown path, shared by Release and by
// Provision's failure paths. Provision passes a context.WithoutCancel copy: the
// context that just failed is frequently the reason, and a cleanup that inherits
// the cancellation leaks exactly the container it was written to remove.
func (b *containerBackend) removeContainer(ctx context.Context, id string) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", id).CombinedOutput()
	if err != nil {
		slog.Warn("container: remove failed", "id", id, "error", err, "output", string(out))
		return err
	}
	return nil
}
