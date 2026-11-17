// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/paths"
)

// The Remote* RPCs are how a NATIVE executor installs and runs executors on
// other machines over ssh. The daemon never runs ssh: it asks a launcher it
// already trusts, and the launcher is trusted with the target's traffic exactly
// as far as it is already trusted with the ssh keys to it.
//
// Everything the remote command is built from is a TYPED request field, quoted
// for a POSIX shell. Nothing caller-supplied reaches the command unquoted, so a
// hostile value can never become shell syntax; the validation below is the
// second line, refusing the shapes that make no sense at all (a destination
// that would be read as an ssh option, a path outside the content-addressed
// binary cache).
//
// The process handling is modelled on MCPStdio: one ssh per stream, its own
// process group, stdout/stderr pumped as opaque bytes, and the group swept the
// instant the leader is reaped so nothing it forked outlives the stream.

// remoteSSHOptions is the whole ssh option set, always and only. No host-key
// option: ProxyJump, User, keys and Include all come from the launcher's own
// ssh config, because that is the operator's trust decision, not the daemon's.
var remoteSSHOptions = []string{
	"-T",
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
}

// sshArgs builds the exact argv every Remote* ssh runs.
func sshArgs(destination, remoteCommand string) []string {
	args := make([]string, 0, len(remoteSSHOptions)+2)
	args = append(args, remoteSSHOptions...)
	return append(args, destination, remoteCommand)
}

// shellQuote renders s as ONE POSIX shell word. Every value interpolated into a
// remote command goes through this; the only exception is the literal $HOME
// prefix of binary_path, which remoteBinaryExpr handles.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteBinaryExpr renders a validated binary_path for a remote command. The
// daemon only ever sends $HOME/.cache/rafiki/bin/rafiki-<tag>-<sha>; $HOME is
// expanded by the remote shell and the remainder is quoted like any other
// value, so a cache path can never be read as syntax.
func remoteBinaryExpr(binaryPath string) string {
	const home = "$HOME/"
	if strings.HasPrefix(binaryPath, home) {
		return `"$HOME"/` + shellQuote(strings.TrimPrefix(binaryPath, home))
	}
	return shellQuote(binaryPath)
}

// RemoteDataDirExpr renders the target's data directory as a POSIX shell
// expression, unexpanded — the form an operator can paste on the target, and
// the form the daemon's install refusals name a file in. It MUST agree with
// paths.DataDir() under paths.SetInstance(instance); remoteDataDirScript
// assigns exactly this expression to $d, so the credential and the install
// marker are read and written through the same path a message names.
// It is deliberately NOT shell-quoted, so it reads the same in a script and in
// a message. That is safe because the instance name is validated to
// [A-Za-z0-9._-] by paths.ValidateInstance at the one gate every Remote* RPC
// shares (validateRemoteTarget), and a variable assignment does not word-split
// in any case.
func RemoteDataDirExpr(instance string) string {
	expr := `${XDG_DATA_HOME:-$HOME/.local/share}/rafiki`
	if instance != paths.DefaultInstance {
		expr += "/profiles/" + instance
	}
	return expr
}

// remoteDataDirScript is the POSIX sh that computes the target's data directory
// in $d. It MUST agree with paths.DataDir() under paths.SetInstance(instance):
// the credential and the install marker are read and written through it.
func remoteDataDirScript(instance string) string {
	return "d=" + RemoteDataDirExpr(instance)
}

// remoteProbeScript prints exactly four lines, in this order:
//
//	uname=<uname -sm>
//	binary=<yes|no>
//	cred=<yes|no>
//	marker=<first line of the install marker, or empty>
//
// and, when this launcher has an address a target could dial, the two reach
// lines remoteReachScript defines. addr is THIS launcher's own address, never
// a request field: it is what the target is asked to reach, so it never travels
// through the daemon. An address that is not a plain host:port is treated as
// no address at all — it is never rendered into a script, and the probe
// reports it unavailable with the address named.
func remoteProbeScript(instance, binaryPath, addr string) string {
	bin := remoteBinaryExpr(binaryPath)
	s := remoteDataDirScript(instance) + "; " +
		`printf 'uname=%s\n' "$(uname -sm)"; ` +
		`if [ -x ` + bin + ` ]; then printf 'binary=yes\n'; else printf 'binary=no\n'; fi; ` +
		`if [ -f "$d/executor.cred" ]; then printf 'cred=yes\n'; else printf 'cred=no\n'; fi; ` +
		`if [ -f "$d/installed-executor" ]; then printf 'marker=%s\n' "$(head -n 1 "$d/installed-executor")"; else printf 'marker=\n'; fi`
	if !remoteReachAddrRE.MatchString(addr) {
		return s
	}
	return s + "; " + remoteReachScript(addr)
}

