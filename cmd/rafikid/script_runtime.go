// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childsock"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/gitpymodules"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/pymodules"
)

// scriptChildren kinds, hosted locally. Wave 4 moves hosting onto executors
// via daraja; until then every script child forks on the daemon's own host,
// which is why checkKindNarrowing refuses a script spawn under an
// executor-granted parent (the same refusal the claude local fallback gets).
//
// The runtime:
//
//   - materializes the pymodule the way pymodule_run consumes it — repo
//     "local" resolves against the OWNER's pymodule store, one directory per
//     module under scriptHostDir, the script's own directory staying off
//     PYTHONPATH because it is sys.path[0] (pymodule_run's exact layout,
//     minus venvs, which only exist in an executor's synced cache);
//   - serves the per-child Connect socket (pkg/childsock) inside the same
//     directory, reverse-proxying to the proxy face's Connect route with the
//     child's own per-child secret injected — the process never sees a
//     token, only the socket path, which reaches it as RAFIKI_CHILD_CONNECT;
//   - runs the script in its own process group with the full daemon
//     environment minus every RAFIKI_/ANTHROPIC_/OPENROUTER_ variable (so no
//     credential, no retired client socket, no child-id global leaks into a
//     process the operator did not give them to) plus the one variable
//     above;
//   - feeds stdout/stderr through the ordinary child plumbing, so logs,
//     tail, watch, the cockpit and the log dumps work unchanged.

// scriptHostRoot is the per-daemon directory script children materialize
// into: <stateDir>/script-host/<childID>/ holds both the materialized
// pymodule tree and the child's connect.sock. The integration suite
// reconstructs the path, so it must move with childsock.SocketName.
func scriptHostDir(stateDir, childID string) string {
	return filepath.Join(stateDir, "script-host", childID)
}

// scriptStderrTailBytes caps the stderr tail a failed script's settle
// fragment carries when it never called SetResult.
const scriptStderrTailBytes = 4 * 1024

// envStripPrefixes are the variable-name prefixes a script child's
// environment is stripped of. The daemon's own RAFIKI_* variables name
// daemon internals (the control socket, the profile, the test DSN); the
// ANTHROPIC_*/OPENROUTER_* families carry LLM credentials the child has no
// business holding. RAFIKI_CHILD_CONNECT is the deliberate exception — the
// one channel the child is given — and is appended after the strip.
var envStripPrefixes = []string{"RAFIKI_", "ANTHROPIC_", "OPENROUTER_"}

