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
	bin    string
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
	return &remoteFixture{home: home, bin: bin, record: record, env: env}
}

func (f *remoteFixture) server(t *testing.T) *Server {
	t.Helper()
	return NewServer(Options{Root: t.TempDir(), Env: f.env})
}

// serverAt is server with a dialable address: the one a probe asks the target
// to reach and a launch hands the remote executor as its --connect.
func (f *remoteFixture) serverAt(t *testing.T, addr string) *Server {
	t.Helper()
	return NewServer(Options{Root: t.TempDir(), Env: f.env, ConnectAddr: addr})
}

// dataDir is where the fake target's per-instance state lives.
func (f *remoteFixture) dataDir() string {
	return filepath.Join(f.home, ".local", "share", "rafiki")
}

// writeBin writes one executable helper into the fixture's PATH directory.
func (f *remoteFixture) writeBin(t *testing.T, name, body string) {
	t.Helper()
	assert.NewAborting(t).Require().NoError(
		os.WriteFile(filepath.Join(f.bin, name), []byte(body), 0o755), "write %s", name)
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

func TestParseRemoteProbeAcceptsFourOrSixLines(t *testing.T) {
	c := assert.NewAborting(t)

	four := "uname=Linux x86_64\nbinary=yes\ncred=no\nmarker=install-42\n"
	got, err := parseRemoteProbeOutput(four)
	c.Require().NoError(err, "four lines")
	c.Eq("install-42", got.GetMarker(), "the four-line shape parses as it did")
	c.Eq("unavailable", got.GetReach(), "four lines means the target was never asked")
	c.Eq(remoteReachNoAddr, got.GetReachDetail(), "the four-line detail")

	six := four + "reach=yes\nreach_detail=via curl\n"
	got, err = parseRemoteProbeOutput(six)
	c.Require().NoError(err, "six lines")
	c.Eq("yes", got.GetReach(), "reach")
	c.Eq("via curl", got.GetReachDetail(), "reach detail")

	got, err = parseRemoteProbeOutput(four + "reach=no\nreach_detail=curl: (7) Failed to connect\n")
	c.Require().NoError(err, "six lines with a failure")
	c.Eq("no", got.GetReach(), "reach")
	c.Eq("curl: (7) Failed to connect", got.GetReachDetail(), "the tool's own words survive")

	for name, out := range map[string]string{
		"five lines":     four + "reach=yes\n",
		"wrong prefix":   "platform=Linux x86_64\nbinary=yes\ncred=no\nmarker=\n",
		"reach=maybe":    four + "reach=maybe\nreach_detail=via curl\n",
		"no detail":      four + "reach=yes\nverdict=yes\n",
		"line five only": four + "reach=yes\n",
		"too many":       six + "extra=1\n",
	} {
		_, err := parseRemoteProbeOutput(out)
		c.Require().Error(err, "%s was accepted", name)
	}
}

func TestRemoteProbeReachUnavailableWithoutConnectAddr(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	resp, err := client.RemoteProbe(context.Background(), connect.NewRequest(&executorpb.RemoteProbeRequest{
		Target: &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
	}))
	c.Require().NoError(err, "probe")
	c.Eq("unavailable", resp.Msg.GetReach(),
		"a launcher with no dialable address reports unavailable, which selects tunnel")
	c.Eq("launcher has no dialable address", resp.Msg.GetReachDetail(), "reach detail")

	argv := f.argv(t)
	c.NotStrContains(argv[len(argv)-1], "reach=",
		"a probe with no address must not ask the target to dial anything")
}

func TestRemoteProbeReachRejectsAHostileAddress(t *testing.T) {
	c := assert.NewAborting(t)

	for _, addr := range []string{
		"127.0.0.1",
		"[::1]:8035",
		"127.0.0.1:9999; touch /tmp/rafiki-pwned",
		"127.0.0.1:9999 $(id)",
		"127.0.0.1:9999 'quoted'",
		"http://127.0.0.1:8035",
		"127.0.0.1:99999999",
	} {
		f := newRemoteFixture(t)
		client := newRemoteClient(t, f.serverAt(t, addr))

		resp, err := client.RemoteProbe(context.Background(), connect.NewRequest(&executorpb.RemoteProbeRequest{
			Target: &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
		}))
		c.Require().NoError(err, "probe with address %q", addr)
		c.Eq("unavailable", resp.Msg.GetReach(), "address %q", addr)
		c.StrContains(resp.Msg.GetReachDetail(), "is not a plain host:port", "address %q", addr)

		script := f.argv(t)[len(f.argv(t))-1]
		c.NotStrContains(script, "reach=", "address %q reached the remote script", addr)
		c.NotStrContains(script, addr, "address %q reached the remote script", addr)
		c.NotStrContains(script, "touch /tmp/rafiki-pwned", "address %q carried shell syntax", addr)
	}
}

func TestRemoteProbeScriptRunsReachChecks(t *testing.T) {
	addr := "daemon.example:8035"

	// run executes the probe script under sh with a PATH the test owns
	// completely: no system curl, bash, nc or timeout is visible, so the tool
	// the script picks is the test's choice and nothing else's. The script is
	// bounded here too, so a branch that hangs fails the test instead of it.
	run := func(t *testing.T, dialAddr string, tools map[string]string) (map[string]string, *remoteFixture) {
		t.Helper()
		c := assert.NewAborting(t)
		f := &remoteFixture{bin: t.TempDir(), home: t.TempDir()}
		for name, body := range tools {
			f.writeBin(t, name, body)
		}
		for _, name := range []string{"head", "tr", "uname"} {
			real, err := exec.LookPath(name)
			c.Require().NoError(err, "%s must be on the test's PATH", name)
			c.Require().NoError(os.Symlink(real, filepath.Join(f.bin, name)), "symlink %s", name)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", remoteProbeScript(paths.DefaultInstance, "", dialAddr))
		cmd.Env = []string{"PATH=" + f.bin, "HOME=" + f.home}
		// A branch that hangs would otherwise leave the shell's child holding
		// the pipe, so the bound has to close it rather than wait for it.
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.Output()
		c.Require().NoError(err, "run the probe script")

		lines := map[string]string{}
		for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
			k, v, ok := strings.Cut(line, "=")
			c.Require().True(ok, "unparsable probe line %q", line)
			lines[k] = v
		}
		return lines, f
	}
	argvOf := func(c *assert.C, path string) []string {
		c.Helper()
		b, err := os.ReadFile(path)
		c.Require().NoError(err, "read %s", path)
		return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}

	t.Run("curl that connects", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, f := run(t, addr, map[string]string{"curl": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/curl.argv\"\nexit 0\n"})
		c.Eq("yes", lines["reach"], "curl exit 0 is reached")
		c.Eq("via curl", lines["reach_detail"], "the tool that answered")
		argv := argvOf(c, filepath.Join(f.home, "curl.argv"))
		c.EqDeep([]string{"-sS", "-m", "5", "-o", "/dev/null", "https://" + addr + "/healthz"}, argv,
			"the curl invocation, without -f and without -k: any answer over the system roots counts")
	})

	t.Run("curl that fails", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, _ := run(t, addr, map[string]string{"curl": "#!/bin/sh\n" +
			"printf 'curl: (7) Failed to connect to daemon.example port 8035\\n' >&2\nexit 7\n"})
		c.Eq("no", lines["reach"], "a failing curl is not reached")
		c.Eq("curl: (7) Failed to connect to daemon.example port 8035", lines["reach_detail"],
			"reach_detail is the tool's own first stderr line")
	})

	t.Run("bash under timeout connects", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, f := run(t, addr, map[string]string{
			"bash":    "#!/bin/sh\n: > \"$HOME/bash.ran\"\n",
			"timeout": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/timeout.argv\"\nexit 0\n",
		})
		c.Eq("yes", lines["reach"], "a bounded bash /dev/tcp is reached")
		c.Eq("via bash", lines["reach_detail"], "the tool that answered")
		c.EqDeep([]string{"5", "bash", "-c", "exec 3<>/dev/tcp/'daemon.example'/'8035'"},
			argvOf(c, filepath.Join(f.home, "timeout.argv")),
			"bash's /dev/tcp runs under timeout, always")
		c.NoFileExists(filepath.Join(f.home, "bash.ran"), "bash was run directly, with nothing to bound it")
	})

	t.Run("no timeout means no bash probe", func(t *testing.T) {
		// bash's /dev/tcp has no connect timeout of its own: with no `timeout` on
		// the target the probe must not run it at all, or a black-holed route
		// hangs the whole probe. The fake bash sleeps for far longer than the
		// probe's own bound, so taking the branch fails the test.
		c := assert.NewAborting(t)
		lines, f := run(t, addr, map[string]string{
			"bash": "#!/bin/sh\n: > \"$HOME/bash.ran\"\nexec /bin/sleep 30\n",
		})
		c.Eq("no", lines["reach"], "an unusable bash is not reached")
		c.Eq("no curl, bash or nc on the target", lines["reach_detail"], "the no-tool answer")
		c.NoFileExists(filepath.Join(f.home, "bash.ran"), "bash was probed with no timeout to bound it")
	})

	t.Run("no timeout falls through to nc", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, f := run(t, addr, map[string]string{
			"bash": "#!/bin/sh\n: > \"$HOME/bash.ran\"\nexec /bin/sleep 30\n",
			"nc":   "#!/bin/sh\nexit 0\n",
		})
		c.Eq("yes", lines["reach"], "nc is still reached without timeout")
		c.Eq("via nc", lines["reach_detail"], "the tool that answered")
		c.NoFileExists(filepath.Join(f.home, "bash.ran"), "bash was probed with no timeout to bound it")
	})

	t.Run("no curl, nc connects", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, f := run(t, addr, map[string]string{"nc": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/nc.argv\"\nexit 0\n"})
		c.Eq("yes", lines["reach"], "nc exit 0 is reached")
		c.Eq("via nc", lines["reach_detail"], "the tool that answered")
		argv := argvOf(c, filepath.Join(f.home, "nc.argv"))
		c.EqDeep([]string{"-z", "-w", "5", "daemon.example", "8035"}, argv, "the nc invocation")
	})

	t.Run("no curl, bash or nc", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, _ := run(t, addr, nil)
		c.Eq("no", lines["reach"], "a target with no tool is not reached")
		c.Eq("no curl, bash or nc on the target", lines["reach_detail"],
			"the answer names why nothing was tried")
	})

	t.Run("curl's stderr keeps one line", func(t *testing.T) {
		c := assert.NewAborting(t)
		lines, _ := run(t, addr, map[string]string{"curl": "#!/bin/sh\n" +
			"printf 'first line\\nsecond line\\n' >&2\nexit 7\n"})
		c.Eq("first line", lines["reach_detail"], "newlines removed: one line, the first")
	})
}