// remoteReachScript asks the target whether it can dial addr, and prints the
// probe's last two lines:
//
//	reach=<yes|no>
//	reach_detail=<one line>
//
// The target tries the first of curl, bash's /dev/tcp (only where `timeout` can
// bound it) and nc that it has; a success is named (via curl / via bash / via
// nc) and a failure carries the tool's own first stderr line, so the daemon can
// say what the target said rather than summarizing it. Host and port are
// interpolated only through shellQuote, and only after remoteReachAddrRE has
// vouched for them.
func remoteReachScript(addr string) string {
	host, port, _ := strings.Cut(addr, ":")
	h, p := shellQuote(host), shellQuote(port)
	// onFail is the tail of every branch: the target is unreachable and
	// reach_detail is the first line of the tool's stderr. err is captured from
	// stderr only (2>&1 duplicates it onto the capture pipe before stdout is
	// sent to /dev/null), and the carriage return comes off so the detail is one
	// clean line whatever the target's line endings are.
	onFail := `reach=no; reach_detail=$(printf '%s\n' "$err" | head -n 1 | tr -d '\r')`
	bashExec := `exec 3<>/dev/tcp/` + h + `/` + p
	// curl runs WITHOUT -k on purpose: a remote executor dials the daemon with
	// the system roots only (pinned self-signed certificates are an explicit
	// non-goal), so a target that cannot verify the daemon's certificate cannot
	// connect either, and curl's exit status alone is the honest answer — and
	// because any HTTP answer counts, -f would misreport a 404 as unreachable.
	curlBranch := `if command -v curl >/dev/null 2>&1; then ` +
		`if err=$(curl -sS -m 5 -o /dev/null ` + shellQuote("https://"+addr+"/healthz") + ` 2>&1 >/dev/null); ` +
		`then reach=yes; reach_detail='via curl'; else ` + onFail + `; fi; `
	// The bash branch is only usable where `timeout` exists to bound it: bash's
	// /dev/tcp has no connect timeout of its own, and a black-holed route would
	// hang the probe (macOS ships no timeout). Without it, nc — which has -w —
	// is next.
	bashBranch := `elif command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then ` +
		`if err=$(timeout 5 bash -c ` + shellQuote(bashExec) + ` 2>&1 >/dev/null); ` +
		`then reach=yes; reach_detail='via bash'; else ` + onFail + `; fi; `
	ncBranch := `elif command -v nc >/dev/null 2>&1; then ` +
		`if err=$(nc -z -w 5 ` + h + ` ` + p + ` 2>&1 >/dev/null); ` +
		`then reach=yes; reach_detail='via nc'; else ` + onFail + `; fi; `
	return "reach=no\n" +
		`reach_detail='no curl, bash or nc on the target'` + "\n" +
		curlBranch + bashBranch + ncBranch +
		`fi` + "\n" +
		`printf 'reach=%s\n' "$reach"; printf 'reach_detail=%s\n' "$reach_detail"`
}

// remoteInstallScript writes stdin to a temp file in the content-addressed
// binary cache, verifies it before the final rename, and removes the temp file
// on any failure. No scp or sftp: the target needs no internet and the launcher
// keeps no copy.
func remoteInstallScript(binaryPath, sha256 string) string {
	return `mkdir -p "$HOME/.cache/rafiki/bin" && ` +
		`t=$(mktemp "$HOME/.cache/rafiki/bin/.rafiki.XXXXXX") && ` +
		`cat > "$t" && ` +
		`{ sha256sum "$t" 2>/dev/null || shasum -a 256 "$t" 2>/dev/null || sha256 -q "$t"; } | awk '{print $1}' | grep -qx ` + shellQuote(sha256) + ` && ` +
		`chmod 0755 "$t" && mv -f "$t" ` + remoteBinaryExpr(binaryPath) + ` || { rm -f "$t"; exit 1; }`
}

