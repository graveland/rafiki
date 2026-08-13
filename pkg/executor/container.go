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
}

func newContainerBackend(image, executorID string) *containerBackend {
	return &containerBackend{image: image, executorID: executorID}
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
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", ws.id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("container: release failed", "id", ws.id, "error", err, "output", string(out))
		return err
	}
	return nil
}