func TestRemoteCleanStopsOnlyAMatchingInstall(t *testing.T) {
	c := assert.NewAborting(t)

	// sleeper starts a process in its own process group, as the detached
	// executor is, and reaps it the moment it dies so `kill -0` answers ESRCH
	// rather than a zombie's success.
	sleeper := func(t *testing.T) int {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		c.Require().NoError(cmd.Start(), "start the sleeper")
		go func() { _ = cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd.Process.Pid
	}
	run := func(t *testing.T, home, installID string) string {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", remoteCleanScript(paths.DefaultInstance, installID, true))
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
		out, err := cmd.Output()
		c.Require().NoError(err, "run the clean script")
		return string(out)
	}
	seed := func(t *testing.T) (home, dir string, pid int) {
		t.Helper()
		home = t.TempDir()
		dir = filepath.Join(home, ".local", "share", "rafiki")
		c.Require().NoError(os.MkdirAll(dir, 0o755), "mkdir the data dir")
		pid = sleeper(t)
		c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.pid"),
			[]byte(strconv.Itoa(pid)+"\n"), 0o600), "write the pid file")
		c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.cred"), []byte("cred\n"), 0o600), "cred")
		c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.lock"), nil, 0o600), "lock")
		return home, dir, pid
	}

	t.Run("a matching marker stops the process", func(t *testing.T) {
		c := assert.NewAborting(t)
		home, dir, pid := seed(t)
		c.Require().NoError(os.WriteFile(filepath.Join(dir, "installed-executor"),
			[]byte("install-a\n"), 0o600), "write the marker")

		out := run(t, home, "install-a")
		c.StrContains(out, "stopped\n", "a matching marker stops the install")
		c.StrContains(out, "removed\n", "the marker is removed")
		c.True(waitForRemoteDeath(pid), "the install's process group survived the stop")
		for _, name := range []string{"installed-executor", "executor.pid", "executor.cred", "executor.lock"} {
			_, err := os.Stat(filepath.Join(dir, name))
			c.True(os.IsNotExist(err), "%s survived a matching clean", name)
		}
	})

	t.Run("a mismatched marker leaves everything alone", func(t *testing.T) {
		c := assert.NewAborting(t)
		home, dir, pid := seed(t)
		c.Require().NoError(os.WriteFile(filepath.Join(dir, "installed-executor"),
			[]byte("install-b\n"), 0o600), "write the marker")

		out := run(t, home, "install-a")
		c.NotStrContains(out, "removed", "another install's marker is not this one's to remove")
		c.NotStrContains(out, "stopped", "another install's process is not this one's to stop")
		c.False(remoteProcessDead(pid), "the other install's process was killed")
		for _, name := range []string{"installed-executor", "executor.pid", "executor.cred", "executor.lock"} {
			_, err := os.Stat(filepath.Join(dir, name))
			c.NoError(err, "%s was removed for another install", name)
		}
	})

	t.Run("stop requested with no pid file", func(t *testing.T) {
		c := assert.NewAborting(t)
		home, dir, _ := seed(t)
		c.Require().NoError(os.Remove(filepath.Join(dir, "executor.pid")), "clear the pid file")
		c.Require().NoError(os.WriteFile(filepath.Join(dir, "installed-executor"),
			[]byte("install-a\n"), 0o600), "write the marker")

		out := run(t, home, "install-a")
		c.StrContains(out, "removed\n", "the marker is still removed")
		c.NotStrContains(out, "stopped", "nothing was stopped, so nothing reports a stop")
	})
}