// remoteServeScript runs the installed executor in its stdio mode. `exec`
// replaces the shell, so the ssh channel's exit status is the executor's.
func remoteServeScript(binaryPath, instance, root, installID string) string {
	s := `exec ` + remoteBinaryExpr(binaryPath) + ` executor serve --stdio -P ` + shellQuote(instance)
	if root != "" {
		s += ` --root ` + shellQuote(root)
	}
	return s + ` --install-id ` + shellQuote(installID)
}

// remoteCleanScript removes the install marker iff its first line names THIS
// install. The shared binary cache is content-addressed and is left alone.
//
// With stop, the marker's own process is stopped FIRST and only under that
// same match: a marker for another install id leaves the process, the pid file
// and the credential alone, and prints neither word. A stop that is asked for
// and not ACHIEVED removes nothing at all — not even the marker — and prints
// neither word either: the marker is the only file that names which install the
// leftover credential belongs to.
func remoteCleanScript(instance, installID string, stop bool) string {
	s := remoteDataDirScript(instance) +
		`; f="$d/installed-executor"; ` +
		`[ -f "$f" ] && [ "$(head -n 1 "$f")" = ` + shellQuote(installID) + ` ] || exit 0; `
	if stop {
		s += remoteStopScript(installID)
	}
	return s + `rm -f "$f" && echo removed || true`
}

// remoteStopScript stops the detached executor recorded in $d/executor.pid and
// removes the per-install state that would let it come back: the pid file, the
// credential and the lock. TERM first, then up to 5s of 0.5s polls, then KILL
// and the same again. It runs inside the matching-marker gate
// remoteCleanScript opens, never on its own, and it ends the whole script
// (exit 0, before the marker removal) when it cannot finish its job.
//
// A PID IS NOT AN IDENTITY. The kernel reuses pids, so this file can name an
// unrelated process — or an unrelated session, since a session-leader pid is
// its own group. Before anything is signalled the process's command line must
// carry THIS install's id, which the detached child keeps in its --install-id
// argv (--detach reshapes its parent's argv but keeps that flag). A ps that
// cannot answer, prints nothing, or prints somebody else's command line is no
// evidence at all: nothing is signalled and no file is touched.
//
// The detached executor is a session leader, so its pid is also its process
// group id and one signal reaches everything it forked. That is true only of a
// pid `ps` agrees is the group's leader: everything else is signalled as a
// plain pid, and a ps that cannot answer is no evidence of a group. Nothing here
// ever guesses.
//
// The pid must be 2..9999999: `kill -TERM -0` would signal the remote shell's
// OWN process group and `kill -TERM -1` every process the remote user may
// signal, this ssh session included, so an implausible pid file is no pid.
//
// `stopped` is printed, and the per-install files removed, only once the process
// is confirmed gone — where a ZOMBIE counts as gone, because a defunct process
// holds a pid without doing any work, and waiting one out would wedge every
// reinstall. Every other way out removes nothing and prints neither word: an
// orphaned credential beside a removed marker leaves the next install's clobber
// guard with nothing to name, and telling the daemon an executor stopped while
// it is still running is worse than saying nothing.
func remoteStopScript(installID string) string {
	return `id=` + shellQuote(installID) + `; ` +
		// gone is the one liveness question, so the poll and the success gate
		// cannot disagree: a pid kill cannot find, or one ps calls a zombie.
		`gone() { kill -0 "$1" 2>/dev/null || return 0; ` +
		`case "$(ps -o stat= -p "$1" 2>/dev/null | tr -d '[:space:]')" in Z*) return 0 ;; esac; ` +
		`return 1; }; ` +
		`p=""; [ -f "$d/executor.pid" ] && p=$(head -n 1 "$d/executor.pid"); ` +
		`[ -n "$p" ] || exit 0; ` +
		`case "$p" in 0|0*|*[!0-9]*) exit 0 ;; esac; ` +
		`case "$p" in ????????*) exit 0 ;; esac; ` +
		`[ "$p" -ge 2 ] || exit 0; ` +
		`if ! gone "$p"; then ` +
		// $id is letters, digits, '-' and '_' (validateRemoteInstallID), so it
		// is a literal in this pattern and never glob syntax.
		`case "$(ps -o command= -p "$p" 2>/dev/null)" in *"$id"*) ;; *) exit 0 ;; esac; ` +
		`g=$(ps -o pgid= -p "$p" 2>/dev/null | tr -d '[:space:]'); ` +
		`tg="$p"; [ "$g" = "$p" ] && tg="-$p"; ` +
		`kill -TERM "$tg" 2>/dev/null || true; ` +
		`i=0; while ! gone "$p" && [ "$i" -lt 10 ]; do sleep 0.5; i=$((i+1)); done; ` +
		`if ! gone "$p"; then ` +
		`kill -KILL "$tg" 2>/dev/null || true; ` +
		`i=0; while ! gone "$p" && [ "$i" -lt 10 ]; do sleep 0.5; i=$((i+1)); done; ` +
		`fi; ` +
		`gone "$p" || exit 0; ` +
		`fi; ` +
		`rm -f "$d/executor.pid" "$d/executor.cred" "$d/executor.lock"; echo stopped; `
}

