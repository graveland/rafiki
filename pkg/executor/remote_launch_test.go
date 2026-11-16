// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// remoteLaunchTicket is long and distinctive enough that a substring search for
// it in an argv, a script or an error message means what it says.
const remoteLaunchTicket = "tkt-7c1f9a2e4b"

// remoteLaunchAddr is the launcher's own dialable address in these tests.
const remoteLaunchAddr = "127.0.0.1:8035"

func launchStart(binPath, root, installID string) *executorpb.RemoteLaunchRequest {
	return &executorpb.RemoteLaunchRequest{
		Msg: &executorpb.RemoteLaunchRequest_Start{Start: &executorpb.RemoteLaunchStart{
			Target:     &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
			BinaryPath: binPath,
			Root:       root,
			InstallId:  installID,
		}},
	}
}

func launchTicket(ticket string) *executorpb.RemoteLaunchRequest {
	return &executorpb.RemoteLaunchRequest{
		Msg: &executorpb.RemoteLaunchRequest_Stdin{Stdin: []byte(ticket)},
	}
}

// launchFrames sends the frames, half-closes the request side as the daemon
// does, and returns the response or the server's refusal. A send that fails
// after the server answered is not the test's subject, so it is returned only
// when the server answered nothing.
func launchFrames(
	t *testing.T,
	client executorpbconnect.ExecutorServiceClient,
	frames ...*executorpb.RemoteLaunchRequest,
) (*connect.Response[executorpb.RemoteLaunchResponse], error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	stream := client.RemoteLaunch(ctx)
	var sendErr error
	for _, f := range frames {
		if err := stream.Send(f); err != nil {
			sendErr = err
			break
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return resp, err
	}
	return resp, sendErr
}

// recordingLaunchBinary is a fake remote executor: it writes the argv it was
// exec'd with and the ticket it read from stdin into the fixture's HOME, so a
// test can see exactly what the launch handed the target.
const recordingLaunchBinary = `#!/bin/sh
printf '%s\n' "$@" > "$HOME/argv"
read -r line
printf '%s' "$line" > "$HOME/ticket"
printf 'started pid=4242\n'
`

// printingLaunchBinary is a fake remote executor that reads (and drops) the
// ticket line, prints exactly out, and exits with status.
func printingLaunchBinary(out string, status int) string {
	return "#!/bin/sh\nread -r line\nprintf '" + out + "'\nexit " + strconv.Itoa(status) + "\n"
}

func TestRemoteLaunchScriptArgv(t *testing.T) {
	c := assert.NewAborting(t)
	const binPath = "$HOME/.cache/rafiki/bin/rafiki-v1.2.3-abcdef"

	got := remoteLaunchScript(binPath, "work-2", "/srv/a'b", "install-7", remoteLaunchAddr)
	want := `exec "$HOME"/'.cache/rafiki/bin/rafiki-v1.2.3-abcdef' ` +
		`executor serve --connect '127.0.0.1:8035' -P 'work-2' --root '/srv/a'\''b' ` +
		`--install-id 'install-7' --ticket-stdin --detach`
	c.Eq(want, got, "the launch script")

	c.Eq(`exec "$HOME"/'.cache/rafiki/bin/rafiki-v1.2.3-abcdef' `+
		`executor serve --connect '127.0.0.1:8035' -P 'pro-file' `+
		`--install-id 'install-7' --ticket-stdin --detach`,
		remoteLaunchScript(binPath, "pro-file", "", "install-7", remoteLaunchAddr),
		"no --root when the request names none")

	// The instance is validated to [A-Za-z0-9._-] before it can reach a script,
	// but a quote must still be inert here: this function is where the rule lives.
	c.StrContains(remoteLaunchScript(binPath, "ins't", "", "install-7", remoteLaunchAddr),
		`-P 'ins'\''t'`, "a quote in a value is one shell word, never syntax")
}

func TestRemoteLaunchTicketNeverInSSHArgv(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	binPath := f.installBinary(t, "rafiki-launch", recordingLaunchBinary)
	client := newRemoteClient(t, f.serverAt(t, remoteLaunchAddr))

	resp, err := launchFrames(t, client, launchStart(binPath, "", "install-7"), launchTicket(remoteLaunchTicket))
	c.Require().NoError(err, "launch")
	c.Eq("started", resp.Msg.GetState(), "state")
	c.Eq(int32(4242), resp.Msg.GetPid(), "pid")

	// The ticket travelled on stdin: the fake executor read it there.
	ticket, err := os.ReadFile(filepath.Join(f.home, "ticket"))
	c.Require().NoError(err, "the target's executor read no ticket from stdin")
	c.Eq(remoteLaunchTicket, string(ticket), "the ticket arrived on stdin, as one line")

	// And nowhere else: not in the ssh argv, and not in the remote command.
	argv := f.argv(t)
	c.Require().True(len(argv) > 1, "the launch ran no ssh")
	for _, arg := range argv {
		c.NotStrContains(arg, remoteLaunchTicket, "the ticket reached the ssh argv")
	}
	script := argv[len(argv)-1]
	c.StrContains(script, "--ticket-stdin --detach", "the remote executor is told to read its ticket and detach")
	c.StrContains(script, "--connect '127.0.0.1:8035'", "the remote executor is handed this launcher's address")
	c.NotStrContains(script, remoteLaunchTicket, "the ticket reached the remote command")
}

func TestRemoteLaunchScrubsTheTicketFromErrors(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	// A target whose shell echoes back what it read: the launcher must not hand
	// that echo to the daemon (or its log) as-is.
	binPath := f.installBinary(t, "rafiki-echo-ticket",
		"#!/bin/sh\nread -r line\nprintf '%s\\n' \"$line\" >&2\nexit 1\n")
	client := newRemoteClient(t, f.serverAt(t, remoteLaunchAddr))

	_, err := launchFrames(t, client, launchStart(binPath, "", "install-7"), launchTicket(remoteLaunchTicket))
	c.Require().Error(err, "a failed launch was reported as a success")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(err), "a failed launch is Unavailable")
	c.NotStrContains(err.Error(), remoteLaunchTicket, "the ticket reached the error message")
	c.StrContains(err.Error(), "[redacted]", "the echoed ticket is scrubbed, not merely absent")
}