// remoteStopRun is one execution of the clean script. kill is a shell FUNCTION
// the test owns: the shell's kill is a BUILTIN, so a PATH stub would never run,
// while a function shadows the builtin exactly where the script's own `kill`
// calls look. It records its argv and returns a status, so no signal is ever
// sent and every signal the script tried to send is a line.
type remoteStopRun struct {
	output string
	kill   []string
	dir    string
}

// runRemoteStop seeds a target whose install marker matches install-a, puts
// pidLine in executor.pid, and runs the full clean script under sh. ps selects
// what the target can ask about the pid's process group: "" is no ps at all,
// "real" is the host's own ps, anything else is a script printing a pgid.
// alive makes the kill stub answer `kill -0` with success, so the process is
// still running after both signals.
func runRemoteStop(t *testing.T, pidLine string, alive bool, ps string) remoteStopRun {
	t.Helper()
	c := assert.NewAborting(t)
	f := &remoteFixture{bin: t.TempDir(), home: t.TempDir()}
	// The poll's sleep is a no-op so the two 5s bounds cost the test nothing.
	f.writeBin(t, "sleep", "#!/bin/sh\nexit 0\n")
	for _, name := range []string{"head", "tr", "rm"} {
		real, err := exec.LookPath(name)
		c.Require().NoError(err, "%s must be on the test's PATH", name)
		c.Require().NoError(os.Symlink(real, filepath.Join(f.bin, name)), "symlink %s", name)
	}
	switch ps {
	case "":
	case "real":
		real, err := exec.LookPath("ps")
		c.Require().NoError(err, "ps must be on the test's PATH")
		c.Require().NoError(os.Symlink(real, filepath.Join(f.bin, "ps")), "symlink ps")
	default:
		f.writeBin(t, "ps", ps)
	}

	dir := filepath.Join(f.home, ".local", "share", "rafiki")
	c.Require().NoError(os.MkdirAll(dir, 0o755), "mkdir the data dir")
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "installed-executor"), []byte("install-a\n"), 0o600), "marker")
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.pid"), []byte(pidLine+"\n"), 0o600), "pid file")
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.cred"), []byte("cred\n"), 0o600), "cred")
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "executor.lock"), nil, 0o600), "lock")

	zero := "1"
	if alive {
		zero = "0"
	}
	record := filepath.Join(f.home, "kill.argv")
	stub := `kill() { printf '%s\n' "$*" >> ` + shellQuote(record) +
		`; case "$1" in -0) return ` + zero + ` ;; *) return 0 ;; esac; }; `

	cmd := exec.Command("/bin/sh", "-c",
		stub+remoteCleanScript(paths.DefaultInstance, "install-a", true))
	cmd.Env = []string{"PATH=" + f.bin, "HOME=" + f.home}
	out, err := cmd.Output()
	c.Require().NoError(err, "run the clean script")
	run := remoteStopRun{output: string(out), dir: dir}
	if raw, rerr := os.ReadFile(record); rerr == nil {
		run.kill = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
	return run
}

