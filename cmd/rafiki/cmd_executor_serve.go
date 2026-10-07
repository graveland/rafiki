package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executor"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/foothold"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/sandbox"
	"go.graveland.dev/rafiki/pkg/sandboxrelay"
	"go.graveland.dev/rafiki/pkg/version"
)

// This file is the executor. It used to be its own binary, cmd/rafiki-executor,
// and folding it into `rafiki` leaves one client and one server rather than
// three commands to build, ship and version.
//
// Two things made the fold possible and are worth not undoing. First,
// pkg/executor became pgx-free, so `rafiki` linking it does not trip
// TestClientDoesNotLinkPostgres — see pkg/executor/no_postgres_test.go for what
// that took. Second, the operator verbs alongside this (`enroll`, `list`,
// `label`, `disable`, `enable`) only work against the daemon's control socket,
// which an executor host does not have, so shipping them to one is inert rather
// than a widening of authority.

// resolveRoot turns a possibly-empty, possibly-relative --root into an absolute
// path, defaulting to the working directory.
func resolveRoot(root string) (string, error) {
	wd := root
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot get working directory: %w", err)
		}
	}
	if !filepath.IsAbs(wd) {
		abs, err := filepath.Abs(wd)
		if err != nil {
			return "", fmt.Errorf("cannot resolve root: %w", err)
		}
		wd = abs
	}
	return wd, nil
}

// resolveSandboxMountRoots validates repeated --sandbox-mount-root flags: each
// must name an absolute path to an existing directory. A relative path or a
// missing directory is refused up front, naming the flag, because this list is
// what gates which host paths a sandbox container may bind-mount: a typo that
// silently matched nothing would surface only later as a refusal deep in the
// docker proxy guard, far from the flag that caused it.
func resolveSandboxMountRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			return nil, fmt.Errorf("--sandbox-mount-root must not be empty")
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("--sandbox-mount-root %q must be an absolute path", r)
		}
		st, err := os.Stat(r)
		if err != nil {
			return nil, fmt.Errorf("--sandbox-mount-root %q: %w", r, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("--sandbox-mount-root %q is not a directory", r)
		}
		out = append(out, r)
	}
	return out, nil
}

// resolveSandboxRelayDir validates --relay-dir: absolute if set. The relay
// creates the directory if it is missing, so only absoluteness is checked here.
func resolveSandboxRelayDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("--relay-dir %q must be an absolute path", dir)
	}
	return dir, nil
}

// relayDirGOOS is the OS defaultSandboxRelayDir keys off, a package var so a
// test can pretend to be linux or darwin.
var relayDirGOOS = runtime.GOOS

// defaultSandboxRelayDir is the relay directory an executor uses when
// --relay-dir is not given: under paths.RuntimeDir, beside the daemon's own
// sockets. It applies only on Linux, and only when the executor declares a
// docker launcher whose socket is local, because the container's bind mount is
// resolved by the docker host and a path made here does not exist on a remote
// or VM-hosted one — a host-bound relay can never work through a macOS docker
// VM, so on other platforms it stays opt-in. Without a docker proxy the
// executor hosts no sandboxes and binds nothing.
func defaultSandboxRelayDir(proxies map[string]string) string {
	if relayDirGOOS != "linux" {
		return ""
	}
	if !strings.HasPrefix(proxies["docker"], "unix://") {
		return ""
	}
	return filepath.Join(paths.RuntimeDir(), "relay")
}

// relayDirNeedsDaemon refuses --relay-dir when this command has no daemon
// address to relay to. The relay is the sandboxes' only link to the daemon, so
// a relay with nowhere to dial is not a degraded mode but a broken one — better
// to refuse the start than to bind a socket every sandbox connection dies on.
// The address may come from --connect, --connect-socket, or a remote
// $RAFIKI_URL, exactly the sources resolveExecutorConnectFlags accepts.
func relayDirNeedsDaemon(relayDir, connect, connectSocket string) error {
	if relayDir == "" {
		return nil
	}
	if connect != "" || connectSocket != "" || executorEnvURL() != "" {
		return nil
	}
	return fmt.Errorf("--relay-dir requires --connect or --connect-socket (or a remote RAFIKI_URL): " +
		"the relay forwards sandbox connections to the daemon, and there is none to forward to")
}