func TestRemoteLaunchRefusesWithoutConnectAddr(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	binPath := f.installBinary(t, "rafiki-launch", recordingLaunchBinary)
	client := newRemoteClient(t, f.server(t))

	_, err := launchFrames(t, client, launchStart(binPath, "", "install-7"), launchTicket(remoteLaunchTicket))
	c.Require().Error(err, "a launcher with no dialable address launched a self-dialling executor")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "no address is a precondition, not a remote failure")
	c.StrContains(err.Error(),
		"this launcher dials the daemon by no address, so it cannot start a self-dialling executor",
		"the refusal names the reason")

	_, statErr := os.Stat(f.record)
	c.True(os.IsNotExist(statErr), "ssh must not run for a launcher with no address")
}

func TestRemoteLaunchParsesStartedAndRunning(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		out   string
		state string
		pid   int32
	}{
		{"started pid=4242\n", "started", 4242},
		{"running pid=7\n", "running", 7},
	} {
		f := newRemoteFixture(t)
		binPath := f.installBinary(t, "rafiki-launch", printingLaunchBinary(tc.out, 0))
		client := newRemoteClient(t, f.serverAt(t, remoteLaunchAddr))

		resp, err := launchFrames(t, client, launchStart(binPath, "", "install-7"), launchTicket(remoteLaunchTicket))
		c.Require().NoError(err, "launch printing %q", tc.out)
		c.Eq(tc.state, resp.Msg.GetState(), "state from %q", tc.out)
		c.Eq(tc.pid, resp.Msg.GetPid(), "pid from %q", tc.out)
	}
}

