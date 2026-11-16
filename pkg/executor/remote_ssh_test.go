// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// remoteFakeSSH is the ssh every test in this file runs. It records its argv,
// then — like a real ssh — drops the options and the destination and runs the
// one remaining argument as a shell command. That makes the remote script run
// LOCALLY, against the test's temp HOME, so a test can assert on what the
// target would actually have done.
const remoteFakeSSH = `#!/bin/sh
if [ -n "$RAFIKI_TEST_SSH_RECORD" ]; then
	: > "$RAFIKI_TEST_SSH_RECORD"
	for a in "$@"; do printf '%s\n' "$a" >> "$RAFIKI_TEST_SSH_RECORD"; done
fi
if [ -n "$RAFIKI_TEST_SSH_OUTPUT" ]; then
	printf '%s' "$RAFIKI_TEST_SSH_OUTPUT"
	exit 0
fi
while [ $# -gt 0 ]; do
	case "$1" in
		-T) shift ;;
		-o) shift; shift ;;
		-*) shift ;;
		*) break ;;
	esac
done
shift
exec /bin/sh -c "$1"
`

type remoteFixture struct {
	home   string
	record string
	env    []string
}

func newRemoteFixture(t *testing.T, extraEnv ...string) *remoteFixture {
	t.Helper()
	c := assert.NewAborting(t)
	bin := t.TempDir()
	home := t.TempDir()
	record := filepath.Join(bin, "ssh-argv")
	c.Require().NoError(os.WriteFile(filepath.Join(bin, "ssh"), []byte(remoteFakeSSH), 0o755), "write fake ssh")
	env := []string{
		"PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + home,
		"RAFIKI_TEST_SSH_RECORD=" + record,
	}
	env = append(env, extraEnv...)
	return &remoteFixture{home: home, record: record, env: env}
}

func (f *remoteFixture) server(t *testing.T) *Server {
	t.Helper()
	return NewServer(Options{Root: t.TempDir(), Env: f.env})
}

func (f *remoteFixture) argv(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.record)
	assert.NewAborting(t).Require().NoError(err, "read the recorded ssh argv")
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// installBinary writes a script at the content-addressed path the RPC accepts
// and returns its $HOME-relative spelling.
func (f *remoteFixture) installBinary(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(f.home, ".cache", "rafiki", "bin", name)
	c := assert.NewAborting(t)
	c.Require().NoError(os.MkdirAll(filepath.Dir(p), 0o755), "mkdir the binary cache")
	c.Require().NoError(os.WriteFile(p, []byte(body), 0o755), "write the remote binary")
	return "$HOME/.cache/rafiki/bin/" + name
}

// newRemoteClient mounts the executor service on an unencrypted HTTP/2 unix
// socket. The socket lives in os.TempDir() because t.TempDir() can exceed the
// unix path limit on macOS.
func newRemoteClient(t *testing.T, srv *Server) executorpbconnect.ExecutorServiceClient {
	t.Helper()
	c := assert.NewAborting(t)
	sum := sha256.Sum256([]byte(t.Name()))
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("rafiki-remote-%x.sock", sum[:8]))
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })

	mux := http.NewServeMux()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv))
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Handler: mux, Protocols: protos}
	ln, err := net.Listen("unix", sock)
	c.Require().NoError(err, "listen")
	t.Cleanup(func() { _ = httpSrv.Close() })
	go func() { _ = httpSrv.Serve(ln) }()

	return executorpbconnect.NewExecutorServiceClient(
		&http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
		"http://executor",
	)
}

func TestSSHArgvIsExactlyTheOptionSet(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	_, err := client.RemoteProbe(context.Background(), connect.NewRequest(&executorpb.RemoteProbeRequest{
		Target: &executorpb.RemoteTarget{Destination: "user@target.example", Instance: paths.DefaultInstance},
	}))
	c.Require().NoError(err, "probe")

	argv := f.argv(t)
	want := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"user@target.example",
	}
	c.Require().Eq(len(want)+1, len(argv), "ssh argv = %q", argv)
	c.EqDeep(want, argv[:len(want)], "ssh option set and destination")
	joined := strings.Join(argv, " ")
	c.NotStrContains(joined, "StrictHostKeyChecking", "ssh must carry no host-key option")
	c.NotStrContains(joined, "UserKnownHostsFile", "ssh must carry no host-key option")
	c.StrContains(argv[len(argv)-1], "uname=", "the last argv element is the remote command")
}