// relayFootholdNeedsDaemon refuses --relay-foothold-image when this command has
// no daemon address to relay to. It is the foothold sibling of
// relayDirNeedsDaemon and reads the same three address sources. A foothold's
// whole job is to carry sandbox connections to the daemon, so one with nowhere
// to dial is a broken launcher, not a degraded one.
func relayFootholdNeedsDaemon(image, connect, connectSocket string) error {
	if image == "" {
		return nil
	}
	if connect != "" || connectSocket != "" || executorEnvURL() != "" {
		return nil
	}
	return fmt.Errorf("--relay-foothold-image requires --connect or --connect-socket (or a remote RAFIKI_URL): " +
		"the relay forwards sandbox connections to the daemon, and there is none to forward to")
}

// validateRelayFoothold refuses --relay-foothold-image combinations that cannot
// work, before anything binds. Foothold mode is selected ONLY by this flag —
// nothing sniffs a VM — so these checks are the only thing standing between a
// misconfigured launcher and a foothold that can never serve a sandbox. The
// daemon-address requirement lives in relayFootholdNeedsDaemon, which the
// caller checks alongside this.
func validateRelayFoothold(image, relayDir string, relayDirSet bool, proxies map[string]string) error {
	if image == "" {
		return nil
	}
	if relayDirSet && relayDir != "" {
		return fmt.Errorf("--relay-foothold-image and --relay-dir are mutually exclusive")
	}
	if !strings.HasPrefix(proxies["docker"], "unix://") {
		return fmt.Errorf("--relay-foothold-image requires --proxy docker=unix:///path/to/docker.sock")
	}
	return nil
}

// bindFootholdRelay binds the launcher's loopback TCP relay and returns the
// listener and the port the foothold container dials back to. Loopback ONLY: a
// foothold reaches it as host.docker.internal, which is the docker host's
// loopback, and a wildcard bind would expose this unauthenticated relay — the
// daemon-side credential is the gate, not the relay — on every interface.
func bindFootholdRelay() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("bind foothold relay: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, 0, fmt.Errorf("foothold relay listener is not TCP: %T", ln.Addr())
	}
	return ln, addr.Port, nil
}

// footholdEngine builds the Docker Engine client the foothold converges
// through. It dials the local docker unix socket DIRECTLY rather than through
// the executor proxy: the foothold is the launcher's own container, created
// before any sandbox request could flow through the proxy, and the proxy's
// create-body guard is for the sandboxes it forwards, not for this.
func footholdEngine(dockerSocketPath string) *sandbox.Engine {
	return sandbox.NewEngine(&http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", dockerSocketPath)
		},
	})
}

// hostnameOrEmpty returns this machine's hostname, or "" when it cannot be
// read; foothold.Key turns an empty name into "host".
func hostnameOrEmpty() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// relayFootholdImageHelp is the shared help text for --relay-foothold-image,
// used by both `executor serve` and `executor service install` so the two
// cannot drift.
const relayFootholdImageHelp = "image to run as this launcher's foothold: a container inside the docker host's kernel that bridges " +
	"a volume socket to a loopback TCP relay, for docker hosts that cannot bind-mount this host's " +
	"sockets (Docker Desktop or OrbStack; the bridge dials host.docker.internal, which those " +
	"runtimes map to this host's loopback). The image must have rafiki on PATH. Requires --proxy " +
	"docker=unix://… and --connect or --connect-socket; exclusive with --relay-dir"

