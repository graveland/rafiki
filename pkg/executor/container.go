package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
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
		name: "rafiki",
		argv: []string{"rafiki", "--version"},
		why: "every tool on a container workspace runs through `rafiki executor serve-stdio` inside " +
			"the container, so without this binary the workspace can run nothing at all",
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

// innerStartTimeout bounds the startup handshake with the inner server.
//
// The lesson is execpool's join path, where an unbounded Describe held a
// goroutine forever against a peer that completed the handshake and then went
// silent. Here the peer is a `docker exec` away, so the same shape applies: a
// container that starts but never answers must fail the Provision rather than
// hang it.
const innerStartTimeout = 30 * time.Second

// startInnerServer runs `rafiki executor serve-stdio` inside the container and
// returns a client speaking to it.
//
// Confirming it answers Describe before returning is the point: a Provision that
// hands back a dead inner server turns into a confusing failure on the child's
// first tool call, somewhere else entirely, instead of a clear one here. It is
// also the real compatibility check that the version-string comparison in
// warnOnVersionSkew only approximates — if the baked binary cannot speak this
// protocol, this is where it shows.
func (b *containerBackend) startInnerServer(ctx context.Context, id, workdir string) (*innerServer, error) {
	argv := []string{"exec", "-i"}
	if workdir != "" {
		argv = append(argv, "--workdir", workdir)
	}
	argv = append(argv, id, "rafiki", "executor", "serve-stdio")
	if workdir != "" {
		argv = append(argv, "--root", workdir)
	}

	// exec.Command, NOT exec.CommandContext. ctx here belongs to the Provision
	// RPC and is cancelled the moment Provision returns; binding the process to
	// it would kill the inner server exactly when the workspace becomes usable.
	// Its lifetime is the workspace's, and Close ends it.
	cmd := exec.Command("docker", argv...)
	// The inner server logs to stderr, and forwarding it means a failure inside
	// the container is visible in the executor's own log rather than swallowed.
	// stdout is untouched: it is the wire.
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("provision: inner server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("provision: inner server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("provision: start inner server: %w", err)
	}

	conn := execpool.NewStdioConn(stdout, stdin)
	httpClient, err := execpool.ClientForConn(conn)
	if err != nil {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("provision: inner server transport: %w", err)
	}

	inner := &innerServer{
		client: executorpbconnect.NewExecutorServiceClient(httpClient, "http://container"),
		cmd:    cmd,
		conn:   conn,
	}

	dctx, cancel := context.WithTimeout(ctx, innerStartTimeout)
	defer cancel()
	if _, err := inner.client.Describe(dctx, connect.NewRequest(&executorpb.DescribeRequest{})); err != nil {
		inner.Close()
		return nil, fmt.Errorf("provision: the tool server in image %q did not answer Describe within %s: %w",
			b.image, innerStartTimeout, err)
	}
	return inner, nil
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
func (b *containerBackend) warnOnVersionSkew(probeOutput string) {
	// `rafiki --version` prints "rafiki version X", not a bare X. Comparing the
	// whole line against our version would differ every time and make this warn
	// unconditionally — a warning that always fires is one nobody reads.
	fields := strings.Fields(probeOutput)
	if len(fields) == 0 {
		return
	}
	imageVersion := fields[len(fields)-1]

	// A build that is not a release build carries a sentinel, and two sentinels
	// tell you nothing about whether the code matches. Comparing them is pure
	// noise in exactly the situation — local development — where it fires most.
	if unversioned(imageVersion) || unversioned(b.version) || imageVersion == b.version {
		return
	}
	slog.Warn("container: the rafiki baked into the workspace image is a different build than this executor; "+
		"rebuild the image if tools behave unexpectedly",
		"image", b.image, "image_version", imageVersion, "our_version", b.version)
}

func unversioned(v string) bool {
	switch v {
	case "", "dev", "unknown", "test":
		return true
	}
	return false
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

	inner, err := b.startInnerServer(ctx, id, req.Workdir)
	if err != nil {
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
		inner:     inner,
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
	// Order matters: stop the inner server before removing the container.
	// Removing first leaves the `docker exec` client reaping oddly against a
	// container that is already gone.
	if ws.inner != nil {
		ws.inner.Close()
	}
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