func TestRemoteRefusesOptionInjection(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	for _, dest := range []string{
		"-oProxyCommand=touch /tmp/rafiki-pwned",
		"-T",
		"two words",
		"line\nbreak",
		"",
	} {
		_, err := client.RemoteProbe(context.Background(), connect.NewRequest(&executorpb.RemoteProbeRequest{
			Target: &executorpb.RemoteTarget{Destination: dest, Instance: paths.DefaultInstance},
		}))
		c.Require().Error(err, "destination %q was accepted", dest)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "destination %q", dest)
	}

	_, statErr := os.Stat(f.record)
	c.True(os.IsNotExist(statErr), "ssh must never run for a refused destination")
}

func TestRemoteQuotingSurvivesHostileValues(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	binPath := f.installBinary(t, "rafiki-echo",
		"#!/bin/sh\nfor a in \"$@\"; do printf 'arg=%s\\n' \"$a\" >&2; done\n")
	client := newRemoteClient(t, f.server(t))

	root := `/tmp/a'b; rm -rf ~`
	stream := client.RemoteServe(context.Background())
	c.Require().NoError(stream.Send(&executorpb.RemoteServeRequest{
		Msg: &executorpb.RemoteServeRequest_Start{Start: &executorpb.RemoteServeStart{
			Target:     &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
			BinaryPath: binPath,
			Root:       root,
			InstallId:  "install-1",
		}},
	}), "send start")

	frames := remoteServeCollect(t, stream)
	c.Require().NoError(frames.err, "serve")
	c.Require().True(frames.exit != nil, "no exit frame")
	stderr := string(frames.stderr)
	c.StrContains(stderr, "arg="+root+"\n", "the hostile root must arrive as ONE argument")
	c.NotStrContains(stderr, "arg=b; rm -rf ~", "the hostile root was split into shell words")
	_, statErr := os.Stat(f.home)
	c.NoError(statErr, "the hostile root executed `rm -rf ~`")
}

func TestRemoteDataDirMatchesPaths(t *testing.T) {
	c := assert.NewAborting(t)
	paths.ResetInstanceForTest()
	t.Cleanup(paths.ResetInstanceForTest)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")

	run := func(script string) string {
		out, err := exec.Command("/bin/sh", "-c", script).Output()
		c.Require().NoError(err, "run %q", script)
		return string(out)
	}

	for _, instance := range []string{paths.DefaultInstance, "work-2"} {
		c.Require().NoError(paths.SetInstance(instance), "SetInstance")
		c.Eq(paths.DataDir(), run(remoteDataDirScript(instance)+`; printf %s "$d"`),
			"data dir for instance %q", instance)
	}

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c.Require().NoError(paths.SetInstance("work-2"), "SetInstance")
	c.Eq(paths.DataDir(), run(remoteDataDirScript("work-2")+`; printf %s "$d"`),
		"data dir under XDG_DATA_HOME")
}