// executorProfileProxy resolves a reachable LLM proxy URL for daraja-hosted
// children from this executor's OWN profile, best-effort — see
// docs/plans/2026-09-05-daraja-proxy-identity-design.md, Piece 3.
//
// Unlike resolveProfile/mustProfile (profile_glue.go), this does NOT call
// profile.CheckRetiredEnv: rafiki executor serve legitimately derives
// --connect from RAFIKI_URL (see resolveExecutorConnectFlags), and
// CheckRetiredEnv would treat that as a retired client variable and refuse
// EVERY time --launch is combined with a RAFIKI_URL-derived --connect —
// exactly the executors this feature exists for.
//
// It also does not let profile.Resolve's bootstrap fallback run: with no
// profiles.toml on this machine at all, Resolve would silently CREATE one
// pointing at a local daemon (profile.Bootstrap's default) and hand back
// that guess as though it were a proven address — worse than the plain
// fallback to the daemon's own proxy_url this function's caller already has.
// So resolution is attempted only when a manifest already exists.
//
// Any failure (no manifest, no profile selected, unknown profile name) is
// logged and swallowed to "" — never fatal to the executor's actual job of
// hosting tools.
func executorProfileProxy(cmd *cobra.Command) string {
	flag := ""
	if cmd != nil {
		flag, _ = cmd.Flags().GetString("profile")
	}
	env, envSet := os.LookupEnv("RAFIKI_PROFILE")

	explicit := flag != "" || (envSet && env != "")
	if !explicit {
		if _, err := profile.Load(); err != nil {
			return ""
		}
	}

	resolved, err := profile.Resolve(profile.Selection{Flag: flag, Env: env, EnvSet: envSet})
	if err != nil {
		slog.Warn("executor: could not resolve a profile for --launch; hosted children will fall back to the daemon's own proxy address", "error", err)
		return ""
	}
	if resolved.Proxy == "" {
		slog.Warn("executor: resolved profile has no usable proxy address; hosted children will fall back to the daemon's own proxy address", "profile", resolved.Name)
	}
	return resolved.Proxy
}

func executorHandler(srv *executor.Server, admin *executor.AdminServer) http.Handler {
	mux := http.NewServeMux()
	interceptor := executor.NewRPCInterceptor()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv, connect.WithInterceptors(interceptor)))
	// A second service on the SAME mux and the same reverse-dialled connection.
	// The executor stays the executor; this is the machine-admin surface.
	//
	// admin is nil on the surfaces that host nothing by construction — the
	// short-lived enroll executor and a session executor — and those mount no
	// admin service, matching --launch's opt-in rule.
	if admin != nil {
		mux.Handle(admin.Routes())
	}
	return mux
}

// loadExecutorEnv applies the executor's environment files before anything in
// RunE resolves a default from the environment. This is what gives a launchd-
// or systemd-supervised executor the operator's ordinary working environment:
// captureExecutorEnv froze it into the 0600 file at install time, and this is
// where serve picks it up.
//
// Two files, two precedences, applied in order:
//
//   - executor.env (paths.ExecutorEnvFile) fills gaps. A variable already
//     present in the process environment — everything the unit bakes in, HOME
//     and PATH above all — wins over the file.
//   - executor-overrides.env (paths.ExecutorOverridesFile) WINS: every
//     variable it names is set unconditionally, beating the unit and the
//     fill-gaps file alike. It exists for the variables launchd/systemd seed
//     themselves and get wrong — SSH_AUTH_SOCK is the canonical case: launchd
//     injects its own per-session agent socket into every LaunchAgent, so a
//     captured value in executor.env is inert under fill-gaps precedence, and
//     only a file that overrides can point the executor at the agent that
//     actually holds the keys. Hand-maintained; install never writes it.
//
// Scoped to `executor serve` ONLY, and deliberately not loaded at binary
// startup the way the daemon's file is: `rafiki` is also the client, and
// applying executor.env to every client invocation would leak captured
// machine state — a RAFIKI_URL or credential captured on an executor-only box
// — into unrelated commands run against some other instance.
func loadExecutorEnv() {
	path := paths.ExecutorEnvFile()
	applied, warnings, err := paths.LoadEnvFile(path)
	if err != nil {
		slog.Error("could not read the executor environment file; continuing without it",
			"path", path, "error", err)
	}
	for _, w := range warnings {
		slog.Warn("executor environment file", "detail", w)
	}
	if len(applied) > 0 {
		slog.Info("loaded executor environment file", "path", path, "vars", applied)
	}

	// Resolved AFTER the fill-gaps load, so executor.env itself can name the
	// overrides file (a RAFIKI_EXECUTOR_OVERRIDES_FILE entry there is applied
	// by the load above and picked up here); the default is otherwise
	// <config dir>/executor-overrides.env.
	overrides := paths.ExecutorOverridesFile()
	if overrides == path {
		slog.Warn("executor environment overrides file is the environment file itself; ignoring",
			"path", path)
		return
	}
	oapplied, owarnings, oerr := paths.LoadEnvFileOverrides(overrides)
	if oerr != nil {
		slog.Error("could not read the executor environment overrides file; continuing without it",
			"path", overrides, "error", oerr)
	}
	for _, w := range owarnings {
		slog.Warn("executor environment overrides file", "detail", w)
	}
	if len(oapplied) > 0 {
		slog.Info("loaded executor environment overrides file", "path", overrides, "vars", oapplied)
	}
}