// standIn starts a process for the stop script to be wrong about: group puts it
// in its own process group, as the detached executor is.
func standIn(t *testing.T, group bool) int {
	t.Helper()
	c := assert.NewAborting(t)
	cmd := exec.Command("sleep", "60")
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	c.Require().NoError(cmd.Start(), "start the stand-in process")
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

// TestRemoteStopRefusesAnImplausiblePid pins the pid guard: a plausible-looking
// pid 1 reaches `kill -TERM -1` (every process the remote user may signal, this
// ssh session included), and a pid the guard stops rejecting signals something
// nobody vouched for.
func TestRemoteStopRefusesAnImplausiblePid(t *testing.T) {
	victim := standIn(t, true)

	for _, pid := range []string{"", "0", "1", "-5", "12abc", "12345678901234567890"} {
		t.Run("pid="+strconv.Quote(pid), func(t *testing.T) {
			c := assert.NewAborting(t)
			run := runRemoteStop(t, pid, false, "real")
			c.Eq(0, len(run.kill), "the stop script signalled %v for pid %q", run.kill, pid)
			c.NotStrContains(run.output, "stopped", "pid %q was reported stopped", pid)
			c.StrContains(run.output, "removed\n", "the marker is still removed for pid %q", pid)
			for _, name := range []string{"executor.pid", "executor.cred", "executor.lock"} {
				c.FileExists(filepath.Join(run.dir, name), "%s was removed for pid %q", name, pid)
			}
			c.False(remoteProcessDead(victim), "the stand-in process was signalled")
		})
	}
}

// TestRemoteStopSignalsTheGroupOnlyForASessionLeader pins the pgid equality: a
// group signal is only safe when ps says this pid IS the group's leader, and
// anything else — a different pgid, no ps at all — is a plain pid signal.
func TestRemoteStopSignalsTheGroupOnlyForASessionLeader(t *testing.T) {
	t.Run("a session leader takes its group", func(t *testing.T) {
		c := assert.NewAborting(t)
		pid := standIn(t, true)
		run := runRemoteStop(t, strconv.Itoa(pid), false, "real")
		c.Require().NotEmpty(run.kill, "the stop script sent no signal")
		c.Eq("-TERM -"+strconv.Itoa(pid), run.kill[0], "a session leader's pid is its process group")
		c.StrContains(run.output, "stopped\n", "a process reported gone is stopped")
		c.NoFileExists(filepath.Join(run.dir, "executor.pid"), "a stopped process's pid file is removed")
	})

	t.Run("a pid outside its own group is signalled alone", func(t *testing.T) {
		c := assert.NewAborting(t)
		pid := standIn(t, false)
		run := runRemoteStop(t, strconv.Itoa(pid), false, "real")
		c.Require().NotEmpty(run.kill, "the stop script sent no signal")
		c.Eq("-TERM "+strconv.Itoa(pid), run.kill[0], "a pid that leads no group must not take one with it")
	})

	t.Run("no ps means no group signal", func(t *testing.T) {
		c := assert.NewAborting(t)
		run := runRemoteStop(t, "4242", false, "")
		c.Require().NotEmpty(run.kill, "the stop script sent no signal")
		c.Eq("-TERM 4242", run.kill[0], "with no ps to vouch for it, the pid is signalled alone")
	})

	t.Run("a ps that disagrees means no group signal", func(t *testing.T) {
		c := assert.NewAborting(t)
		run := runRemoteStop(t, "4242", false, "#!/bin/sh\nprintf '  9999\\n'\n")
		c.Require().NotEmpty(run.kill, "the stop script sent no signal")
		c.Eq("-TERM 4242", run.kill[0], "a pgid that is not the pid is not this pid's group")
	})
}

// TestRemoteStopLeavesStateWhenTheProcessSurvivesKill pins the success gate: a
// live executor must keep its credential and lock, and the daemon must not be
// told it stopped.
func TestRemoteStopLeavesStateWhenTheProcessSurvivesKill(t *testing.T) {
	c := assert.NewAborting(t)
	run := runRemoteStop(t, "4242", true, "#!/bin/sh\nprintf '  4242\\n'\n")

	c.Require().NotEmpty(run.kill, "the stop script sent no signal")
	c.Eq("-TERM -4242", run.kill[0], "the first signal is TERM")
	c.Contains(run.kill, "-KILL -4242", "a survivor of TERM is sent KILL")
	c.NotStrContains(run.output, "stopped", "a process that survived KILL was reported stopped")
	c.StrContains(run.output, "removed\n", "the marker is still removed")
	for _, name := range []string{"executor.pid", "executor.cred", "executor.lock"} {
		c.FileExists(filepath.Join(run.dir, name), "%s was removed although the process survived", name)
	}
}