func TestRemoteProbeParsesFourLines(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)

	dataDir := filepath.Join(f.home, ".local", "share", "rafiki")
	c.Require().NoError(os.MkdirAll(dataDir, 0o755), "mkdir the data dir")
	c.Require().NoError(os.WriteFile(filepath.Join(dataDir, "executor.cred"), []byte("cred\n"), 0o600), "cred")
	c.Require().NoError(os.WriteFile(filepath.Join(dataDir, "installed-executor"), []byte("install-42\n"), 0o600), "marker")
	binPath := f.installBinary(t, "rafiki-v1", "#!/bin/sh\n")

	client := newRemoteClient(t, f.server(t))
	probe := func(instance, binaryPath string) *executorpb.RemoteProbeResponse {
		resp, err := client.RemoteProbe(context.Background(), connect.NewRequest(&executorpb.RemoteProbeRequest{
			Target:     &executorpb.RemoteTarget{Destination: "target", Instance: instance},
			BinaryPath: binaryPath,
		}))
		c.Require().NoError(err, "probe")
		return resp.Msg
	}

	uname, err := exec.Command("uname", "-sm").Output()
	c.Require().NoError(err, "uname -sm")
	got := probe(paths.DefaultInstance, binPath)
	c.Eq(strings.TrimSpace(string(uname)), got.GetUname(), "uname")
	c.True(got.GetBinaryPresent(), "binary present")
	c.True(got.GetCredentialPresent(), "credential present")
	c.Eq("install-42", got.GetMarker(), "marker")

	got = probe(paths.DefaultInstance, "")
	c.False(got.GetBinaryPresent(), "an empty binary path is never present")
	c.True(got.GetCredentialPresent(), "credential present")

	got = probe("work", binPath)
	c.False(got.GetCredentialPresent(), "another instance has its own namespace")
	c.Eq("", got.GetMarker(), "another instance has no marker")
}

func TestRemoteProbeStrangeOutputIsInternal(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t, "RAFIKI_TEST_SSH_OUTPUT=not a probe at all\n")
	client := newRemoteClient(t, f.server(t))

	_, err := client.RemoteProbe(context.Background(), connect.NewRequest(&executorpb.RemoteProbeRequest{
		Target: &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
	}))
	c.Require().Error(err, "strange probe output was accepted")
	c.Eq(connect.CodeInternal, connect.CodeOf(err), "a shape the parser does not know is Internal")
	c.StrContains(err.Error(), "not a probe at all", "the raw output must reach the caller")
}

func TestRemoteInstallVerifiesSha(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	const binPath = "$HOME/.cache/rafiki/bin/rafiki-v1"
	installed := filepath.Join(f.home, ".cache", "rafiki", "bin", "rafiki-v1")
	install := func(sha string, body []byte) error {
		stream := client.RemoteInstall(context.Background())
		if err := stream.Send(&executorpb.RemoteInstallRequest{
			Msg: &executorpb.RemoteInstallRequest_Header{Header: &executorpb.RemoteInstallHeader{
				Target:     &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
				BinaryPath: binPath,
				Sha256:     sha,
			}},
		}); err != nil {
			return err
		}
		if err := stream.Send(&executorpb.RemoteInstallRequest{
			Msg: &executorpb.RemoteInstallRequest_Chunk{Chunk: body},
		}); err != nil {
			return err
		}
		_, err := stream.CloseAndReceive()
		return err
	}

	err := install(strings.Repeat("0", 64), []byte("hello"))
	c.Require().Error(err, "a bad sha was accepted")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "a failed remote install is Unavailable")
	_, statErr := os.Stat(installed)
	c.True(os.IsNotExist(statErr), "a bad sha left a file at binary_path")
	if entries, rerr := os.ReadDir(filepath.Dir(installed)); rerr == nil {
		for _, e := range entries {
			c.NotStrContains(e.Name(), ".rafiki.", "a temp file survived a failed install")
		}
	}

	sum := sha256.Sum256([]byte("hello"))
	c.Require().NoError(install(hex.EncodeToString(sum[:]), []byte("hello")), "a matching sha installs")
	body, err := os.ReadFile(installed)
	c.Require().NoError(err, "read the installed binary")
	c.Eq("hello", string(body), "installed bytes")
	info, err := os.Stat(installed)
	c.Require().NoError(err, "stat the installed binary")
	c.True(info.Mode()&0o111 != 0, "the installed binary is executable")
}

// remoteServeFrames is everything one RemoteServe stream delivered.
type remoteServeFrames struct {
	started *executorpb.RemoteServeStarted
	stdout  []byte
	stderr  []byte
	exit    *executorpb.RemoteServeExit
	err     error
}