// executorPinnedEnv applies the executor's environment files and returns the
// result as THE environment every subprocess this executor ever spawns runs
// under — tools (ToolOpts.Env), language servers, and the daraja and script
// children Launch hosts. Taken immediately after the files are applied and
// before anything else resolves a default from the environment, so the
// overrides file's values keep winning over whatever later mutates the
// process environment, however that mutation happens.
//
// The executor's own credential is dropped from the snapshot. It is hygiene,
// not isolation: a tool subprocess shares this process's /proc, so the value is
// still readable there by anyone who could read this process's environment
// anyway. What the drop buys is that a tool that dumps os.environ (or a shell
// that echoes it) does not hand the executor's durable credential to a child
// that has no business reconnecting as this machine.
func executorPinnedEnv() []string {
	loadExecutorEnv()
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == sandbox.CredentialEnv {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// skillsSyncEnabled resolves the effective --skills-sync value from the flag,
// its environment form, and the --launch list. The environment is read HERE
// rather than as the flag's default for two reasons: flag registration runs
// before loadExecutorEnv has applied executor.env, so a default captured at
// construction time would miss what the file provides; and an explicit
// --skills-sync=false must stay able to switch the feature off on a machine
// whose environment enables it — Flags().Changed tells the explicit spelling
// apart from an untouched default, which the flag's value alone cannot.
//
// --launch claude implies the sync. An executor that hosts claude children
// needs the corpus, and the daemon's pusher keys its eligibility off this
// same Describe — so a claude host with the sync off launches children that
// quietly see no rafiki skills, a failure indistinguishable from "skills are
// broken" and invisible on the daemon side. An explicit --skills-sync=false
// still wins over the implication: refusing the corpus on a claude host is a
// deliberate act and must be spelled as one.
func skillsSyncEnabled(cmd *cobra.Command, flagOn bool, launchKinds []string) bool {
	if cmd.Flags().Changed("skills-sync") {
		return flagOn
	}
	if os.Getenv("RAFIKI_EXECUTOR_SKILLS_SYNC") != "" {
		return true
	}
	return slices.Contains(launchKinds, "claude")
}

// pymodulesSyncEnabled resolves the effective --pymodules-sync value from the
// flag, its environment form, and the --launch list. The environment is read
// HERE rather than as the flag's default for two reasons: flag registration
// runs before loadExecutorEnv has applied executor.env, so a default captured
// at construction time would miss what the file provides; and an explicit
// --pymodules-sync=false must stay able to switch the feature off on a machine
// whose environment enables it — Flags().Changed tells the explicit spelling
// apart from an untouched default, which the flag's value alone cannot.
//
// --launch script implies the sync. An executor that hosts script children
// needs the owner's corpus — a script child's pymodule resolves from THIS
// machine's synced cache, and without the sync it is always empty — and the
// daemon's pusher keys its eligibility off this same Describe, so a script
// host with the sync off launches children that can never resolve their
// script. An explicit --pymodules-sync=false still wins over the implication:
// refusing the corpus on a script host is a deliberate act and must be
// spelled as one. (There is deliberately no implication for any other launch
// kind: pymodules correlate with hosting scripts, nothing else.)
func pymodulesSyncEnabled(cmd *cobra.Command, flagOn bool, launchKinds []string) bool {
	if cmd.Flags().Changed("pymodules-sync") {
		return flagOn
	}
	if os.Getenv("RAFIKI_EXECUTOR_PYMODULES_SYNC") != "" {
		return true
	}
	return slices.Contains(launchKinds, "script")
}

// pymoduleGitSyncEnabled resolves the effective --pymodule-git-sync value
// from the flag and its environment form, with the same flag-vs-env precedence
// as pymodulesSyncEnabled (see that function for why the environment is read
// here rather than as the flag's default).
//
// --launch script implies the sync, like pymodules sync: a script child whose
// repo names a git source resolves against THIS machine's synced checkout, so
// a script host that refuses refreshes hosts only the blob-corpus half of its
// scripts. An explicit --pymodule-git-sync=false still wins: refusing the
// checkouts is a deliberate act (the executor's own git does the cloning, a
// real network side effect an operator may not want) and must be spelled as
// one.
func pymoduleGitSyncEnabled(cmd *cobra.Command, flagOn bool, launchKinds []string) bool {
	if cmd.Flags().Changed("pymodule-git-sync") {
		return flagOn
	}
	if os.Getenv("RAFIKI_EXECUTOR_PYMODULE_GIT_SYNC") != "" {
		return true
	}
	return slices.Contains(launchKinds, "script")
}

// ─── serve ─────────────────────────────────────────────────────────────────────

func newExecutorServeCmd() *cobra.Command {
	var (
		connectAddr        string
		connectSocket      string
		root               string
		concurrency        int
		enrollToken        string
		credentialFile     string
		credential         string
		pinnedFingerprint  string
		serverName         string
		rtkMode            string
		spillDir           string
		jobBudgetMB        int64
		lspConfig          string
		noLSP              bool
		skillsSync         bool
		pymodulesSync      bool
		pymoduleGitSync    bool
		proxyArgs          []string
		launchKinds        []string
		sandboxMountRoots  []string
		relayDir           string
		relayFootholdImage string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run an executor: serve filesystem and shell tools to a daemon",
		Long: `Run an executor.

Two transports, exactly one of which is used:

  --connect         reverse-dial the daemon and serve HTTP/2 on the dialled
                    connection. Required when the daemon cannot reach this host,
                    which is the usual case for a laptop behind NAT. Defaults to
                    the host:port derived from $RAFIKI_URL when neither flag is
                    given.
  --connect-socket  reverse-dial a rafikid on this machine over its executor
                    unix socket, enrolling as a fully rowed pool member.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			pinnedEnv := executorPinnedEnv()

			// The relay has nowhere to forward sandboxes unless this command
			// already knows how to reach the daemon, so refuse before anything
			// else resolves a default.
			if err := relayDirNeedsDaemon(relayDir, connectAddr, connectSocket); err != nil {
				return err
			}

			resolvedConnect, resolvedSocket, err := resolveExecutorConnectFlags(connectAddr, connectSocket)
			if err != nil {
				return err
			}

			mountRoots, err := resolveSandboxMountRoots(sandboxMountRoots)
			if err != nil {
				return err
			}

			wd, err := resolveRoot(root)
			if err != nil {
				return err
			}

			proxies, err := executor.ParseProxyFlags(proxyArgs)
			if err != nil {
				return err
			}

			// Foothold mode is selected only by --relay-foothold-image; these
			// checks refuse combinations that cannot serve a sandbox, before
			// anything binds.
			if err := validateRelayFoothold(relayFootholdImage, relayDir, cmd.Flags().Changed("relay-dir"), proxies); err != nil {
				return err
			}
			if err := relayFootholdNeedsDaemon(relayFootholdImage, connectAddr, connectSocket); err != nil {
				return err
			}

			resolvedRelayDir, err := resolveSandboxRelayDir(relayDir)
			if err != nil {
				return err
			}
			// Foothold mode replaces the host-bind relay dir with a volume the
			// foothold serves; the two are mutually exclusive, so the default dir
			// never applies.
			if relayFootholdImage == "" && !cmd.Flags().Changed("relay-dir") {
				resolvedRelayDir = defaultSandboxRelayDir(proxies)
			}

			// Foothold mode: the relay runs on a loopback TCP listener the foothold
			// container dials back to, and the relay volume is what the sandbox
			// mounts. The foothold is converged once up front — a foothold that
			// cannot start means no sandbox can connect, which is a broken
			// launcher, not a degraded one.
			var (
				relayVolume  string
				beforeCreate func(context.Context) error
				footholdLn   net.Listener
			)
			if relayFootholdImage != "" {
				key := foothold.Key(hostnameOrEmpty())
				ln, port, err := bindFootholdRelay()
				if err != nil {
					return err
				}
				footholdLn = ln
				defer ln.Close()

				dockerSocketPath := strings.TrimPrefix(proxies["docker"], "unix://")
				fh := foothold.New(footholdEngine(dockerSocketPath), key, relayFootholdImage, port)
				if err := fh.Ensure(cmdCtx(cmd)); err != nil {
					return fmt.Errorf("foothold: %w", err)
				}
				relayVolume = foothold.VolumeName(key)
				beforeCreate = fh.Ensure
			}

			srv := executor.NewServer(executor.Options{
				Root:                wd,
				Concurrency:         concurrency,
				Version:             version.String(),
				RTK:                 tools.ParseRTKMode(rtkMode),
				SpillDir:            spillDir,
				JobOutputBudget:     jobBudgetMB << 20,
				LSPConfig:           lspConfig,
				NoLSP:               noLSP,
				SkillsSync:          skillsSyncEnabled(cmd, skillsSync, launchKinds),
				PyModulesSync:       pymodulesSyncEnabled(cmd, pymodulesSync, launchKinds),
				PymoduleGitSync:     pymoduleGitSyncEnabled(cmd, pymoduleGitSync, launchKinds),
				Proxies:             proxies,
				LaunchKinds:         launchKinds,
				SandboxMountRoots:   mountRoots,
				SandboxRelayDir:     resolvedRelayDir,
				SandboxRelayVolume:  relayVolume,
				BeforeSandboxCreate: beforeCreate,
				Env:                 pinnedEnv,
			})
			defer func() { _ = srv.Close() }()

			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve own binary: %w", err)
			}
			// The hosted claude binary is only needed when this executor
			// volunteers to host claude children. --launch script alone must
			// start on a box that has no claude install at all -- a script's
			// interpreter is resolved per launch (the script's venv python
			// when one was built, else python3), not here.
			var childBin string
			if slices.Contains(launchKinds, "claude") {
				childBin, err = exec.LookPath("claude")
				if err != nil {
					return fmt.Errorf("--launch claude given but claude is not on PATH: %w", err)
				}
			}

			var proxyURL string
			if slices.Contains(launchKinds, "claude") {
				proxyURL = executorProfileProxy(cmd)
			}
			// daraja's Relay carries the child's stdio both ways, so its socket
			// must not sit in the world-readable temp dir: on a multi-user
			// executor host any local user could connect and read the
			// conversation's stdout or write its stdin. 0700 under the
			// executor's root keeps it to this process's owner.
			darajaSockets := filepath.Join(wd, "daraja-sockets")
			if err := os.MkdirAll(darajaSockets, 0o700); err != nil {
				return fmt.Errorf("create daraja socket dir: %w", err)
			}
			admin := executor.NewAdminServer(executor.AdminOptions{
				SelfBinary:    self,
				ChildBinary:   childBin,
				LaunchKinds:   launchKinds,
				SocketDir:     darajaSockets,
				ConnectAddr:   resolvedConnect,
				ConnectSocket: resolvedSocket,
				ProxyURL:      proxyURL,
				// The executor's own TLS posture rides every launch: the
				// daraja it spawns, and the per-child socket that daraja
				// serves for a script child, verify the SAME listener this
				// executor dials, by the same pinned fingerprint.
				PinCert:    pinnedFingerprint,
				ServerName: serverName,
				Env:        pinnedEnv,
			})
			defer admin.Close()
			handler := executorHandler(srv, admin)

			// resolveExecutorConnectFlags already guarantees exactly one of
			// these is set, or returned an error above.
			return serveWithRelay(cmdCtx(cmd), resolvedConnect, resolvedSocket, pinnedFingerprint, serverName,
				enrollToken, credential, credentialFile, resolvedRelayDir, footholdLn, handler)
		},
	}

	cmd.Flags().StringVar(&connectAddr, "connect", "", "daemon executor endpoint to dial (host:port; default: derived from $RAFIKI_URL)")
	cmd.Flags().StringVar(&connectSocket, "connect-socket", "",
		"unix socket of a rafikid on this machine to reverse-dial (mutually exclusive with --connect)")
	cmd.Flags().StringVar(&root, "root", "",
		"working directory for this executor's tools (defaults to the current directory). "+
			"NOT a sandbox: an absolute path reaches outside it. What this executor may "+
			"actually reach is its process's filesystem view — the container's mounts, or "+
			"the host user's permissions — and the authoritative description of that lives "+
			"on its database row")
	cmd.Flags().IntVar(&concurrency, "concurrency", 6, "maximum concurrent tool calls")
	cmd.Flags().StringVar(&rtkMode, "rtk", "auto",
		"rewrite known commands through rtk: auto|on|off. The executor operator's choice, "+
			"not the child's — a child cannot see what is installed on this machine")
	cmd.Flags().StringVar(&spillDir, "spill-dir", "",
		"where oversized tool results and background job output are written (defaults to the system temp dir)")
	cmd.Flags().Int64Var(&jobBudgetMB, "job-output-budget-mb", 256,
		"megabytes of background-job output retained per workspace, oldest finished job dropped "+
			"first. Output is kept until the workspace is released; there is no time limit")
	cmd.Flags().StringVar(&lspConfig, "lsp-config", "",
		"path to an lsp.json describing language servers this executor may start (default: auto-detect what is on PATH)")
	cmd.Flags().BoolVar(&noLSP, "no-lsp", false, "disable language servers on this executor entirely")
	cmd.Flags().BoolVar(&skillsSync, "skills-sync", false,
		"accept the daemon's skill corpus into this machine's Claude skills directory "+
			"(RAFIKI_EXECUTOR_SKILLS_SYNC enables it from a service unit). "+
			"Implied by --launch claude; --skills-sync=false refuses it explicitly")
	cmd.Flags().BoolVar(&pymodulesSync, "pymodules-sync", false,
		"accept the daemon's owner-scoped pymodule corpus into this machine's disposable cache "+
			"directory (RAFIKI_EXECUTOR_PYMODULES_SYNC enables it from a service unit)")
	cmd.Flags().BoolVar(&pymoduleGitSync, "pymodule-git-sync", false,
		"accept refreshes of this executor's owner's registered git pymodule sources into this "+
			"machine's disposable pymodule-repo cache, cloning or fetching them with this machine's "+
			"own git (RAFIKI_EXECUTOR_PYMODULE_GIT_SYNC enables it from a service unit)")
	cmd.Flags().StringArrayVar(&proxyArgs, "proxy", nil, "LLM endpoint this executor will forward to, name=base_url (repeatable)")
	cmd.Flags().StringArrayVar(&launchKinds, "launch", nil,
		"child protocol this executor will host for the daemon: claude or script (repeatable). "+
			"Opt-in: with no --launch this executor hosts nothing. --launch claude also accepts the "+
			"skill corpus unless --skills-sync=false. --launch script implies the owner's pymodule "+
			"corpus sync and git-source refreshes (unless --pymodules-sync=false / "+
			"--pymodule-git-sync=false): a script child resolves its pymodule from THIS machine's "+
			"synced cache")
	cmd.Flags().StringVar(&enrollToken, "enroll-token", os.Getenv("RAFIKI_ENROLL_TOKEN"),
		"one-time enrollment token, required on first --connect")
	cmd.Flags().StringVar(&credentialFile, "credential-file", "",
		"where the durable credential is stored after enrollment (default: <data dir>/executor.cred)")
	cmd.Flags().StringVar(&credential, "credential", os.Getenv("RAFIKI_EXECUTOR_CREDENTIAL"),
		"use this credential directly and write nothing to disk. For stateless deployments that inject it from a secret store; skips enrollment entirely")
	cmd.Flags().StringVar(&pinnedFingerprint, "pin-cert", "",
		"SHA-256 fingerprint of the daemon's leaf certificate. Pins the leaf instead of verifying against system roots — use for a self-signed or internal-CA daemon")
	cmd.Flags().StringVar(&serverName, "server-name", "",
		"TLS server name (SNI) to present, when it differs from the host in --connect. Needed when dialling an IP or a node port while the certificate names a hostname")
	cmd.Flags().StringArrayVar(&sandboxMountRoots, "sandbox-mount-root", nil,
		"host directory a sandboxed child's container may bind-mount from (repeatable; absolute, must exist). "+
			"A bind outside every root and the --relay-dir is refused. With no root, no bind is permitted")
	cmd.Flags().StringVar(&relayDir, "relay-dir", "",
		"absolute directory under which to expose the daemon to sandboxes over a unix socket "+
			"(created if missing). Requires --connect or --connect-socket. Defaults on Linux to a "+
			"directory under the runtime dir when --proxy docker= names a local unix socket; on "+
			"other platforms the relay dir is opt-in. "+
			"--relay-dir= (empty) disables it")
	cmd.Flags().StringVar(&relayFootholdImage, "relay-foothold-image", "", relayFootholdImageHelp)

	return cmd
}

// executorConnectOptions builds the ConnectOptions this command dials the
// daemon with. It is shared by the executor's own reverse dial and the relay's
// per-connection dial, so both reach the same listener the same way; the relay
// only ever calls DialDaemon on it, which reads the dial fields and ignores the
// handler and credential.
func executorConnectOptions(addr, socketPath, pinCert, serverName, enrollToken, credential, credentialFile string, handler http.Handler) execpool.ConnectOptions {
	// The credential-file default deliberately does NOT sit under --root. It
	// used to (`<root>/.rafiki-executor-credential`), which put the executor's
	// own credential inside the very directory tree its file tools operate on —
	// and native executors have no path scoping, by design. An agent running on
	// the executor could read it and reconnect as that machine, including after
	// an operator disabled it. paths.DataDir is for state that must survive a
	// reboot, which a credential must: losing it means re-enrolling by hand.
	credFile := credentialFile
	if credFile == "" && credential == "" {
		credFile = filepath.Join(paths.DataDir(), "executor.cred")
	}
	return execpool.ConnectOptions{
		Addr:           addr,
		SocketPath:     socketPath,
		PinCert:        pinCert,
		ServerName:     serverName,
		EnrollToken:    enrollToken,
		Credential:     credential,
		CredentialFile: credFile,
		SelfReported: map[string]string{
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"version": version.String(),
		},
		Handler: handler,
	}
}

func serveReverseDial(ctx context.Context, opts execpool.ConnectOptions) error {
	if err := execpool.Connect(ctx, opts); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// serveWithRelay runs the executor's reverse dial for the life of the command
// and, when a relay was given, a sandbox relay alongside it. The relay is
// either the host-bind unix relay under relayDir or — foothold mode — the
// already-bound loopback TCP listener relayLn the foothold container dials
// back to; exactly one is set.
//
// The relay is the sandboxes' only link to the daemon, so it is not a
// best-effort sidecar: a Serve error (other than the clean return of a
// cancelled context) is logged and ends the command nonzero, and a relay that
// stops takes the reverse dial down with it rather than leaving a socket that
// accepts connections into nothing. The caller owns relayLn and closes it on
// every return path.
func serveWithRelay(ctx context.Context, addr, socketPath, pinCert, serverName, enrollToken, credential, credentialFile, relayDir string, relayLn net.Listener, handler http.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	connErr := make(chan error, 1)
	go func() {
		connErr <- serveReverseDial(ctx, executorConnectOptions(addr, socketPath, pinCert, serverName, enrollToken, credential, credentialFile, handler))
	}()

	if relayDir == "" && relayLn == nil {
		return <-connErr
	}

	// The relay dials the SAME daemon the reverse dial does, with the same TLS
	// posture; only the per-connection fields matter here.
	dialOpts := executorConnectOptions(addr, socketPath, pinCert, serverName, "", "", "", nil)
	dial := func(dctx context.Context) (net.Conn, error) {
		return execpool.DialDaemon(dctx, dialOpts)
	}

	relayErr := make(chan error, 1)
	if relayLn != nil {
		// Foothold mode: the relay runs on the loopback TCP listener the
		// foothold container dials back to, not a host-bind unix socket. The
		// listener is owned by the caller, which closes it on every return path;
		// ServeListener also closes it on ctx cancel.
		go func() {
			relayErr <- sandboxrelay.ServeListener(ctx, relayLn, dial)
		}()
	} else {
		go func() {
			relayErr <- sandboxrelay.Serve(ctx, relayDir, dial)
		}()
	}

	select {
	case err := <-relayErr:
		cancel()
		if err != nil {
			slog.Error("sandbox relay stopped; ending executor serve", "error", err)
			return fmt.Errorf("sandbox relay: %w", err)
		}
		return <-connErr
	case err := <-connErr:
		cancel()
		return err
	}
}