// Validation limits. A destination is an ssh destination, not a command: the
// character set is not restricted (user@host and IPv6 are legal), but anything
// that would be read as an option, split into words, or smuggle a control
// character is refused before ssh is executed.
const remoteDestinationMax = 255

// remoteReachNoAddr is what the parse answers for a four-line probe: this
// launcher has no dialable address, so the target was never asked.
const remoteReachNoAddr = "launcher has no dialable address"

var (
	remoteBinaryPathRE = regexp.MustCompile(`^\$HOME/\.cache/rafiki/bin/rafiki-[A-Za-z0-9._-]+$`)
	remoteInstallIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	remoteSHA256RE     = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

	// remoteReachAddrRE is the shape of an address a target is asked to dial: a
	// plain host:port. Anything else — a URL, a bracketed IPv6 literal, a path,
	// a value with shell syntax in it — is not something a target can be told to
	// reach, and is treated as no address at all.
	remoteReachAddrRE = regexp.MustCompile(`^[A-Za-z0-9._-]+:[0-9]{1,5}$`)
)

func validateRemoteDestination(dest string) error {
	if dest == "" {
		return errors.New("destination must not be empty")
	}
	if len(dest) > remoteDestinationMax {
		return fmt.Errorf("destination is longer than %d bytes", remoteDestinationMax)
	}
	if strings.HasPrefix(dest, "-") {
		return fmt.Errorf("destination %q must not start with '-': it would be read as an ssh option", dest)
	}
	for _, r := range dest {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("destination %q must not contain whitespace or control characters", dest)
		}
	}
	return nil
}

func validateRemoteTarget(t *executorpb.RemoteTarget) error {
	if t == nil {
		return errors.New("target is required")
	}
	if err := validateRemoteDestination(t.GetDestination()); err != nil {
		return err
	}
	if err := paths.ValidateInstance(t.GetInstance()); err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	return nil
}

func validateRemoteBinaryPath(binaryPath string, allowEmpty bool) error {
	if binaryPath == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("binary_path is required")
	}
	if !remoteBinaryPathRE.MatchString(binaryPath) {
		return fmt.Errorf("binary_path %q must be $HOME/.cache/rafiki/bin/rafiki-<tag>-<sha>", binaryPath)
	}
	return nil
}

func validateRemoteRoot(root string) error {
	if root == "" {
		return nil
	}
	if !strings.HasPrefix(root, "/") {
		return fmt.Errorf("root %q must be an absolute path on the target", root)
	}
	for _, r := range root {
		if unicode.IsControl(r) {
			return fmt.Errorf("root %q must not contain control characters", root)
		}
	}
	return nil
}

func validateRemoteInstallID(installID string) error {
	if !remoteInstallIDRE.MatchString(installID) {
		return fmt.Errorf("install_id %q must be 1-64 characters of letters, digits, '-' or '_'", installID)
	}
	return nil
}

// refuseSSHLaunch is the operator's opt-out: `executor serve --no-ssh-launch`
// makes this executor refuse every Remote* RPC, before it validates anything.
func (s *Server) refuseSSHLaunch() error {
	if s.opts.NoSSHLaunch {
		return connect.NewError(connect.CodePermissionDenied,
			errors.New("ssh launch is disabled on this executor (--no-ssh-launch)"))
	}
	return nil
}