func remoteServeCollect(t *testing.T, stream *connect.BidiStreamForClient[executorpb.RemoteServeRequest, executorpb.RemoteServeResponse]) remoteServeFrames {
	t.Helper()
	var f remoteServeFrames
	for {
		msg, err := stream.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				f.err = err
			}
			return f
		}
		switch {
		case msg.GetStarted() != nil:
			f.started = msg.GetStarted()
		case msg.GetStdout() != nil:
			f.stdout = append(f.stdout, msg.GetStdout()...)
		case msg.GetStderr() != nil:
			f.stderr = append(f.stderr, msg.GetStderr()...)
		case msg.GetExit() != nil:
			f.exit = msg.GetExit()
		}
	}
}

func TestRemoteServeRelaysAndSweepsOnStreamEnd(t *testing.T) {
	startStream := func(t *testing.T, f *remoteFixture, binPath string) (*connect.BidiStreamForClient[executorpb.RemoteServeRequest, executorpb.RemoteServeResponse], context.CancelFunc) {
		t.Helper()
		client := newRemoteClient(t, f.server(t))
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		stream := client.RemoteServe(ctx)
		c := assert.NewAborting(t)
		c.Require().NoError(stream.Send(&executorpb.RemoteServeRequest{
			Msg: &executorpb.RemoteServeRequest_Start{Start: &executorpb.RemoteServeStart{
				Target:     &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
				BinaryPath: binPath,
				InstallId:  "install-1",
			}},
		}), "send start")
		return stream, cancel
	}

	t.Run("relays stdin stdout and stderr", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newRemoteFixture(t)
		binPath := f.installBinary(t, "rafiki-relay",
			"#!/bin/sh\nread line\necho \"err:$line\" >&2\nsleep 0.2\necho \"out:$line\"\n")
		stream, _ := startStream(t, f, binPath)
		c.Require().NoError(stream.Send(&executorpb.RemoteServeRequest{
			Msg: &executorpb.RemoteServeRequest_Stdin{Stdin: []byte("hello\n")},
		}), "send stdin")
		c.Require().NoError(stream.CloseRequest(), "half-close")

		frames := remoteServeCollect(t, stream)
		c.Require().NoError(frames.err, "serve")
		c.Require().True(frames.exit != nil, "no exit frame")
		c.Eq(int32(0), frames.exit.GetCode(), "exit code")
		c.StrContains(string(frames.stdout), "out:hello", "stdout relayed")
		// A stderr FRAME may be dropped under a stalled send (that is the
		// design); the exit's tail is the record that must survive either way.
		stderr := string(frames.stderr) + frames.exit.GetStderrTail()
		c.StrContains(stderr, "err:hello", "stderr relayed")
	})

	t.Run("stream end kills the process group", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newRemoteFixture(t)
		binPath := f.installBinary(t, "rafiki-sleeper",
			"#!/bin/sh\nsleep 60 &\necho $! > \"$HOME/grandchild.pid\"\necho ready\nsleep 60\n")
		stream, _ := startStream(t, f, binPath)

		var leader int
		deadline := time.Now().Add(10 * time.Second)
		for {
			msg, err := stream.Receive()
			c.Require().NoError(err, "receive before ready")
			if msg.GetStarted() != nil {
				leader = int(msg.GetStarted().GetPid())
			}
			if strings.Contains(string(msg.GetStdout()), "ready") {
				break
			}
			c.Require().True(time.Now().Before(deadline), "the fixture never printed ready")
		}
		grandchild := waitForRemotePID(t, filepath.Join(f.home, "grandchild.pid"))

		// The daemon went away: both sides of its stream end. The launcher must
		// kill the process group rather than leave a remote executor nobody can
		// talk to. (Closing the response side resets the stream, which is what
		// cancels the handler's context.)
		c.Require().NoError(stream.CloseRequest(), "close the request side")
		c.Require().NoError(stream.CloseResponse(), "close the response side")

		c.True(waitForRemoteDeath(leader), "the ssh process survived stream end")
		c.True(waitForRemoteDeath(grandchild), "a forked child survived stream end")
	})

	t.Run("a forked survivor is swept when the leader exits", func(t *testing.T) {
		c := assert.NewAborting(t)
		f := newRemoteFixture(t)
		binPath := f.installBinary(t, "rafiki-forker",
			"#!/bin/sh\nsleep 60 &\necho $! > \"$HOME/grandchild.pid\"\nexit 0\n")
		stream, _ := startStream(t, f, binPath)

		frames := remoteServeCollect(t, stream)
		c.Require().NoError(frames.err, "serve")
		c.Require().True(frames.exit != nil, "no exit frame")
		grandchild := waitForRemotePID(t, filepath.Join(f.home, "grandchild.pid"))
		// The sweep runs BEFORE the exit frame is sent, so the survivor is
		// already gone by the time this frame arrives.
		c.True(remoteProcessDead(grandchild), "a forked survivor outlived the leader")
	})
}