// scriptEnvStripped reports whether env entry e carries a stripped prefix.
func scriptEnvStripped(e string) bool {
	k := e
	if i := strings.IndexByte(e, '='); i >= 0 {
		k = e[:i]
	}
	for _, p := range envStripPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// scriptChildEnv builds a script child's process environment:
//
//   - the FULL daemon environment, minus every RAFIKI_*/ANTHROPIC_*/
//     OPENROUTER_* variable — PATH, HOME and everything else the daemon
//     inherited survive (envWithPythonPath's rule: a script that shells out
//     or reads the caller's environment must not lose them), but the
//     daemon's control-plane variables do not;
//   - then the caller's forwarded env (req.Env, the --forward-env channel),
//     stripped by the same rule, so a forwarded ANTHROPIC_API_KEY cannot
//     resurrect what the strip removed;
//   - then PYTHONPATH, recomputed by the materialization (never inherited:
//     the key must appear exactly once);
//   - then RAFIKI_CHILD_CONNECT, the child's control socket path.
//
// The order matters: the caller's entries override the daemon's inherited
// ones, and the daemon's computed PYTHONPATH and RAFIKI_CHILD_CONNECT
// override everything, since a child cannot opt out of its control channel.
func scriptChildEnv(environ []string, forwarded map[string]string, pythonPath, socketPath string) []string {
	env := make([]string, 0, len(environ)+len(forwarded)+2)
	for _, e := range environ {
		if scriptEnvStripped(e) {
			continue
		}
		env = append(env, e)
	}
	for k, v := range forwarded {
		if k == "" || scriptEnvStripped(k+"="+v) {
			continue
		}
		env = append(env, k+"="+v)
	}
	if pythonPath != "" {
		env = append(env, "PYTHONPATH="+pythonPath)
	}
	return append(env, "RAFIKI_CHILD_CONNECT="+socketPath)
}

// scriptMaterialization is what the runner execs: the interpreter, the argv
// tail (script path then the spec's args), and the PYTHONPATH the run needs.
type scriptMaterialization struct {
	interpreter string
	argv        []string
	pythonPath  string
}

// scriptInterpreter mirrors pymodule_run's Materialize: the operator's
// RAFIKI_PYMODULE_PYTHON, else "python3". Read from the daemon's own
// environment at spawn time — the child's environment deliberately carries
// no RAFIKI_* variable.
func scriptInterpreter() string {
	if p := os.Getenv("RAFIKI_PYMODULE_PYTHON"); p != "" {
		return p
	}
	return "python3"
}

// scriptRunner builds the local Runner for a kind=script child: materialize
// the pymodule, open the per-child Connect socket, exec the interpreter. The
// returned runner wraps child.NewProcessRunner so the process gets exactly
// the subprocess semantics every other child gets — its own process group,
// process-group SIGTERM/SIGKILL, the Wait contract — and closes the socket
// when the child exits.
func (c *Controller) scriptRunner(req protocol.SpawnRequest, childID, ownerUserID string) (child.Runner, error) {
	if req.Script == nil {
		return nil, errors.New("script kind requires a script spec (repo + script)")
	}
	// The per-child socket reverse-proxies to the proxy face's Connect
	// route, which is where the child credential is resolved. A daemon with
	// no proxy face has no route to proxy to — refuse the spawn rather than
	// start a child that cannot reach its daemon.
	if c.proxyURL == "" {
		return nil, errors.New("script children need the daemon's proxy face (the Connect control route): none is running")
	}
	target, err := url.Parse(c.proxyURL)
	if err != nil {
		return nil, fmt.Errorf("script child: proxy face URL: %w", err)
	}

	mat, err := c.materializeScript(req.Script, childID, ownerUserID)
	if err != nil {
		return nil, err
	}

	// The child secret rides the proxy injection, never the environment or
	// argv. It is the same per-child secret the MCP face authenticates — one
	// mint, one resolver (ChildForMCPToken) — and it dies with the child
	// (handleChildExit's forgetMCPToken).
	secret := c.mintMCPToken(childID)
	c.sweepMCPTokensIfDue()

	dir := scriptHostDir(c.stateDir, childID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("script child: create host directory: %w", err)
	}
	sock, err := childsock.Serve(c.baseCtx, dir, target, secret)
	if err != nil {
		return nil, fmt.Errorf("script child: per-child socket: %w", err)
	}

	inner, err := child.NewProcessRunner(child.SpawnSpec{
		ChildID: childID,
		Cwd:     req.Cwd,
		// Argv[0] of a SpawnSpec is PiBinary, so the script path is Argv[1].
		PiBinary: mat.interpreter,
		Argv:     mat.argv,
		Env:      scriptChildEnv(os.Environ(), req.Env, mat.pythonPath, childsock.SocketPath(dir)),
		// The runner builds the COMPLETE environment (the full daemon env
		// minus the credential prefixes); nothing is inherited on top of it.
		EnvOverride: true,
	})
	if err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("script child: %w", err)
	}
	return &scriptRunner{inner: inner, sock: sock}, nil
}

// scriptRunner adapts child.NewProcessRunner to the child.Runner seam,
// adding the per-child socket's lifecycle: closed and unlinked when the
// child is reaped — and on a failed Start, so a spawn that never ran does
// not leave a listening socket behind.
type scriptRunner struct {
	inner child.Runner
	sock  *childsock.Server
}

var _ child.Runner = (*scriptRunner)(nil)

func (r *scriptRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdin, stdout, stderr, err := r.inner.Start()
	if err != nil {
		_ = r.sock.Close()
		return nil, nil, nil, err
	}
	return stdin, stdout, stderr, nil
}

// Wait reaps the process and then closes the per-child socket: the child has
// exited, so its control channel must stop serving and its socket file must
// go away.
func (r *scriptRunner) Wait() (int, string) {
	code, sig := r.inner.Wait()
	_ = r.sock.Close()
	return code, sig
}

func (r *scriptRunner) PID() int         { return r.inner.PID() }
func (r *scriptRunner) Terminate() error { return r.inner.Terminate() }
func (r *scriptRunner) Kill() error      { return r.inner.Kill() }
func (r *scriptRunner) Interrupt() error { return r.inner.Interrupt() }