// lookPathIn resolves name against the PATH in env, not the process's own. The
// executor's pinned env is what every subprocess it spawns runs under, so ssh
// must be found the same way a tool shell would find it.
func lookPathIn(env []string, name string) (string, error) {
	path := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

// sshCommand builds the one ssh a Remote* call runs. ctx cancels it; Setpgid
// gives the serve path a group to sweep, exactly as MCPStdio does.
func (s *Server) sshCommand(ctx context.Context, destination, remoteCommand string) (*exec.Cmd, error) {
	env := s.opts.Env
	if env == nil {
		env = os.Environ()
	}
	sshPath, err := lookPathIn(env, "ssh")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, sshPath, sshArgs(destination, remoteCommand)...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// cappedBuffer keeps the last max bytes written to it, so ssh's stderr is
// bounded no matter how chatty a refusal is.
type cappedBuffer struct {
	max int
	buf []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf = append(b.buf, p...)
	if b.max > 0 && len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return n, nil
}

func (b *cappedBuffer) Bytes() []byte { return b.buf }

// runSSHScript runs one ssh and returns its stdout and stderr. Used by the
// one-shot calls (probe, clean), never by the streaming ones.
func (s *Server) runSSHScript(ctx context.Context, destination, script string) (stdout, stderr []byte, err error) {
	cmd, err := s.sshCommand(ctx, destination, script)
	if err != nil {
		return nil, nil, err
	}
	var out, errBuf cappedBuffer
	out.max = 64 << 10
	errBuf.max = stderrTailMax
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return out.Bytes(), errBuf.Bytes(), err
}

// sshFailure turns a non-zero ssh exit into the error a caller sees. ssh's
// stderr is returned verbatim (bounded): an agent reading "Host key
// verification failed" knows the fix, and that knowledge must not be replaced
// by an invented summary.
func sshFailure(destination string, stderr []byte, err error) error {
	tail := strings.TrimSpace(string(stderr))
	if tail != "" {
		return errors.New(tail)
	}
	return fmt.Errorf("ssh %s: %w", destination, err)
}

// RemoteProbe runs one ssh and reports what the daemon needs to decide an
// install: the target's platform, whether the binary is already there, and
// whether the target carries state another install would clobber.
func (s *Server) RemoteProbe(
	ctx context.Context,
	req *connect.Request[executorpb.RemoteProbeRequest],
) (*connect.Response[executorpb.RemoteProbeResponse], error) {
	if err := s.refuseSSHLaunch(); err != nil {
		return nil, err
	}
	target := req.Msg.GetTarget()
	if err := validateRemoteTarget(target); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteBinaryPath(req.Msg.GetBinaryPath(), true); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	addr := s.opts.ConnectAddr
	stdout, stderr, err := s.runSSHScript(ctx, target.GetDestination(),
		remoteProbeScript(target.GetInstance(), req.Msg.GetBinaryPath(), addr))
	if err != nil {
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		return nil, connect.NewError(connect.CodeUnavailable, sshFailure(target.GetDestination(), stderr, err))
	}

	resp, perr := parseRemoteProbeOutput(string(stdout))
	if perr != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remote probe: %w; raw output: %q", perr, string(stdout)))
	}
	if addr != "" && !remoteReachAddrRE.MatchString(addr) {
		// %q keeps the address on one line whatever it holds: the detail is a
		// statement about this launcher's own setting, never shell syntax.
		resp.Reach = "unavailable"
		resp.ReachDetail = fmt.Sprintf("launcher address %q is not a plain host:port", addr)
	}
	return connect.NewResponse(resp), nil
}

// parseRemoteProbeOutput accepts exactly the four lines the probe script always
// prints, or those four plus the two reach lines. Any other shape is a bug or a
// shell that is not POSIX, and the caller sees the raw output rather than a
// half-parsed guess. The four-line shape answers reach=unavailable: the script
// only omits the reach lines when this launcher has no dialable address.
func parseRemoteProbeOutput(out string) (*executorpb.RemoteProbeResponse, error) {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 && len(lines) != 6 {
		return nil, fmt.Errorf("expected four or six lines, got %d", len(lines))
	}
	uname, ok := strings.CutPrefix(lines[0], "uname=")
	if !ok {
		return nil, fmt.Errorf("line 1 is not uname=")
	}
	binary, ok := strings.CutPrefix(lines[1], "binary=")
	if !ok || (binary != "yes" && binary != "no") {
		return nil, fmt.Errorf("line 2 is not binary=yes|no")
	}
	cred, ok := strings.CutPrefix(lines[2], "cred=")
	if !ok || (cred != "yes" && cred != "no") {
		return nil, fmt.Errorf("line 3 is not cred=yes|no")
	}
	marker, ok := strings.CutPrefix(lines[3], "marker=")
	if !ok {
		return nil, fmt.Errorf("line 4 is not marker=")
	}
	resp := &executorpb.RemoteProbeResponse{
		Uname:             strings.TrimSpace(uname),
		BinaryPresent:     binary == "yes",
		CredentialPresent: cred == "yes",
		Marker:            strings.TrimSpace(marker),
		Reach:             "unavailable",
		ReachDetail:       remoteReachNoAddr,
	}
	if len(lines) == 6 {
		reach, ok := strings.CutPrefix(lines[4], "reach=")
		if !ok || (reach != "yes" && reach != "no") {
			return nil, fmt.Errorf("line 5 is not reach=yes|no")
		}
		detail, ok := strings.CutPrefix(lines[5], "reach_detail=")
		if !ok {
			return nil, fmt.Errorf("line 6 is not reach_detail=")
		}
		resp.Reach = reach
		resp.ReachDetail = strings.TrimSpace(detail)
	}
	return resp, nil
}

// RemoteInstall pipes one binary to the target. The first message is the
// header; every later message is a chunk of the binary, forwarded to ssh's
// stdin. The target verifies the sha256 before the final rename, so a torn or
// corrupted stream leaves no file at binary_path.
func (s *Server) RemoteInstall(
	ctx context.Context,
	stream *connect.ClientStream[executorpb.RemoteInstallRequest],
) (*connect.Response[executorpb.RemoteInstallResponse], error) {
	if err := s.refuseSSHLaunch(); err != nil {
		return nil, err
	}
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("remote install: no header: %w", err))
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("remote install: no header"))
	}
	header := stream.Msg().GetHeader()
	if header == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("remote install: first message must be a header"))
	}
	target := header.GetTarget()
	if err := validateRemoteTarget(target); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteBinaryPath(header.GetBinaryPath(), false); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if !remoteSHA256RE.MatchString(header.GetSha256()) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sha256 %q must be 64 hexadecimal characters", header.GetSha256()))
	}

	cmd, err := s.sshCommand(ctx, target.GetDestination(),
		remoteInstallScript(header.GetBinaryPath(), header.GetSha256()))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	var out, errBuf cappedBuffer
	out.max = 64 << 10
	errBuf.max = stderrTailMax
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remote install: stdin pipe: %w", err))
	}
	if err := cmd.Start(); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("remote install: starting ssh: %w", err))
	}

	var writeErr error
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetHeader() != nil {
			// The daemon is the only caller, so a second header is a bug loud
			// enough to name rather than a condition to tolerate.
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("remote install: header sent more than once"))
		}
		if chunk := msg.GetChunk(); len(chunk) > 0 {
			if _, werr := stdin.Write(chunk); werr != nil {
				// ssh is gone (a bad destination, a full disk); Wait below
				// reports why through its stderr.
				slog.Debug("executor: remote install stdin write failed",
					"destination", target.GetDestination(), "error", werr)
				writeErr = werr
				break
			}
		}
	}
	recvErr := stream.Err()
	if writeErr != nil {
		recvErr = nil
	}
	_ = stdin.Close() // EOF ends the remote `cat`
	waitErr := cmd.Wait()

	if recvErr != nil {
		return nil, recvErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		return nil, connect.NewError(connect.CodeUnavailable, sshFailure(target.GetDestination(), errBuf.Bytes(), waitErr))
	}
	return connect.NewResponse(&executorpb.RemoteInstallResponse{}), nil
}