func TestRemoteLaunchRejectsAnUnparsableOutput(t *testing.T) {
	c := assert.NewAborting(t)

	for _, out := range []string{"boom\n", "started\n", "started pid=zero\n", "started pid=1\nrunning pid=2\n"} {
		f := newRemoteFixture(t)
		binPath := f.installBinary(t, "rafiki-launch", printingLaunchBinary(out, 0))
		client := newRemoteClient(t, f.serverAt(t, remoteLaunchAddr))

		_, err := launchFrames(t, client, launchStart(binPath, "", "install-7"), launchTicket(remoteLaunchTicket))
		c.Require().Error(err, "output %q was accepted", out)
		c.Eq(connect.CodeInternal, connect.CodeOf(err), "output %q", out)
		c.StrContains(err.Error(), "raw output:", "the raw output must reach the caller")
	}
}

func TestRemoteLaunchRejectsASecondStdinFrame(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	binPath := f.installBinary(t, "rafiki-launch", recordingLaunchBinary)
	client := newRemoteClient(t, f.serverAt(t, remoteLaunchAddr))

	_, err := launchFrames(t, client,
		launchStart(binPath, "", "install-7"),
		launchTicket(remoteLaunchTicket),
		launchTicket("tkt-second"),
	)
	c.Require().Error(err, "a second stdin frame was accepted")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "two tickets is a protocol error")

	_, statErr := os.Stat(f.record)
	c.True(os.IsNotExist(statErr), "a launch with two tickets must be refused before any ssh runs")
}

func TestRemoteLogTailClampsLines(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	c.Require().NoError(os.MkdirAll(f.dataDir(), 0o755), "mkdir the data dir")
	log := "line-1\nline-2\nline-3\nline-4\nline-5\n"
	c.Require().NoError(os.WriteFile(filepath.Join(f.dataDir(), "executor.log"), []byte(log), 0o600),
		"write the remote log")

	tail := func(lines int32) (string, string) {
		t.Helper()
		resp, err := client.RemoteLogTail(context.Background(), connect.NewRequest(&executorpb.RemoteLogTailRequest{
			Target: &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
			Lines:  lines,
		}))
		c.Require().NoError(err, "log tail with lines=%d", lines)
		argv := f.argv(t)
		return resp.Msg.GetTail(), argv[len(argv)-1]
	}

	got, script := tail(3)
	c.StrContains(script, `tail -n 3 "$d/executor.log"`, "the requested count reaches the target")
	c.Eq("line-3\nline-4\nline-5\n", got, "the last lines of the remote log")

	_, script = tail(0)
	c.StrContains(script, "tail -n 40", "zero means the default of 40 lines")
	_, script = tail(-5)
	c.StrContains(script, "tail -n 1", "a negative count is one line, never zero or negative")
	_, script = tail(1000)
	c.StrContains(script, "tail -n 200", "a count past the cap is the cap")
}

func TestRemoteLogTailMissingFile(t *testing.T) {
	c := assert.NewAborting(t)
	f := newRemoteFixture(t)
	client := newRemoteClient(t, f.server(t))

	resp, err := client.RemoteLogTail(context.Background(), connect.NewRequest(&executorpb.RemoteLogTailRequest{
		Target: &executorpb.RemoteTarget{Destination: "target", Instance: paths.DefaultInstance},
		Lines:  40,
	}))
	c.Require().NoError(err, "a missing executor.log is an answer, not an error")
	c.Eq("(no executor.log)", resp.Msg.GetTail(), "the missing-file sentinel")

	// A bad target is still a protocol error.
	_, err = client.RemoteLogTail(context.Background(), connect.NewRequest(&executorpb.RemoteLogTailRequest{
		Target: &executorpb.RemoteTarget{Destination: "-oProxyCommand=touch /tmp/rafiki-pwned", Instance: paths.DefaultInstance},
	}))
	c.Require().Error(err, "a hostile destination was accepted")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "a bad target is InvalidArgument")

	// And so is one with no target at all.
	_, err = client.RemoteLogTail(context.Background(), connect.NewRequest(&executorpb.RemoteLogTailRequest{}))
	c.Require().Error(err, "a request with no target was accepted")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "a missing target is InvalidArgument")
}

// unusedImportAnchor keeps the launch test's import list honest if a helper is
// removed: it fails compilation rather than silently dropping a package.
var _ = strings.TrimSpace