// materializeScript writes the script and its named modules into the child's
// host directory the way pymodule_run consumes them: one directory per name
// (script at <dir>/<script>/<script>.py — sys.path[0] for the run — each
// module's directory joining PYTHONPATH in call order). repo "local"
// resolves against the OWNER's own saved modules; a git source names an
// executor-hosted checkout, which a locally hosted child cannot reach.
func (c *Controller) materializeScript(spec *protocol.ScriptSpec, childID, ownerUserID string) (scriptMaterialization, error) {
	if err := validateScriptSpecNames(spec); err != nil {
		return scriptMaterialization{}, err
	}
	if spec.Repo != pymodules.LocalRepo {
		// A git source's content lives in an EXECUTOR's synced checkout
		// (pkg/executor's SyncPyModuleGitSource); the daemon has no clone of
		// its own. Wave 4 hosts scripts on executors, where that cache
		// exists. Refuse rather than half-simulate it locally.
		return scriptMaterialization{}, fmt.Errorf(
			"script child: repo %q is a git source: locally hosted children run only the owner's own saved modules (repo %q); host it on an executor or use repo %q",
			spec.Repo, pymodules.LocalRepo, pymodules.LocalRepo)
	}
	if c.pymoduleStore == nil {
		return scriptMaterialization{}, errors.New("script children need the pymodule store: this daemon has no database")
	}

	dir := scriptHostDir(c.stateDir, childID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return scriptMaterialization{}, fmt.Errorf("script child: create host directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(c.baseCtx, 10*time.Second)
	defer cancel()

	writeOut := func(name string) (mdir string, hasRequirements bool, err error) {
		rec, getErr := c.pymoduleStore.Get(ctx, ownerUserID, name)
		if getErr != nil {
			if errors.Is(getErr, pymodules.ErrNotFound) {
				return "", false, fmt.Errorf("script child: module %q is not saved for this owner (save it with pymodule_put first)", name)
			}
			return "", false, fmt.Errorf("script child: fetch module %q: %w", name, getErr)
		}
		mdir = filepath.Join(dir, name)
		if err := os.MkdirAll(mdir, 0o700); err != nil {
			return "", false, fmt.Errorf("script child: create module directory %s: %w", mdir, err)
		}
		if err := os.WriteFile(filepath.Join(mdir, name+".py"), []byte(rec.Code), 0o600); err != nil {
			return "", false, fmt.Errorf("script child: write module %q: %w", name, err)
		}
		return mdir, len(pymodules.ParseRequirements(rec.Code)) > 0, nil
	}

	scriptDir, scriptReqs, err := writeOut(spec.Script)
	if err != nil {
		return scriptMaterialization{}, err
	}
	ppEntries := make([]string, 0, len(spec.Modules))
	anyModuleReqs := false
	for _, m := range spec.Modules {
		mdir, modReqs, mErr := writeOut(m)
		if mErr != nil {
			return scriptMaterialization{}, mErr
		}
		if modReqs {
			anyModuleReqs = true
		}
		ppEntries = append(ppEntries, mdir)
	}
	if scriptReqs || anyModuleReqs {
		// pymoduleVenvReadiness's rule, adapted to the local host: a
		// requirements block needs a dependency venv, and venvs exist only in
		// an executor's synced cache. Refuse rather than run against missing
		// imports.
		return scriptMaterialization{}, fmt.Errorf(
			"script child: script or modules declare dependencies (# pymodule-requirements:) and locally hosted children have no venv; host the script on an executor or drop the requirements block")
	}

	return scriptMaterialization{
		interpreter: scriptInterpreter(),
		argv:        append([]string{filepath.Join(scriptDir, spec.Script+".py")}, spec.Args...),
		pythonPath:  strings.Join(ppEntries, string(os.PathListSeparator)),
	}, nil
}

// validateScriptSpecNames re-checks every name that becomes a path segment or
// an import, exactly like pymodule_run: each consumption point validates
// independently, never trusting an earlier check.
func validateScriptSpecNames(spec *protocol.ScriptSpec) error {
	if spec.Repo == "" {
		return errors.New(`script child: repo is required ("local" for the owner's own saved modules, or a registered git source's name)`)
	}
	if spec.Repo != pymodules.LocalRepo {
		if err := gitpymodules.ValidateName(spec.Repo); err != nil {
			return fmt.Errorf("script child: repo %q: %w", spec.Repo, err)
		}
	}
	if err := pymodules.ValidName(spec.Script); err != nil {
		return fmt.Errorf("script child: script: %w", err)
	}
	for _, m := range spec.Modules {
		if err := pymodules.ValidName(m); err != nil {
			return fmt.Errorf("script child: module %q: %w", m, err)
		}
	}
	return nil
}

// validateScriptSpawn refuses a kind=script request that names any
// fundi/claude-only field. A script child has no engine, no model, no tools,
// no session and no pre-fill; a request carrying one of those is refused, in
// table order, so a request that sets several reports the first. Called
// after applyPreset (which resolves the kind and has already refused
// preset-level contradictions), before anything is minted.
func validateScriptSpawn(req protocol.SpawnRequest) error {
	kind := req.Kind
	if kind == "" {
		kind = protocol.KindFundi
	}
	if kind != protocol.KindScript {
		return nil
	}
	for _, bad := range []struct {
		field string
		set   bool
	}{
		{"model", req.Model != ""},
		{"provider", req.Provider != ""},
		{"api_key", req.APIKey != ""},
		{"thinking", req.Thinking != ""},
		{"tools", req.Tools != ""},
		{"tools", req.NoTools},
		{"tools", req.NoBuiltinTools},
		{"extensions", len(req.Extensions) > 0},
		{"extensions", req.NoExtensions},
		{"skills", len(req.Skills) > 0},
		{"skills", req.NoSkills},
		{"skills_dirs", len(req.SkillsDirs) > 0},
		{"mcp_config", req.MCPConfig != ""},
		{"mcp_servers", len(req.MCPServers) > 0},
		{"mcp_servers", req.NoMCP},
		{"prompt_templates", len(req.PromptTemplates) > 0},
		{"prompt_templates", req.NoPromptTemplates},
		{"themes", len(req.Themes) > 0},
		{"themes", req.NoThemes},
		{"context_files", req.NoContextFiles},
		{"system_prompt", req.SystemPrompt != ""},
		{"append_system_prompt", req.AppendSystemPrompt != ""},
		{"config_dir", req.ConfigDir != ""},
		{"passthrough_auth", req.PassthroughAuth != ""},
		{"record_requests", req.RecordRequests},
		{"verbose", req.Verbose},
		{"no_session", req.NoSession},
		{"session_dir", req.SessionDir != ""},
		{"resume_session", req.ResumeSession != ""},
		{"fork_session", req.ForkSession != ""},
		{"pi_binary", req.PiBinary != ""},
		{"extra_args", len(req.ExtraArgs) > 0},
		{"resumed_from_session", req.ResumedFromSession != ""},
		{"env_override", req.EnvOverride},
	} {
		if bad.set {
			return &control.ControllerError{
				Code:    protocol.ErrInvalidArgs,
				Message: fmt.Sprintf("field %q does not apply to kind %q", bad.field, protocol.KindScript),
			}
		}
	}
	// prefill is refused by validatePrefill for every non-fundi kind; the
	// spec check here is the script-specific shape the runner needs.
	if req.Script == nil {
		return &control.ControllerError{
			Code:    protocol.ErrInvalidArgs,
			Message: "script kind requires a script spec: repo (\"local\" or a git source's name) and the script's pymodule name",
		}
	}
	if err := validateScriptSpecNames(req.Script); err != nil {
		return err
	}
	return nil
}

// scriptSettleFor computes the settle notification for a script child that
// exited: exit 0 settles done, anything else failed, and the stderr tail is
// what the settle fragment carries when the script never called SetResult.
// A signalled child has ExitCode 0 with Signal set (pkg/child's Wait
// contract), so "exit 0" is "no signal AND code 0".
func scriptSettleFor(kind string, res child.ShutdownResult, stderr []byte) (reason, tail string) {
	if kind != protocol.KindScript {
		return "exited", ""
	}
	if res.Signal == "" && res.ExitCode == 0 {
		return "done", ""
	}
	return "failed", lastStderrTail(stderr, scriptStderrTailBytes)
}

// lastStderrTail returns the last cap bytes of stderr, or the whole buffer
// when it is smaller. The tail is verbatim: it is quoted, not interpreted.
func lastStderrTail(stderr []byte, cap int) string {
	if len(stderr) == 0 {
		return ""
	}
	if len(stderr) > cap {
		stderr = stderr[len(stderr)-cap:]
	}
	return string(stderr)
}