func TestRemoteNoSSHLaunchRefusesAllFour(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	srv := NewServer(Options{Root: t.TempDir(), Env: f.env, NoSSHLaunch: true})
	client := newRemoteClient(t, srv)

	const want = "ssh launch is disabled on this executor (--no-ssh-launch)"
	ctx := context.Background()
	target := &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance}
	refused := func(err error, what string) {
		t.Helper()
		c.Require().Error(err, "%s was not refused", what)
		c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "%s", what)
		c.StrContains(err.Error(), want, "%s", what)
	}

	_, err := client.RemoteProbe(ctx, connect.NewRequest(&executorpb.RemoteProbeRequest{Target: target}))
	refused(err, "RemoteProbe")

	_, err = client.RemoteClean(ctx, connect.NewRequest(&executorpb.RemoteCleanRequest{Target: target, InstallId: "install-1"}))
	refused(err, "RemoteClean")

	install := client.RemoteInstall(ctx)
	_ = install.Send(&executorpb.RemoteInstallRequest{
		Msg: &executorpb.RemoteInstallRequest_Header{Header: &executorpb.RemoteInstallHeader{
			Target:     target,
			BinaryPath: "$HOME/.cache/rafiki/bin/rafiki-v1",
			Sha256:     strings.Repeat("0", 64),
		}},
	})
	_, err = install.CloseAndReceive()
	refused(err, "RemoteInstall")

	serve := client.RemoteServe(ctx)
	_ = serve.Send(&executorpb.RemoteServeRequest{
		Msg: &executorpb.RemoteServeRequest_Start{Start: &executorpb.RemoteServeStart{
			Target:     target,
			BinaryPath: "$HOME/.cache/rafiki/bin/rafiki-v1",
			InstallId:  "install-1",
		}},
	})
	_, err = serve.Receive()
	refused(err, "RemoteServe")

	_, statErr := os.Stat(f.record)
	c.True(os.IsNotExist(statErr), "ssh must not run when ssh launch is disabled")
}

func TestRemoteCleanOnlyRemovesMatchingMarker(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	marker := filepath.Join(f.home, ".local", "share", "rafiki", "installed-executor")
	c.Require().NoError(os.MkdirAll(filepath.Dir(marker), 0o755), "mkdir the data dir")
	c.Require().NoError(os.WriteFile(marker, []byte("install-a\n"), 0o600), "write the marker")

	clean := func(installID string) bool {
		resp, err := client.RemoteClean(context.Background(), connect.NewRequest(&executorpb.RemoteCleanRequest{
			Target:    &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
			InstallId: installID,
		}))
		c.Require().NoError(err, "clean %q", installID)
		return resp.Msg.GetRemoved()
	}

	c.False(clean("install-b"), "a different id must not remove the marker")
	_, err := os.Stat(marker)
	c.NoError(err, "the marker survived a non-matching clean")
	c.True(clean("install-a"), "the matching id removes the marker")
	_, err = os.Stat(marker)
	c.True(os.IsNotExist(err), "the marker is gone")
	c.False(clean("install-a"), "a second clean is a no-op")
}

// remoteProcessDead reports whether pid has stopped doing work: ESRCH is the
// clean answer, and a zombie counts as dead (a reparented process is not this
// test's to reap).
func remoteProcessDead(pid int) bool {
	if pid <= 0 {
		return true
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func waitForRemoteDeath(pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if remoteProcessDead(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitForRemotePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if n, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && n > 0 {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fixture never wrote %s", path)
	return 0
}