// remoteServeSink serializes sends on one stream. Connect's server-side
// BidiStream is not documented as safe for concurrent Send, and the stdout
// pump, the stderr pump and the exit frame all send.
//
// stderr uses TryLock for the same reason MCPStdio does: a stalled send must
// never let a chatty remote executor deadlock its own session. The ring is the
// record that survives.
type remoteServeSink struct {
	mu     sync.Mutex
	stream *connect.BidiStream[executorpb.RemoteServeRequest, executorpb.RemoteServeResponse]
}

func (s *remoteServeSink) send(msg *executorpb.RemoteServeResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(msg)
}

func (s *remoteServeSink) trySend(msg *executorpb.RemoteServeResponse) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	return s.stream.Send(msg) == nil
}

// RemoteServe runs `executor serve --stdio` on the target over ssh and relays
// its stdio, unparsed. The daemon wraps the stream as a net.Conn and runs the
// ordinary executor upgrade on it, so this side needs no daemon address and
// parses nothing.
//
// One process per stream: stream end (the daemon going away, a pod rolling)
// kills the process group, so no remote executor outlives the session that
// asked for it.
func (s *Server) RemoteServe(
	ctx context.Context,
	stream *connect.BidiStream[executorpb.RemoteServeRequest, executorpb.RemoteServeResponse],
) error {
	if err := s.refuseSSHLaunch(); err != nil {
		return err
	}
	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("remote serve: no start message: %w", err))
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("remote serve: first message must be a start"))
	}
	target := start.GetTarget()
	if err := validateRemoteTarget(target); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteBinaryPath(start.GetBinaryPath(), false); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteRoot(start.GetRoot()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteInstallID(start.GetInstallId()); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	cmd, err := s.sshCommand(ctx, target.GetDestination(),
		remoteServeScript(start.GetBinaryPath(), target.GetInstance(), start.GetRoot(), start.GetInstallId()))
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, err)
	}

	// Hand-built pipes, not cmd.StdinPipe/StdoutPipe/StderrPipe: os/exec closes
	// those on Wait, which discards whatever the process wrote just before it
	// died — and a dying executor's last stderr line is exactly the diagnostic
	// this feature exists to carry (the same reasoning as MCPStdio).
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("remote serve: stdin pipe: %w", err))
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return connect.NewError(connect.CodeInternal, fmt.Errorf("remote serve: stdout pipe: %w", err))
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return connect.NewError(connect.CodeInternal, fmt.Errorf("remote serve: stderr pipe: %w", err))
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	closeAll := func() {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW} {
			f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("remote serve: starting ssh: %w", err))
	}
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	pid := cmd.Process.Pid
	slog.Debug("executor: remote serve started", "destination", target.GetDestination(), "pid", pid)

	// Wait reaps the process exactly once; procMu/reaped make "no signal after
	// the wait" an invariant rather than a timing argument (a pgid belongs to
	// this executor only while its leader is unreaped).
	var (
		procMu sync.Mutex
		reaped bool
	)
	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		procMu.Lock()
		reaped = true
		procMu.Unlock()
		waitCh <- err
	}()

	watcherDone := make(chan struct{})
	var stopOnce sync.Once
	stopWatcher := func() { stopOnce.Do(func() { close(watcherDone) }) }
	defer stopWatcher()
	var killedByUs atomic.Bool
	kill := func(grace time.Duration) {
		procMu.Lock()
		defer procMu.Unlock()
		if reaped {
			return
		}
		killedByUs.Store(true)
		killMCPGroup(pid, grace)
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-watcherDone:
		}
		if ctx.Err() != nil {
			kill(child.TermGrace)
		}
	}()

	sink := &remoteServeSink{stream: stream}
	if err := sink.send(&executorpb.RemoteServeResponse{
		Msg: &executorpb.RemoteServeResponse_Started{Started: &executorpb.RemoteServeStarted{Pid: int32(pid)}},
	}); err != nil {
		kill(child.TermGrace)
		<-waitCh
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		return err
	}

	ring := &mcpStderrRing{}
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		buf := make([]byte, mcpStdoutChunk)
		for {
			n, rerr := stdoutR.Read(buf)
			if n > 0 {
				if err := sink.send(&executorpb.RemoteServeResponse{
					Msg: &executorpb.RemoteServeResponse_Stdout{Stdout: append([]byte(nil), buf[:n]...)},
				}); err != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		defer pumps.Done()
		buf := make([]byte, 16<<10)
		for {
			n, rerr := stderrR.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				ring.append(chunk)
				sink.trySend(&executorpb.RemoteServeResponse{
					Msg: &executorpb.RemoteServeResponse_Stderr{Stderr: chunk},
				})
			}
			if rerr != nil {
				return
			}
		}
	}()

	// The stdin pump runs on its own goroutine: the remote process exiting must
	// end the stream even while the daemon still holds the request side open.
	stdinDone := make(chan error, 1)
	go func() {
		defer stdinW.Close() // a half-close closes the remote process's stdin
		for {
			msg, rerr := stream.Receive()
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					slog.Debug("executor: remote serve stream ended", "destination", target.GetDestination(), "error", rerr)
				}
				stdinDone <- nil
				return
			}
			if msg.GetStart() != nil {
				stdinDone <- connect.NewError(connect.CodeInvalidArgument,
					errors.New("remote serve: start sent more than once"))
				return
			}
			if data := msg.GetStdin(); len(data) > 0 {
				if _, werr := stdinW.Write(data); werr != nil {
					slog.Debug("executor: remote serve stdin write failed",
						"destination", target.GetDestination(), "error", werr)
					stdinDone <- nil
					return
				}
			}
		}
	}()

	var (
		waitErr error
		pumpErr error
	)
	select {
	case waitErr = <-waitCh:
	case pumpErr = <-stdinDone:
		if pumpErr == nil {
			// The daemon half-closed; the process may still be running, and
			// killing it now would truncate one that answers on EOF.
			select {
			case waitErr = <-waitCh:
			case <-ctx.Done():
				kill(child.TermGrace)
				waitErr = <-waitCh
			}
		} else {
			kill(child.TermGrace)
			waitErr = <-waitCh
		}
	case <-ctx.Done():
		kill(child.TermGrace)
		waitErr = <-waitCh
	}

	// The leader is reaped: stop the watcher NOW so a cancellation during the
	// drain cannot signal a pid the kernel is free to reuse.
	stopWatcher()

	// Anything the leader FORKED may still hold the pipes; sweep the group
	// immediately, before the pid can be recycled.
	sweepMCPGroup(pid)

	drained := make(chan struct{})
	go func() { pumps.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(mcpDrainGrace):
	}
	stdoutR.Close()
	stderrR.Close()
	stdinW.Close()

	code := -1
	signal := ""
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			signal = ws.Signal().String()
		}
	}
	tail := ring.tail()
	slog.Info("executor: remote serve exited",
		"destination", target.GetDestination(), "pid", pid,
		"exitCode", code, "signal", signal, "killedByUs", killedByUs.Load(), "error", waitErr)

	if pumpErr != nil {
		return pumpErr
	}
	if err := sink.send(&executorpb.RemoteServeResponse{
		Msg: &executorpb.RemoteServeResponse_Exit{Exit: &executorpb.RemoteServeExit{
			Code:       int32(code),
			Signal:     signal,
			StderrTail: tail,
		}},
	}); err != nil {
		return err
	}
	return nil
}

// RemoteClean removes the target's install marker iff its first line names this
// install. The binary cache is content-addressed and shared, so it stays.
func (s *Server) RemoteClean(
	ctx context.Context,
	req *connect.Request[executorpb.RemoteCleanRequest],
) (*connect.Response[executorpb.RemoteCleanResponse], error) {
	if err := s.refuseSSHLaunch(); err != nil {
		return nil, err
	}
	target := req.Msg.GetTarget()
	if err := validateRemoteTarget(target); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateRemoteInstallID(req.Msg.GetInstallId()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	stdout, stderr, err := s.runSSHScript(ctx, target.GetDestination(),
		remoteCleanScript(target.GetInstance(), req.Msg.GetInstallId(), req.Msg.GetStopProcess()))
	if err != nil {
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		return nil, connect.NewError(connect.CodeUnavailable, sshFailure(target.GetDestination(), stderr, err))
	}
	resp := &executorpb.RemoteCleanResponse{}
	for _, line := range strings.Split(string(stdout), "\n") {
		switch strings.TrimSpace(line) {
		case "removed":
			resp.Removed = true
		case "stopped":
			resp.Stopped = true
		}
	}
	return connect.NewResponse(resp), nil
}
