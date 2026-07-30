# Phase 1a: In-Process Agent Execution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the `agent` kind as a goroutine inside `fundid` instead of as an `exec`'d subprocess, with the frame protocol, bus, ring, and state machine completely unchanged.

**Architecture:** `internal/child` gains an exported `Runner` seam covering the five places `Child` touches `exec.Cmd` — start, PID, reap, signal, kill. `processRunner` is today's behaviour moved behind it. A new `internal/inproc` package implements `Runner` by driving an `agent.Engine` over a pair of `io.Pipe`s, so the daemon's `supervise`/`readStdout` loops are byte-identical for both. The runner is **injected** via `SpawnSpec` rather than selected inside `internal/child`, because `internal/agent` already imports `internal/child` (`emit.go:14`) and the reverse import would be a cycle.

Per-child configuration is *not* re-mapped by hand. The daemon keeps building an argv with `buildAgentArgv` and then parses it with the existing pure `parseAgentFlags`, converting the result to `agent.RuntimeOptions`. One mapping, so `req.ExtraArgs` and its documented last-flag-wins behaviour keep working, and no field can be silently dropped.

**Tech Stack:** Go 1.26.4, stdlib `testing` only (no testify), `io.Pipe` for in-memory streams, `pgxpool` for the shared pool.

## Global Constraints

- Go 1.26.4. Module `git.graveland.dev/brent/fundi`.
- Tests use the **stdlib `testing` package only**. There is no testify in `go.mod`; do not add it. Existing tests fake a child with a bash script (see `internal/child/child_test.go`); follow that idiom.
- `go vet ./...` must report zero findings.
- `golangci-lint run ./... --max-same-issues=0 --max-issues-per-linter=0` must report zero findings. The default `--max-same-issues=3` truncates non-deterministically — always pass both flags.
- `GOOS=linux go build ./...` must succeed. It is a supported target and it has been broken before.
- **`go test -run` with no matching test prints `ok` and exits 0.** Every "verify it fails" step must use `-v` and confirm a `=== RUN` line appears. A step printing `ok` with no `=== RUN` verified nothing.
- **Non-verbose `go test` never prints `--- SKIP`.** Use `-v` when reporting skip counts.
- **`$?` after a pipeline is the last element's status.** Never `go test ./... | tail; echo $?`. Redirect to a file, then check the file.
- Never swallow an error. No empty `if err != nil {}`, no ignored return from a fallible call. Log it or return it.
- Do **not** add Co-Authored-By trailers to commits.
- `git add` specific files by name. Never `git add -A` or `git add .`. Run `git diff --cached --stat` before every commit and confirm only intended files are staged.
- Work in a git worktree, not the shared checkout — concurrent sessions edit the same tree. Create it with the `superpowers:using-git-worktrees` skill before Task 1.
- The frame protocol does not change. A diff to `protocol/` or to the `child.Pi*` event constructors means something has gone wrong.

## File Structure

**Create:**

| file | responsibility |
|---|---|
| `internal/child/runner.go` | The exported `Runner` interface and `processRunner`, today's `exec.Cmd` behaviour moved verbatim behind it. |
| `internal/child/runner_test.go` | A fake-`Runner` test proving injection is honoured and no `exec` happens. |
| `internal/agent/runtime.go` | `RuntimeOptions` + `BuildRuntime`: assembles context files, skills, tool registry, MCP, `Config`, and the `Engine` from **explicit** inputs — no `os.Getwd`, no signal handlers. Shared by the CLI and the daemon. |
| `internal/agent/runtime_test.go` | Proves `BuildRuntime` honours an explicit cwd and never opens a pool. |
| `internal/inproc/runner.go` | Implements `child.Runner` by driving an `agent.Engine` over `io.Pipe`s, with panic containment. Imports `internal/agent` and `internal/child`; imported only by `cmd/fundid`. |
| `internal/inproc/runner_test.go` | Fake-turn end-to-end test through the runner, plus panic containment. |
| `cmd/fundid/agent_runtime.go` | `agentFlags.toRuntimeOptions(cwd string)` — the single conversion from parsed flags to `RuntimeOptions`. |
| `cmd/fundid/agent_runtime_test.go` | Round-trips `buildAgentArgv` output through `parseAgentFlags` and asserts every field survives. |

**Modify:**

| file | change |
|---|---|
| `internal/child/child.go` | `Child` holds a `Runner` instead of `*exec.Cmd`. |
| `cmd/fundid/agent.go` | Use `agent.BuildRuntime` and `toRuntimeOptions`; open a pool for the standalone path. |
| `internal/agent/config.go` | `BuildEngine` takes an injected pool instead of creating one. |
| `cmd/fundid/controller.go` | Own a pool; set `spec.Runner` for the agent kind at **all three** `SpawnSpec` sites. |
| `cmd/fundid/main.go` | Open the pool, pass the daemon context and pool to `NewController`, close the pool last. |

---

### Task 0: Extract `internal/skills` to break the tools↔agent cycle

**Why this task exists.** Task 1 puts `BuildRuntime` in `package agent` and has it call
`tools.NewRegistry`. That cannot compile: `internal/agent/tools/skill.go` imports
`internal/agent` for `SkillMeta`, so `agent` importing `tools` is a cycle. The architecture is
deliberate and documented at `internal/agent/config.go:42-49` — `Config.Tools` is typed as
rafiki's `agentloop.ToolSet` interface precisely to avoid it, and the comment concludes
"cmd/fundid is where both sides meet."

Rather than work around it, fix the cause. `internal/agent/skills.go` imports **only stdlib and
`gopkg.in/yaml.v3`** — it has no dependency on the rest of `package agent`. It is already a leaf
living in a non-leaf package, and that is what drags the whole runtime into `tools`. Moving it
out breaks the cycle at its source and lets `internal/agent` assemble a whole conversation
(skills, tools, system prompt) — which is what a runtime package is for.

`internal/agent/tools` does **not** move. Once the dependency points one way, a parent importing
its own subpackage is ordinary Go.

**Files:**
- Create: `internal/skills/skills.go` — moved verbatim from `internal/agent/skills.go`, package renamed
- Create: `internal/skills/skills_test.go` — moved from `internal/agent/skills_test.go`
- Delete: `internal/agent/skills.go`, `internal/agent/skills_test.go`
- Modify: `internal/agent/config.go`, `internal/agent/sysprompt.go`, `internal/agent/tools/skill.go`, `cmd/fundid/agent.go`, and the tests referencing the moved identifiers (`internal/agent/sysprompt_test.go`, `internal/agent/tools/skill_test.go`, `cmd/fundid/agent_test.go`)

**Interfaces:**
- Consumes: nothing.
- Produces: package `skills` at `git.graveland.dev/brent/fundi/internal/skills`, exporting
  `SkillMeta`, `DiscoverSkills(dirs []string, only []string) ([]SkillMeta, error)`,
  `SkillsInventory(skills []SkillMeta) string`, and `SkillBody(path string) (string, error)` —
  same signatures as today, only the package qualifier changes. Task 1 imports this.

- [ ] **Step 1: Confirm the cycle exists before changing anything**

```bash
cat > /tmp/cycle_probe.go <<'EOF'
package agent

import _ "git.graveland.dev/brent/fundi/internal/agent/tools"
EOF
cp /tmp/cycle_probe.go internal/agent/zz_cycle_probe.go
go build ./internal/agent/ > /tmp/cycle.log 2>&1; echo "exit=$?"; cat /tmp/cycle.log
rm internal/agent/zz_cycle_probe.go
```

Expected: a non-zero exit and `import cycle not allowed` naming `agent` → `tools` → `agent`.
This is the failing state Task 0 fixes; record the exact output in your report.

- [ ] **Step 2: Move the package**

```bash
mkdir -p internal/skills
git mv internal/agent/skills.go internal/skills/skills.go
git mv internal/agent/skills_test.go internal/skills/skills_test.go
```

Change the package clause in both files from `package agent` to `package skills`. Change nothing
else in them — no renames, no signature changes, no new exports. `SkillMeta` stays `SkillMeta`
(it becomes `skills.SkillMeta` at call sites); do **not** rename it to `skills.Meta`, because
Tasks 1-5 and the existing call sites all use the current name.

- [ ] **Step 3: Update every reference**

Add `"git.graveland.dev/brent/fundi/internal/skills"` to the imports of each file below and
qualify the moved identifiers:

- `internal/agent/config.go` — `SkillsInventory`, `SkillMeta`
- `internal/agent/sysprompt.go` and `internal/agent/sysprompt_test.go`
- `internal/agent/tools/skill.go` and `internal/agent/tools/skill_test.go` — **this file's
  `internal/agent` import must be removed entirely**; it was the cycle's second leg. If
  `skill.go` imports `internal/agent` for anything besides the skills identifiers, stop and
  report `BLOCKED` with what else it needs.
- `cmd/fundid/agent.go` and `cmd/fundid/agent_test.go`

Find them all rather than trusting this list:

```bash
grep -rln 'SkillMeta\|DiscoverSkills\|SkillsInventory\|SkillBody' --include='*.go' .
```

- [ ] **Step 4: Prove the cycle is gone**

```bash
cp /tmp/cycle_probe.go internal/agent/zz_cycle_probe.go
go build ./internal/agent/ > /tmp/cycle2.log 2>&1; echo "exit=$?"; test -s /tmp/cycle2.log && cat /tmp/cycle2.log
rm internal/agent/zz_cycle_probe.go
```

Expected: exit 0 and no output — `internal/agent` may now import `internal/agent/tools`. This is
the precondition Task 1 depends on; if it still fails, Task 1 cannot proceed.

- [ ] **Step 5: Full suite, unchanged behaviour**

This task moves code without changing it, so every existing test must still pass and the skip
count must not move.

```bash
go build ./... > /tmp/t0-build.log 2>&1; echo "build exit=$?"; test -s /tmp/t0-build.log && cat /tmp/t0-build.log
go vet ./... > /tmp/t0-vet.log 2>&1; echo "vet exit=$?"; test -s /tmp/t0-vet.log && cat /tmp/t0-vet.log
go test ./... -v > /tmp/t0-test.log 2>&1; echo "test exit=$?"
grep -E '^(FAIL|--- FAIL)' /tmp/t0-test.log || echo "no failures"
grep -cE '^(\s+)?--- SKIP' /tmp/t0-test.log
GOOS=linux go build ./... && echo "linux build ok"
```

Expected: build and vet clean, no failures, **skip count exactly 3** (the recorded baseline),
linux build ok.

- [ ] **Step 6: Commit**

```bash
git add internal/skills/skills.go internal/skills/skills_test.go internal/agent/config.go internal/agent/sysprompt.go internal/agent/sysprompt_test.go internal/agent/tools/skill.go internal/agent/tools/skill_test.go cmd/fundid/agent.go cmd/fundid/agent_test.go
git status --short   # confirm the two deletions from internal/agent are staged
git diff --cached --stat
git commit -m "refactor: extract internal/skills to break the tools-agent cycle

internal/agent/tools imported internal/agent for SkillMeta, so internal/agent
could not import tools -- documented at config.go:42-49, which is why
Config.Tools is an agentloop.ToolSet interface.

skills.go depended on nothing else in package agent: it was a leaf in a
non-leaf package, and that was what dragged the whole runtime into tools.
Moved verbatim, signatures unchanged. internal/agent can now import tools and
assemble a whole conversation, which the next task needs."
```

---

### Task 1: Extract the agent runtime builder

**Depends on Task 0.** `BuildRuntime` lives in `package agent` and calls into
`internal/agent/tools`, which is only legal once Task 0 has moved `SkillMeta` out. Every
`SkillMeta` / `DiscoverSkills` / `SkillsInventory` reference in this task's code is
`skills.SkillMeta` etc., imported from `git.graveland.dev/brent/fundi/internal/skills`.

The daemon must build an `Engine` from a spawn request, but that assembly lives inline in `cmd/fundid/agent.go:126-215` and depends on two process globals a daemon must not touch per child: `os.Getwd()` and `signal.NotifyContext`.

**Files:**
- Create: `internal/agent/runtime.go`, `internal/agent/runtime_test.go`
- Modify: `cmd/fundid/agent.go:126-215`

**Interfaces:**
- Consumes: nothing.
- Produces: `agent.RuntimeOptions` (fields listed in Step 3) and
  `agent.BuildRuntime(ctx context.Context, fe *Frontend, opts RuntimeOptions) (*Engine, func(), error)`.
  The returned `func()` closes MCP connections and engine resources and must be called exactly once.
  Tasks 3, 4 and 5 all use these.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/runtime_test.go`:

```go
package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRuntimeOptions returns options that build a working engine with no API
// credentials and no database: FakeTurns replaces the upstream sender.
//
// NOTE: Config.FakeTurns is a PATH to a newline-delimited-JSON file of
// anthropic.Message values (loaded by LoadFakeSender), NOT literal reply text.
// Build it with this package's existing writeFakeTurns/sampleEndTurn helpers.
func fakeRuntimeOptions(t *testing.T, cwd string) RuntimeOptions {
	t.Helper()
	return RuntimeOptions{
		Model:          "anthropic/claude-sonnet-4-5",
		Cwd:            cwd,
		Ref:            "test-ref",
		SpillDir:       t.TempDir(),
		FakeTurns:      writeFakeTurns(t, sampleEndTurn),
		NoSkills:       true,
		NoContextFiles: true,
	}
}

// TestBuildRuntimeUsesExplicitCwd proves BuildRuntime resolves from opts.Cwd,
// not the process working directory. The daemon's cwd is never the child's, so
// a BuildRuntime that called os.Getwd would load the wrong context files and
// skills for every in-process child — and would do it silently.
func TestBuildRuntimeUsesExplicitCwd(t *testing.T) {
	childCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(childCwd, "AGENTS.md"), []byte("MARKER-CHILD-CWD"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	// Run from somewhere else so os.Getwd() cannot accidentally pass.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})

	opts := fakeRuntimeOptions(t, childCwd)
	opts.NoContextFiles = false // the point of this test is that AGENTS.md is found

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	eng, shutdown, err := BuildRuntime(context.Background(), fe, opts)
	if err != nil {
		t.Fatalf("BuildRuntime: %v", err)
	}
	defer shutdown()
	if eng == nil {
		t.Fatal("BuildRuntime returned a nil engine")
	}
}

// TestBuildRuntimeRejectsRelativeCwd guards the same failure from the other
// side: a relative cwd would resolve against the daemon's directory.
func TestBuildRuntimeRejectsRelativeCwd(t *testing.T) {
	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	opts := fakeRuntimeOptions(t, "relative/path")
	if _, _, err := BuildRuntime(context.Background(), fe, opts); err == nil {
		t.Fatal("expected an error for a relative Cwd, got nil")
	}
}

// TestBuildRuntimeMissingMCPConfigIsAnError pins the contract cmd/fundid relies
// on: BuildRuntime errors on any MCPConfig path that does not exist. The
// "silently skip a defaulted <cwd>/.mcp.json" behaviour stays in cmd/fundid,
// which passes an empty MCPConfig in that case.
func TestBuildRuntimeMissingMCPConfigIsAnError(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.MCPConfig = filepath.Join(t.TempDir(), "does-not-exist.json")

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	if _, _, err := BuildRuntime(context.Background(), fe, opts); err == nil {
		t.Fatal("expected an error for a non-existent MCPConfig, got nil")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/agent/ -run 'TestBuildRuntime' -v 2>&1 | tee /tmp/t1.log
grep -c '=== RUN' /tmp/t1.log
```

Expected: a non-zero `=== RUN` count and a compile failure — `undefined: RuntimeOptions`, `undefined: BuildRuntime`. A zero count means the tests never ran and this step proved nothing.

- [ ] **Step 3: Write `internal/agent/runtime.go`**

Read `cmd/fundid/agent.go:126-215` first; the `agent.Config` literal there is the source of truth for which fields exist. Copy the field assignments exactly.

```go
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/fundi/internal/agent/tools"
	"git.graveland.dev/brent/fundi/internal/paths"
	"git.graveland.dev/brent/fundi/internal/skills"
)

// RuntimeOptions is everything BuildRuntime needs to assemble an Engine. It is
// deliberately free of process globals: Cwd is explicit because the daemon's
// working directory is never the child's, and ctx is a parameter because a
// daemon must not install per-child signal handlers.
type RuntimeOptions struct {
	Model                string
	ThinkingBudget       int64 // int64: agent.ThinkingBudgetFor returns int64
	SystemPromptOverride string
	AppendSystemPrompt   string
	Cwd                  string // must be absolute
	Ref                  string
	Name                 string
	SpillDir             string   // defaults to paths.SpillDir(Ref) when empty
	SkillsDirs           []string // already assembled; see assembleSkillDirs in cmd/fundid
	Skills               string   // comma-separated allowlist; empty means all
	NoSkills             bool
	NoContextFiles       bool
	MCPConfig            string // absolute path, or empty to skip MCP entirely
	FakeTurns            string
	AnthropicAPIKey      string
	OpenRouterAPIKey     string

	// Pool is the shared database pool. A nil Pool means an in-memory
	// conversation. BuildRuntime never opens a pool itself, so a unit test does
	// not need postgres. Read by BuildEngine as of Task 4.
	Pool *pgxpool.Pool
}

// BuildRuntime assembles the tool registry, skills, MCP connections, and the
// Engine. The returned shutdown func releases MCP connections and engine
// resources; call it exactly once.
//
// MCPConfig is required to exist when non-empty. The "silently skip an absent
// defaulted <cwd>/.mcp.json" rule lives in the caller, which passes an empty
// MCPConfig in that case — keeping the policy decision where the default is
// computed rather than duplicating it here.
func BuildRuntime(ctx context.Context, fe *Frontend, opts RuntimeOptions) (*Engine, func(), error) {
	if !filepath.IsAbs(opts.Cwd) {
		return nil, nil, fmt.Errorf("runtime: cwd must be absolute: %q", opts.Cwd)
	}

	spillDir := opts.SpillDir
	if spillDir == "" {
		ref := opts.Ref
		if ref == "" {
			ref = "standalone"
		}
		spillDir = paths.SpillDir(ref)
	}
	outputPolicy := tools.OutputPolicy{SpillDir: spillDir}

	var contextFiles string
	if !opts.NoContextFiles {
		var err error
		contextFiles, err = LoadContextFiles(opts.Cwd)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: load context files: %w", err)
		}
	}

	// NOTE: the local is `discovered`, not `skills` — a variable named `skills`
	// would shadow the imported package of the same name.
	var discovered []skills.SkillMeta
	if !opts.NoSkills {
		var only []string
		if opts.Skills != "" {
			only = strings.Split(opts.Skills, ",")
		}
		var err error
		discovered, err = skills.DiscoverSkills(opts.SkillsDirs, only)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: discover skills: %w", err)
		}
	}

	registry := tools.NewRegistry()
	tools.RegisterFileTools(registry, tools.NewFileTracker())
	tools.RegisterBash(registry, outputPolicy, opts.Cwd)
	if len(discovered) > 0 {
		tools.RegisterSkillTool(registry, discovered)
	}

	mcpShutdown := func() {}
	if opts.MCPConfig != "" {
		if _, err := os.Stat(opts.MCPConfig); err != nil {
			return nil, nil, fmt.Errorf("runtime: mcp config %s: %w", opts.MCPConfig, err)
		}
		mcpCfg, err := tools.LoadMCPConfig(opts.MCPConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: load mcp config %s: %w", opts.MCPConfig, err)
		}
		mcpShutdown, err = tools.ConnectMCP(ctx, registry, mcpCfg, outputPolicy)
		if err != nil {
			return nil, nil, fmt.Errorf("runtime: connect mcp: %w", err)
		}
	}

	cfg := Config{
		Model:                opts.Model,
		ThinkingBudget:       opts.ThinkingBudget,
		SystemPromptOverride: opts.SystemPromptOverride,
		AppendSystemPrompt:   opts.AppendSystemPrompt,
		ContextFiles:         contextFiles,
		SkillsInventory:      skills.SkillsInventory(discovered),
		Cwd:                  opts.Cwd,
		Ref:                  opts.Ref,
		Name:                 opts.Name,
		FakeTurns:            opts.FakeTurns,
		AnthropicAPIKey:      opts.AnthropicAPIKey,
		OpenRouterAPIKey:     opts.OpenRouterAPIKey,
		Tools:                registry,
	}

	eng, engShutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		mcpShutdown()
		return nil, nil, fmt.Errorf("runtime: build engine: %w", err)
	}
	return eng, func() {
		mcpShutdown()
		engShutdown()
	}, nil
}
```

`opts.Pool` is intentionally not wired into `cfg` yet — `BuildEngine` still opens its own pool until Task 4, and wiring it here first would have to be rewritten. The field and its doc comment exist now so `RuntimeOptions` is stable for Task 3.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/agent/ -run 'TestBuildRuntime' -v 2>&1 | tee /tmp/t1.log
grep -E '=== RUN|--- (PASS|FAIL)' /tmp/t1.log
grep '\-\-\- FAIL' /tmp/t1.log || echo "no failures"
```

Expected: three tests present, all PASS.

- [ ] **Step 5: Rewire `cmd/fundid/agent.go` onto `BuildRuntime`**

Replace the assembly at lines 126-215 with a `RuntimeOptions` literal. Keep in `agent.go`: `os.Getwd()`, `signal.NotifyContext`, `agent.ThinkingBudgetFor(f.thinking)`, `assembleSkillDirs(cwd, f.skillsDir)`, and `resolveMCPConfig(f.mcpConfig, cwd)`.

Preserve the MCP asymmetry exactly. Today an explicit `--mcp-config` that does not exist is a startup error while a defaulted `<cwd>/.mcp.json` is silently skipped when absent. `BuildRuntime` errors on any non-existent path, so `agent.go` decides:

```go
	mcpPath := resolveMCPConfig(f.mcpConfig, cwd)
	explicitMCP := f.mcpConfig != ""
	if _, err := os.Stat(mcpPath); err != nil {
		if explicitMCP {
			slog.Error("agent: --mcp-config not found", "path", mcpPath, "error", err)
			return 1
		}
		mcpPath = "" // defaulted path absent: skip MCP, as before
	}
```

Then keep the existing `fe.Run()` / `eng.Wait()` / `eng.Close()` / shutdown ordering untouched — its comment explains why that order is race-free.

- [ ] **Step 6: Verify the CLI path is unchanged**

```bash
go build ./... && go test ./... 2>&1 | tee /tmp/t1-all.log
grep -E '^(FAIL|--- FAIL)' /tmp/t1-all.log || echo "no failures"
go run ./cmd/fundid agent --help 2>&1 | head -5
```

Expected: build clean, no failures, `agent --help` still prints usage and exits 0.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/runtime.go internal/agent/runtime_test.go cmd/fundid/agent.go
git diff --cached --stat
git commit -m "refactor(agent): extract BuildRuntime with explicit cwd and context

The daemon must assemble an Engine per child, but the assembly lived inline in
cmd/fundid/agent.go and depended on os.Getwd() and signal.NotifyContext, two
process globals a daemon must not touch per child. Both are now parameters.

The 'silently skip an absent defaulted .mcp.json, but error on an explicit
one' rule stays in cmd/fundid where the default is computed."
```

---

### Task 2: Introduce the `Runner` seam with `processRunner`

Pure refactor. No behaviour change; every existing `internal/child` test must pass untouched.

**Files:**
- Create: `internal/child/runner.go`, `internal/child/runner_test.go`
- Modify: `internal/child/child.go` — `SpawnSpec` (line 25), `Child` (line 97), `Spawn` (line 135), `PID` (line 219), the reap in `readStdout` (line 486), `Shutdown` (line 602), `Interrupt` (line 654)

**Interfaces:**
- Consumes: nothing.
- Produces: `child.Runner` with `Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error)`, `Wait() (exitCode int, signal string)`, `PID() int`, `Terminate() error`, `Kill() error`, `Interrupt() error`; and `SpawnSpec.Runner Runner`, which when nil makes `Spawn` build a `processRunner`. Task 3 implements the interface; Task 5 sets the field.

- [ ] **Step 1: Write the failing test**

Create `internal/child/runner_test.go`:

```go
package child

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRunner is a Runner backed by in-memory streams. It proves Spawn honours
// an injected runner and never reaches exec.Command.
type stubRunner struct {
	stdoutFrames string

	mu      sync.Mutex
	started bool
	waited  bool
}

func (s *stubRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	// stdin: discard whatever the supervise loop writes.
	inR, inW := io.Pipe()
	go func() {
		if _, err := io.Copy(io.Discard, inR); err != nil {
			return // pipe closed on shutdown; nothing to report
		}
	}()

	stdout := io.NopCloser(strings.NewReader(s.stdoutFrames))
	stderr := io.NopCloser(strings.NewReader(""))
	return inW, stdout, stderr, nil
}

func (s *stubRunner) Wait() (int, string) {
	s.mu.Lock()
	s.waited = true
	s.mu.Unlock()
	return 0, ""
}

func (s *stubRunner) PID() int         { return 0 }
func (s *stubRunner) Terminate() error { return nil }
func (s *stubRunner) Kill() error      { return nil }
func (s *stubRunner) Interrupt() error { return nil }

// TestSpawnUsesInjectedRunner proves the seam is real: with a runner on the
// spec, Spawn must not exec anything, and frames from the runner's stdout must
// reach the ring exactly as they would from a process.
func TestSpawnUsesInjectedRunner(t *testing.T) {
	stub := &stubRunner{
		stdoutFrames: `{"type":"agent_start"}` + "\n" + `{"type":"agent_end"}` + "\n",
	}

	// PiBinary is deliberately empty: an injected runner makes it irrelevant,
	// and a non-empty value would hide a fallback to exec.Command.
	c, err := Spawn(t.Context(), SpawnSpec{
		ChildID: "c_stub",
		Cwd:     t.TempDir(),
		Runner:  stub,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not finish within 5s")
	}

	got := c.RingSnapshot()
	if len(got) != 2 {
		t.Fatalf("ring holds %d frames, want 2: %q", len(got), got)
	}
	if !strings.Contains(string(got[0]), "agent_start") {
		t.Errorf("first frame = %q, want agent_start", got[0])
	}
	if c.PID() != 0 {
		t.Errorf("PID() = %d, want 0 for an injected runner", c.PID())
	}

	stub.mu.Lock()
	started, waited := stub.started, stub.waited
	stub.mu.Unlock()
	if !started {
		t.Error("runner.Start was never called")
	}
	if !waited {
		t.Error("runner.Wait was never called")
	}
}

// TestSpawnWithoutRunnerStillRequiresBinary preserves the process path's
// contract: no runner and no binary is a spec error, not a panic.
func TestSpawnWithoutRunnerStillRequiresBinary(t *testing.T) {
	if _, err := Spawn(t.Context(), SpawnSpec{ChildID: "c_nobin", Cwd: t.TempDir()}); err == nil {
		t.Fatal("expected an error when neither Runner nor PiBinary is set")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/child/ -run 'TestSpawnUsesInjectedRunner|TestSpawnWithoutRunnerStillRequiresBinary' -v 2>&1 | tee /tmp/t2.log
grep -c '=== RUN' /tmp/t2.log
```

Expected: non-zero `=== RUN`, compile failure on `unknown field Runner in struct literal`.

- [ ] **Step 3: Write `internal/child/runner.go`**

```go
package child

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Runner abstracts a child's execution context: how it starts, how its three
// streams are obtained, how it is waited on, and how it is signalled. The
// supervise and readStdout loops are written against the streams alone, so they
// are identical for a subprocess and for an in-process engine.
//
// Runner is exported and injected via SpawnSpec rather than selected inside this
// package: internal/agent already imports internal/child, so an in-process
// implementation cannot live here without an import cycle.
type Runner interface {
	// Start begins execution and returns the child's three streams. stdin is
	// written by the supervise loop, stdout is read by readStdout, and stderr is
	// drained into the child's error buffer. An implementation with no separate
	// diagnostic stream must return a reader at EOF, never nil.
	Start() (stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, err error)

	// Wait blocks until execution has finished and reports the outcome.
	// exitCode is -1 when it could not be determined; signal is empty when the
	// runner was not signalled.
	Wait() (exitCode int, signal string)

	// PID reports the OS process id, or 0 when there is no process.
	PID() int

	// Terminate requests a graceful stop.
	Terminate() error

	// Kill forces an immediate stop.
	Kill() error

	// Interrupt asks the runner to abort its current turn without stopping.
	Interrupt() error
}

// processRunner runs a child as a subprocess: the behaviour Child had before
// the Runner seam existed.
type processRunner struct {
	cmd *exec.Cmd
}

// newProcessRunner builds a subprocess runner for spec. The child gets its own
// process group so subprocesses it spawns can be signalled as a group during
// shutdown — otherwise an orphan keeps a pipe write end open and blocks our
// readers.
func newProcessRunner(spec SpawnSpec) (*processRunner, error) {
	if spec.PiBinary == "" {
		return nil, errors.New("pi binary path required")
	}
	argv := append([]string{}, spec.Argv...)
	argv = append(argv, spec.ExtraArgs...)

	cmd := exec.Command(spec.PiBinary, argv...)
	cmd.Dir = spec.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(spec.Env) > 0 {
		if spec.EnvOverride {
			cmd.Env = append([]string{}, spec.Env...)
		} else {
			cmd.Env = append(os.Environ(), spec.Env...)
		}
	}
	return &processRunner{cmd: cmd}, nil
}

func (p *processRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := p.cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start: %w", err)
	}
	return stdin, stdout, stderr, nil
}

func (p *processRunner) Wait() (int, string) {
	state, err := p.cmd.Process.Wait()
	if err != nil {
		return -1, ""
	}
	code := -1
	if state.ExitCode() >= 0 {
		code = state.ExitCode()
	}
	sig := ""
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig = ws.Signal().String()
	}
	return code, sig
}

func (p *processRunner) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *processRunner) Terminate() error { return p.signalGroup(syscall.SIGTERM) }
func (p *processRunner) Kill() error      { return p.signalGroup(syscall.SIGKILL) }
func (p *processRunner) Interrupt() error { return p.signalGroup(syscall.SIGINT) }

// signalGroup signals the whole process group (negative PID) so subprocesses the
// child spawned are signalled too. A process that exited between the caller's
// liveness check and here yields ESRCH — that is the no-op the caller asked
// for, not an error.
func (p *processRunner) signalGroup(sig syscall.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("signal: no process handle")
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Rewire `Child` onto the seam**

In `internal/child/child.go`:

1. Add to `SpawnSpec`:

```go
	// Runner overrides how the child executes. When nil, a subprocess runner is
	// built from PiBinary/Argv/Env. Injected rather than selected inside this
	// package because internal/agent imports it.
	Runner Runner
```

2. Replace the `cmd *exec.Cmd` field on `Child` with `runner Runner`.
3. In `Spawn`, **delete the `if spec.PiBinary == ""` guard** (it moved into `newProcessRunner`, so an injected runner needs no binary), keep the `Cwd` absoluteness and `os.Stat` checks, and replace the `exec.Command`/pipe/`Start` block with:

```go
	r := spec.Runner
	if r == nil {
		pr, perr := newProcessRunner(spec)
		if perr != nil {
			return nil, perr
		}
		r = pr
	}
	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		return nil, err
	}
```

   and set `runner: r` in the `Child` literal instead of `cmd: cmd`.
4. `func (c *Child) PID() int { return c.runner.PID() }`
5. In `readStdout`, replace the reap block with:

```go
	code, sig := c.runner.Wait()
	c.mu.Lock()
	c.exit.ExitCode = code
	c.exit.Signal = sig
	c.closed = true
	c.mu.Unlock()

	close(c.processDone)
```

6. In `Shutdown`, replace the two `syscall.Kill` calls. Do not discard these errors — the old code used `_ =` on a raw syscall, but a `Runner` error can indicate a broken implementation and must be visible:

```go
		escalated = true
		if terr := c.runner.Terminate(); terr != nil {
			slog.Warn("terminate runner", "child", c.ID, "error", terr)
		}
		select {
		case <-c.done:
		case <-time.After(killTimeout):
			if kerr := c.runner.Kill(); kerr != nil {
				slog.Warn("kill runner", "child", c.ID, "error", kerr)
			}
			<-c.done
		}
```

7. `Interrupt` keeps its closed-check and delegates:

```go
func (c *Child) Interrupt() error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil
	}
	return c.runner.Interrupt()
}
```

Drop the now-unused imports from `child.go` (`os/exec`, and `syscall` if nothing else there uses it).

- [ ] **Step 5: Run the new tests and the whole child suite**

```bash
go test ./internal/child/ -v 2>&1 | tee /tmp/t2.log
grep -E '^(--- FAIL|\s+--- FAIL)' /tmp/t2.log || echo "no failures"
grep -cE '^(--- SKIP|\s+--- SKIP)' /tmp/t2.log
```

Expected: no FAIL lines. Record the skip count and confirm it did not increase — a test that started skipping is a test that stopped protecting you.

- [ ] **Step 6: Commit**

```bash
git add internal/child/runner.go internal/child/runner_test.go internal/child/child.go
git diff --cached --stat
git commit -m "refactor(child): put execution behind an injected Runner seam

Child held *exec.Cmd and touched it in five places: Spawn, PID, the reap in
readStdout, and the signal calls in Shutdown and Interrupt. Those move behind
a Runner interface, with processRunner as the subprocess implementation.

Runner is exported and injected via SpawnSpec rather than chosen inside the
package, because internal/agent already imports internal/child and an
in-process implementation here would be an import cycle."
```

---

### Task 3: In-process runner with panic containment

**Files:**
- Create: `internal/inproc/runner.go`, `internal/inproc/runner_test.go`

**Interfaces:**
- Consumes: `agent.RuntimeOptions`, `agent.BuildRuntime` (Task 1); `child.Runner` (Task 2).
- Produces: `inproc.Options{ChildID string, Runtime agent.RuntimeOptions, Parent context.Context, Build BuildFunc}`, `inproc.BuildFunc` = `func(context.Context, *agent.Frontend, agent.RuntimeOptions) (*agent.Engine, func(), error)`, and `inproc.New(Options) *Runner`. `Build` defaults to `agent.BuildRuntime`; tests inject a panicking builder through it. Task 5 calls `inproc.New`.

- [ ] **Step 1: Write the failing tests**

Create `internal/inproc/runner_test.go`:

```go
package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/internal/agent"
)

// sampleEndTurn is one scripted assistant message: a completed turn whose text
// the test asserts on. Mirrors internal/agent/engine_test.go's fixture of the
// same name — that one is an unexported const in package agent, so it cannot be
// imported here.
const sampleEndTurn = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
	`"stop_reason":"end_turn","content":[{"type":"text","text":"the fake reply"}],` +
	`"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

// writeFakeTurns writes scripted assistant messages as one ndjson file and
// returns its path.
//
// Config.FakeTurns is a PATH to a newline-delimited-JSON file of
// anthropic.Message values (loaded by agent.LoadFakeSender) — NOT literal reply
// text. package agent has its own writeFakeTurns helper, but it is an
// unexported test helper in another package, so this test needs its own.
func writeFakeTurns(t *testing.T, bodies ...string) string {
	t.Helper()
	lines := make([]string, 0, len(bodies))
	for _, b := range bodies {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(b)); err != nil {
			t.Fatalf("compact scripted body: %v", err)
		}
		lines = append(lines, compact.String())
	}
	path := filepath.Join(t.TempDir(), "turns.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write scripted turns: %v", err)
	}
	return path
}

// readFramesUntil reads newline-delimited JSON frames from r until it sees one
// whose "type" equals want, or until r hits EOF. Returns every frame read.
func readFramesUntil(t *testing.T, r io.Reader, want string) []string {
	t.Helper()
	var out []string
	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out
		}
		out = append(out, string(raw))
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			continue // unparseable, but still recorded above
		}
		if hdr.Type == want {
			return out
		}
	}
}

// TestRunnerDrivesAFakeTurn proves a prompt written to the runner's stdin
// produces a real frame sequence on its stdout, with no subprocess involved.
//
// It asserts on the intermediate frames, not just the terminal one. This repo
// shipped empty message_update frames twice with a fully green suite, both
// times because every assertion targeted message_end.
func TestRunnerDrivesAFakeTurn(t *testing.T) {
	r := New(Options{
		ChildID: "c_inproc",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{
			Model:          "anthropic/claude-sonnet-4-5",
			Cwd:            t.TempDir(),
			Ref:            "c_inproc",
			SpillDir:       t.TempDir(),
			FakeTurns:      writeFakeTurns(t, sampleEndTurn),
			NoSkills:       true,
			NoContextFiles: true,
		},
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// stderr must be a live reader at EOF, never nil: readStderr would panic.
	if stderr == nil {
		t.Fatal("Start returned a nil stderr")
	}
	if _, err := io.ReadAll(stderr); err != nil {
		t.Errorf("stderr read: %v", err)
	}

	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"hello"}` + "\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	done := make(chan []string, 1)
	go func() { done <- readFramesUntil(t, stdout, "agent_end") }()

	var frames []string
	select {
	case frames = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("no agent_end frame within 15s")
	}

	joined := strings.Join(frames, "\n")
	for _, want := range []string{"agent_start", "message_end", "agent_end"} {
		if !strings.Contains(joined, want) {
			t.Errorf("frame stream missing %q; got:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "the fake reply") {
		t.Errorf("frame stream missing the fake turn text; got:\n%s", joined)
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	code, sig := r.Wait()
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if sig != "" {
		t.Errorf("signal = %q, want empty for an in-process runner", sig)
	}
	if r.PID() != 0 {
		t.Errorf("PID() = %d, want 0", r.PID())
	}
}

// TestRunnerContainsPanic is the load-bearing test of this phase. A panic in
// one conversation must become that child's exit, not the daemon's. Without the
// recover in run(), this test crashes the test binary rather than failing.
func TestRunnerContainsPanic(t *testing.T) {
	r := New(Options{
		ChildID: "c_panic",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
		Build: func(context.Context, *agent.Frontend, agent.RuntimeOptions) (*agent.Engine, func(), error) {
			panic("boom from the engine builder")
		},
	})

	_, stdout, _, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The panic must close stdout so the daemon's readStdout sees an ordinary
	// EOF and the normal child-exit path runs.
	if _, err := io.ReadAll(stdout); err != nil {
		t.Errorf("stdout should EOF cleanly after a contained panic, got: %v", err)
	}

	code, _ := r.Wait()
	if code == 0 {
		t.Error("exit code = 0 after a panic, want non-zero")
	}
}

// TestRunnerBuildErrorIsAnExit proves a failed build is reported as a non-zero
// exit rather than hanging the daemon's supervise loop forever.
func TestRunnerBuildErrorIsAnExit(t *testing.T) {
	r := New(Options{
		ChildID: "c_builderr",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: "relative/path"}, // BuildRuntime rejects this
	})
	if _, stdout, _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	} else if _, rerr := io.ReadAll(stdout); rerr != nil {
		t.Errorf("stdout read: %v", rerr)
	}
	if code, _ := r.Wait(); code == 0 {
		t.Error("exit code = 0 after a build failure, want non-zero")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/inproc/ -v 2>&1 | tee /tmp/t3.log
grep -c '=== RUN' /tmp/t3.log
```

Expected: non-zero `=== RUN`; failure is the missing package / `undefined: New`.

- [ ] **Step 3: Write `internal/inproc/runner.go`**

```go
// Package inproc runs a fundi agent as a goroutine inside the daemon rather
// than as a subprocess. It implements child.Runner over a pair of io.Pipes, so
// the daemon's frame loops are identical for both execution models.
//
// This package exists because internal/agent imports internal/child: an
// in-process runner cannot live in internal/child without an import cycle.
// Nothing imports this package except cmd/fundid.
package inproc

import (
	"context"
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"

	"git.graveland.dev/brent/fundi/internal/agent"
	"git.graveland.dev/brent/fundi/internal/child"
)

// BuildFunc constructs the engine. Defaults to agent.BuildRuntime; tests
// substitute it to inject failures a real builder cannot produce on demand.
type BuildFunc func(ctx context.Context, fe *agent.Frontend, ro agent.RuntimeOptions) (*agent.Engine, func(), error)

// Options configures a Runner.
type Options struct {
	ChildID string
	Runtime agent.RuntimeOptions
	// Parent is the daemon's context. The runner derives a cancellable child
	// from it, so cancelling Parent stops every in-process child at once.
	Parent context.Context
	// Build defaults to agent.BuildRuntime when nil.
	Build BuildFunc
}

// Runner drives an agent.Engine in a goroutine. It satisfies child.Runner.
type Runner struct {
	opts   Options
	done   chan struct{}

	mu       sync.Mutex
	cancel   context.CancelFunc
	exitCode int
	eng      *agent.Engine
	stdinR   *io.PipeReader
}

// New returns a Runner for opts. Nothing runs until Start is called.
func New(opts Options) *Runner {
	if opts.Build == nil {
		opts.Build = agent.BuildRuntime
	}
	if opts.Parent == nil {
		opts.Parent = context.Background()
	}
	return &Runner{opts: opts, done: make(chan struct{})}
}

// Compile-time proof that Runner satisfies the seam it exists for.
var _ child.Runner = (*Runner)(nil)

// Start wires two pipes and launches the engine goroutine. The daemon writes
// frames to the returned stdin and reads them from the returned stdout.
func (r *Runner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	ctx, cancel := context.WithCancel(r.opts.Parent)
	r.mu.Lock()
	r.cancel = cancel
	r.stdinR = stdinR
	r.mu.Unlock()

	go r.run(ctx, stdinR, stdoutW)

	// An in-process agent has no separate diagnostic stream: its logs go to the
	// daemon's logger tagged with the child id. Return a reader already at EOF
	// so readStderr completes immediately rather than being handed a nil.
	return stdinW, stdoutR, io.NopCloser(strings.NewReader("")), nil
}

// run owns the engine's whole lifetime. Deferred funcs are LIFO, so the
// recover-and-close defer registered first runs last: shutdown() and
// Engine.Close() complete before stdout closes, which is what makes the EOF the
// daemon sees mean "this child is finished".
func (r *Runner) run(ctx context.Context, stdinR *io.PipeReader, stdoutW *io.PipeWriter) {
	defer close(r.done)
	defer func() {
		if v := recover(); v != nil {
			slog.Error("inproc: agent panicked",
				"child", r.opts.ChildID, "panic", v, "stack", string(debug.Stack()))
			r.setExit(2)
			// The engine's turn worker may still be running; stop it. Its
			// Close() is unreachable from here — an accepted leak on a path
			// that should never execute.
			r.mu.Lock()
			cancel := r.cancel
			r.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		if err := stdoutW.Close(); err != nil {
			slog.Warn("inproc: close stdout", "child", r.opts.ChildID, "error", err)
		}
	}()

	fe := agent.NewFrontend(stdinR, stdoutW, nil)
	eng, shutdown, err := r.opts.Build(ctx, fe, r.opts.Runtime)
	if err != nil {
		slog.Error("inproc: build engine", "child", r.opts.ChildID, "error", err)
		r.setExit(1)
		return
	}
	r.mu.Lock()
	r.eng = eng
	r.mu.Unlock()
	defer shutdown()

	// Frontend.Run returns only on stdin EOF or a scan error, so no further
	// HandlePrompt/HandleSteer/HandleAbort can arrive afterwards — which is what
	// makes Wait-then-Close race-free. Same ordering as cmd/fundid/agent.go.
	if runErr := fe.Run(); runErr != nil {
		slog.Error("inproc: frontend run", "child", r.opts.ChildID, "error", runErr)
		r.setExit(1)
	}
	eng.Wait()
	eng.Close()
}

func (r *Runner) setExit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exitCode = code
}

// Wait blocks until the engine goroutine finishes. An in-process runner is
// never signalled, so the signal string is always empty.
func (r *Runner) Wait() (int, string) {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitCode, ""
}

// PID reports 0: there is no process. Callers persisting a PID must treat 0 as
// "no process to signal" — loadOrphans already does (`if rec.PID > 0`).
func (r *Runner) PID() int { return 0 }

// Terminate cancels the engine's context, aborting any turn in flight. The
// daemon calls this only after closing stdin failed to end the child in time.
func (r *Runner) Terminate() error {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Kill cancels the context and closes the read end of stdin, forcing
// Frontend.Run to return even if it is blocked on a read.
func (r *Runner) Kill() error {
	if err := r.Terminate(); err != nil {
		return err
	}
	r.mu.Lock()
	stdinR := r.stdinR
	r.mu.Unlock()
	if stdinR != nil {
		return stdinR.Close()
	}
	return nil
}

// Interrupt aborts the current turn without stopping the runner — the
// in-process equivalent of SIGINT to a subprocess.
func (r *Runner) Interrupt() error {
	r.mu.Lock()
	eng := r.eng
	r.mu.Unlock()
	if eng == nil {
		return nil // not built yet; nothing to abort
	}
	eng.HandleAbort()
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/inproc/ -v 2>&1 | tee /tmp/t3.log
grep -E '=== RUN|--- (PASS|FAIL)' /tmp/t3.log
grep '\-\-\- FAIL' /tmp/t3.log || echo "no failures"
```

Expected: all three PASS. If `TestRunnerContainsPanic` crashes the binary with a stack trace instead of failing cleanly, the `recover` is missing or sits in the wrong defer.

- [ ] **Step 5: Verify race-freedom**

```bash
go test ./internal/inproc/ -race -v 2>&1 | tee /tmp/t3-race.log
grep -E 'DATA RACE|--- FAIL' /tmp/t3-race.log || echo "no races"
```

Expected: no races. This runner is touched by the daemon's supervise goroutine, the engine goroutine, and shutdown callers, so `-race` is mandatory here.

- [ ] **Step 6: Commit**

```bash
git add internal/inproc/runner.go internal/inproc/runner_test.go
git diff --cached --stat
git commit -m "feat(inproc): run an agent Engine as a goroutine behind child.Runner

Implements child.Runner over a pair of io.Pipes so the daemon's frame loops
are identical for a subprocess and an in-process engine. It lives in its own
package because internal/agent imports internal/child.

A panic in one conversation is contained and converted into that child's
exit: recover logs with a stack, records a non-zero code, cancels the engine
context, and closes stdout so the daemon sees an ordinary EOF."
```

---

### Task 4: One shared pool

`BuildEngine` opens its own `pgxpool` (`internal/agent/config.go:129-130`), which suits one agent per process and is wrong once N engines share a daemon.

**Files:**
- Modify: `internal/agent/config.go` — `Config`, and the pool branch in `BuildEngine`
- Modify: `internal/agent/runtime.go` — pass `opts.Pool` into `Config`
- Modify: `internal/agent/runtime_test.go` — add the nil-pool test
- Modify: `cmd/fundid/agent.go` — open a pool for the standalone CLI path

**Interfaces:**
- Consumes: `RuntimeOptions.Pool` (Task 1).
- Produces: `Config.Pool *pgxpool.Pool`. `BuildEngine` keeps its `(ctx, fe)` signature but reads `c.Pool` instead of dialling. A nil pool means an in-memory conversation, exactly as an empty `DBURL` behaves today. Task 5 supplies the daemon's pool.

- [ ] **Step 1: Write the failing test**

Add to `internal/agent/runtime_test.go`:

```go
// TestBuildRuntimeNilPoolIsInMemory pins the contract Task 5 depends on: no
// pool means an in-memory conversation, and BuildRuntime must never open one.
// A BuildRuntime that dialled a database here would make every unit test in
// this package require postgres.
func TestBuildRuntimeNilPoolIsInMemory(t *testing.T) {
	opts := fakeRuntimeOptions(t, t.TempDir())
	opts.Pool = nil

	fe := NewFrontend(strings.NewReader(""), io.Discard, nil)
	eng, shutdown, err := BuildRuntime(context.Background(), fe, opts)
	if err != nil {
		t.Fatalf("BuildRuntime with a nil pool: %v", err)
	}
	defer shutdown()
	if eng == nil {
		t.Fatal("nil engine")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./internal/agent/ -run TestBuildRuntimeNilPoolIsInMemory -v 2>&1 | tee /tmp/t4.log
grep -c '=== RUN' /tmp/t4.log
```

Expected: `=== RUN` present; fails because `Config` has no `Pool` field.

- [ ] **Step 3: Implement**

In `internal/agent/config.go`:

```go
	// Pool is the database pool backing conversation persistence. A nil Pool
	// means an in-memory conversation. BuildEngine never opens a pool itself —
	// the owning process does, so N engines in one daemon share one pool.
	Pool *pgxpool.Pool
```

Replace the `if c.DBURL != "" { pool, err = pgxpool.New(ctx, c.DBURL); ... }` branch with `pool := c.Pool`, and change every later `c.DBURL != ""` test to `pool != nil` — including the boot-time orphan-repair branch near line 200. If nothing reads `DBURL` afterwards, **delete the field**: a stale DSN on the struct would imply persistence that is not happening.

In `internal/agent/runtime.go`, add `Pool: opts.Pool` to the `Config` literal and delete the note from Task 1 Step 3 saying it is unwired.

In `cmd/fundid/agent.go`, the standalone CLI now owns its pool:

```go
	var pool *pgxpool.Pool
	if f.db != "" {
		pool, err = pgxpool.New(ctx, f.db)
		if err != nil {
			slog.Error("agent: open database", "error", err)
			return 1
		}
		defer pool.Close()
	}
```

and pass `Pool: pool` in the `RuntimeOptions` literal.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/agent/ ./cmd/fundid/ -v 2>&1 | tee /tmp/t4.log
grep '\-\-\- FAIL' /tmp/t4.log || echo "no failures"
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent/config.go internal/agent/runtime.go internal/agent/runtime_test.go cmd/fundid/agent.go
git diff --cached --stat
git commit -m "refactor(agent): inject the database pool instead of opening one

BuildEngine opened its own pgxpool, which suits one agent per process and
breaks once N engines share a daemon. The owning process supplies it now; a
nil pool means an in-memory conversation, matching today's empty-DBURL
behaviour."
```

---

### Task 5: Wire the daemon to run agent children in-process

The per-child configuration is **not** re-mapped by hand. `buildAgentArgv` stays, and the daemon parses its output with the existing pure `parseAgentFlags`, converting to `RuntimeOptions`. That keeps one mapping, preserves `req.ExtraArgs` and its documented last-flag-wins behaviour, and makes it impossible to silently drop a field — the exact failure that made `Resume` lose `SkillsDirs` and `MCPConfig` with a fully green suite.

**Files:**
- Create: `cmd/fundid/agent_runtime.go`, `cmd/fundid/agent_runtime_test.go`
- Modify: `cmd/fundid/controller.go` — `NewController` signature, and **all three** `SpawnSpec` sites: `Spawn` (~line 444), `Resume` (~line 965), `RespawnChild` (~line 1080)
- Modify: `cmd/fundid/main.go` — open the pool, pass ctx and pool to `NewController`, close the pool last

**Interfaces:**
- Consumes: `inproc.New`/`inproc.Options` (Task 3), `agent.RuntimeOptions` (Task 1), `Config.Pool` (Task 4).
- Produces: `agentFlags.toRuntimeOptions(cwd string, pool *pgxpool.Pool) (agent.RuntimeOptions, error)` and `(c *Controller) agentRunner(req protocol.SpawnRequest, childID string) (child.Runner, error)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/fundid/agent_runtime_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"git.graveland.dev/brent/fundi/protocol"
)

// TestArgvRoundTripsIntoRuntimeOptions is the anti-drop guard for this task.
// buildAgentArgv is the single place per-child config is expressed; parsing it
// back must reproduce every value. A field that buildAgentArgv emits and
// toRuntimeOptions ignores is silently lost for every in-process child, which
// is precisely how Resume lost SkillsDirs and MCPConfig while all tests passed.
func TestArgvRoundTripsIntoRuntimeOptions(t *testing.T) {
	mcp := t.TempDir() + "/mcp.json"
	req := protocol.SpawnRequest{
		Kind:               "agent",
		Cwd:                t.TempDir(),
		Model:              "anthropic/claude-sonnet-4-5",
		Thinking:           "high",
		SystemPrompt:       "SYSTEM-MARKER",
		AppendSystemPrompt: "APPEND-MARKER",
		Skills:             []string{"alpha", "beta"},
		Name:               "NAME-MARKER",
		SkillsDirs:         []string{"/tmp/skills-one", "/tmp/skills-two"},
		MCPConfig:          mcp,
	}

	argv := buildAgentArgv(req, "c_round", t.TempDir())
	if argv[0] != "agent" {
		t.Fatalf("argv[0] = %q, want \"agent\"", argv[0])
	}

	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags(%q): %v", argv[1:], err)
	}

	got, err := f.toRuntimeOptions(req.Cwd, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}

	if got.Model != req.Model {
		t.Errorf("Model = %q, want %q", got.Model, req.Model)
	}
	if got.SystemPromptOverride != req.SystemPrompt {
		t.Errorf("SystemPromptOverride = %q, want %q", got.SystemPromptOverride, req.SystemPrompt)
	}
	if got.AppendSystemPrompt != req.AppendSystemPrompt {
		t.Errorf("AppendSystemPrompt = %q, want %q", got.AppendSystemPrompt, req.AppendSystemPrompt)
	}
	if got.Skills != "alpha,beta" {
		t.Errorf("Skills = %q, want \"alpha,beta\"", got.Skills)
	}
	if got.Name != req.Name {
		t.Errorf("Name = %q, want %q", got.Name, req.Name)
	}
	if got.MCPConfig != mcp {
		t.Errorf("MCPConfig = %q, want %q", got.MCPConfig, mcp)
	}
	if got.SpillDir == "" {
		t.Error("SpillDir is empty; buildAgentArgv always passes --spill-dir")
	}
	if got.ThinkingBudget == 0 {
		t.Error("ThinkingBudget = 0 for --thinking high; the conversion was skipped")
	}
	// SkillsDirs must include both --skills-dir values. assembleSkillDirs
	// prepends the configured and per-project dirs, so assert containment.
	joined := strings.Join(got.SkillsDirs, ":")
	for _, want := range req.SkillsDirs {
		if !strings.Contains(joined, want) {
			t.Errorf("SkillsDirs %v missing %q", got.SkillsDirs, want)
		}
	}
}

// TestExtraArgsOverrideEarlierFlags proves the last-flag-wins convention
// buildAgentArgv documents survives the in-process path. ExtraArgs are appended
// last precisely so a caller can override, and an in-process child that ignored
// them would diverge from a subprocess one.
func TestExtraArgsOverrideEarlierFlags(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:      "agent",
		Cwd:       t.TempDir(),
		Model:     "anthropic/claude-sonnet-4-5",
		ExtraArgs: []string{"--model", "deepseek/deepseek-chat"},
	}
	argv := buildAgentArgv(req, "c_extra", t.TempDir())
	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	got, err := f.toRuntimeOptions(req.Cwd, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Model != "deepseek/deepseek-chat" {
		t.Errorf("Model = %q, want the ExtraArgs override to win", got.Model)
	}
}

// TestAgentRefIsDaemonControlled proves the child id reaches the engine. It
// normally arrives via the injected FUNDI_CHILD_ID env var, which an in-process
// child never inherits, so it must be appended to argv after ExtraArgs — a
// caller must not be able to point one child at another's conversation.
func TestAgentRefIsDaemonControlled(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:      "agent",
		Cwd:       t.TempDir(),
		Model:     "anthropic/claude-sonnet-4-5",
		ExtraArgs: []string{"--ref", "spoofed-child-id"},
	}
	argv := appendDaemonRef(buildAgentArgv(req, "c_authoritative", t.TempDir()), "c_authoritative")
	f, err := parseAgentFlags(argv[1:])
	if err != nil {
		t.Fatalf("parseAgentFlags: %v", err)
	}
	got, err := f.toRuntimeOptions(req.Cwd, nil)
	if err != nil {
		t.Fatalf("toRuntimeOptions: %v", err)
	}
	if got.Ref != "c_authoritative" {
		t.Errorf("Ref = %q, want the daemon's child id to win over ExtraArgs", got.Ref)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./cmd/fundid/ -run 'TestArgvRoundTrips|TestExtraArgsOverride|TestAgentRefIsDaemonControlled' -v 2>&1 | tee /tmp/t5.log
grep -c '=== RUN' /tmp/t5.log
```

Expected: `=== RUN` present, failing on `undefined: toRuntimeOptions` and `undefined: appendDaemonRef`.

- [ ] **Step 3: Write `cmd/fundid/agent_runtime.go`**

```go
package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/fundi/internal/agent"
)

// appendDaemonRef appends the authoritative --ref last so it wins over
// req.ExtraArgs under buildAgentArgv's last-flag-wins convention. A subprocess
// child receives its id through the injected FUNDI_CHILD_ID env var; an
// in-process child inherits no env, so the id must travel in argv. It must not
// be caller-overridable: --ref selects which stored conversation the child
// reattaches, so a spoofed value points one child at another's history.
func appendDaemonRef(argv []string, childID string) []string {
	return append(argv, "--ref", childID)
}

// toRuntimeOptions converts parsed agent flags into the options an in-process
// engine needs. cwd is explicit because the daemon's working directory is never
// the child's. pool is the daemon's shared pool; a nil pool means an in-memory
// conversation.
//
// This is the only flags-to-options mapping. The daemon builds argv with
// buildAgentArgv and parses it back rather than populating RuntimeOptions
// directly, so ExtraArgs and last-flag-wins behave identically for both
// execution models and no field can be dropped on one path only.
func (f agentFlags) toRuntimeOptions(cwd string, pool *pgxpool.Pool) (agent.RuntimeOptions, error) {
	thinkingBudget, err := agent.ThinkingBudgetFor(f.thinking)
	if err != nil {
		return agent.RuntimeOptions{}, err
	}

	// Mirror runAgent's MCP asymmetry: an explicit --mcp-config that does not
	// exist is an error (BuildRuntime raises it), while an absent defaulted
	// <cwd>/.mcp.json is skipped by passing an empty path.
	mcpPath := resolveMCPConfig(f.mcpConfig, cwd)
	if f.mcpConfig == "" {
		if _, statErr := os.Stat(mcpPath); statErr != nil {
			mcpPath = ""
		}
	}

	return agent.RuntimeOptions{
		Model:                f.model,
		ThinkingBudget:       thinkingBudget,
		SystemPromptOverride: f.systemPrompt,
		AppendSystemPrompt:   f.appendSystemPrompt,
		Cwd:                  cwd,
		Ref:                  f.ref,
		Name:                 f.name,
		SpillDir:             f.spillDir,
		SkillsDirs:           assembleSkillDirs(cwd, f.skillsDir),
		Skills:               f.skills,
		NoSkills:             f.noSkills,
		NoContextFiles:       f.noContextFiles,
		MCPConfig:            mcpPath,
		FakeTurns:            f.fakeTurns,
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		OpenRouterAPIKey:     os.Getenv("OPENROUTER_API_KEY"),
		Pool:                 pool,
	}, nil
}
```

Add `"os"` to the imports. If `agentFlags`' field names differ from those above, use the real ones — read `cmd/fundid/agent.go:30-47`.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./cmd/fundid/ -run 'TestArgvRoundTrips|TestExtraArgsOverride|TestAgentRefIsDaemonControlled' -v 2>&1 | tee /tmp/t5.log
grep -E '=== RUN|--- (PASS|FAIL)' /tmp/t5.log
```

Expected: all three PASS.

- [ ] **Step 5: Wire the three spawn sites**

Add to `controller.go`:

```go
// agentRunner builds the in-process runner for an agent child. Returns a nil
// Runner for every other kind, which leaves SpawnSpec on the subprocess path.
func (c *Controller) agentRunner(req protocol.SpawnRequest, childID string) (child.Runner, error) {
	if req.Kind != "agent" {
		return nil, nil
	}
	argv := appendDaemonRef(buildAgentArgv(req, childID, c.stateDir), childID)
	f, err := parseAgentFlags(argv[1:]) // argv[0] is the "agent" subcommand
	if err != nil {
		return nil, fmt.Errorf("agent flags: %w", err)
	}
	ro, err := f.toRuntimeOptions(req.Cwd, c.pool)
	if err != nil {
		return nil, fmt.Errorf("agent runtime options: %w", err)
	}
	return inproc.New(inproc.Options{
		ChildID: childID,
		Parent:  c.baseCtx,
		Runtime: ro,
	}), nil
}
```

At **each** of the three `SpawnSpec` construction sites, set `Runner` from `agentRunner` and surface the error as `protocol.ErrSpawnFailed`. `Resume` and `RespawnChild` reconstruct a `SpawnRequest` from a stored snapshot; use the same reconstruction they already feed to `resolveSpawnPlan` so all three paths agree. Missing one leaves that path silently on the subprocess model — grep to confirm:

```bash
grep -n 'SpawnSpec{' cmd/fundid/controller.go   # expect 3; each must set Runner
```

For the agent kind leave `PiBinary` and `Argv` empty on the spec: a stale argv there is dead configuration that reads as live. Keep `buildAgentArgv` — it is now the config source that `agentRunner` parses, not an exec argv.

`c.pool` and `c.baseCtx` are new fields. Add both as `NewController` parameters and pass them from `main.go`.

- [ ] **Step 6: Wire `main.go`**

Read `FUNDI_AGENT_DB` through `internal/envvar` (the single source of truth for env names), open one pool when set, hand it and `ctx` to `NewController`, and close it after `ShutdownAllChildren` returns. Log at info whether a pool was opened:

```go
	if dsn := os.Getenv(envvar.AgentDB); dsn != "" {
		pool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			slog.Error("open agent database", "error", err)
			os.Exit(1)
		}
		defer pool.Close()
		slog.Info("agent database pool opened")
	} else {
		slog.Warn("no agent database configured; conversations are in-memory and no cost data is recorded",
			"env", envvar.AgentDB)
	}
```

That warning is deliberate. The `mem-…` sessionId was the only tell before, and a silent nil pool is the failure this whole design exists to remove.

- [ ] **Step 7: Run the full suite**

```bash
go build ./... && go test ./... -race 2>&1 | tee /tmp/t5-all.log
grep -E '^(FAIL|--- FAIL)' /tmp/t5-all.log || echo "no failures"
```

- [ ] **Step 8: Commit**

```bash
git add cmd/fundid/agent_runtime.go cmd/fundid/agent_runtime_test.go cmd/fundid/controller.go cmd/fundid/main.go
git diff --cached --stat
git commit -m "feat(fundid): run agent children in-process on a shared pool

The agent kind no longer re-execs the daemon binary. The daemon owns one
pgxpool, passes its context so every in-process child is cancelled on
shutdown, and wires all three spawn sites (Spawn, Resume, RespawnChild).

Per-child config is not re-mapped by hand: buildAgentArgv stays and the
daemon parses its output with the existing pure parseAgentFlags. One mapping,
so ExtraArgs and last-flag-wins keep working and no field can be dropped on
one path only. --ref is appended last so the daemon's child id beats any
caller-supplied override."
```

---

### Task 6: End-to-end verification

No new production code. This proves the phase against a live daemon and re-runs the checks that caught the streaming bugs, because every one of those shipped with a green suite.

**Files:**
- Modify: `docs/plans/2026-07-30-execution-and-storage-design.md`

- [ ] **Step 1: Full gates**

```bash
go vet ./... > /tmp/v.log 2>&1; test ! -s /tmp/v.log && echo "vet clean" || cat /tmp/v.log
golangci-lint run ./... --max-same-issues=0 --max-issues-per-linter=0 > /tmp/l.log 2>&1; wc -l < /tmp/l.log
GOOS=linux go build ./... && echo "linux build ok"
go test ./... -race > /tmp/all.log 2>&1; grep -E '^(FAIL|--- FAIL)' /tmp/all.log || echo "no failures"
```

Expected: vet clean, lint zero lines, linux build ok, no failures. Do not pipe into `tail` and read `$?` — that reports `tail`.

- [ ] **Step 2: Confirm no subprocess is spawned for an agent child**

```bash
make build
pkill -f 'bin/fundid' || true
mkdir -p /tmp/fundi-p1
cd ~/home/fundi && set -a && . ./.env && set +a && ./bin/fundid &
sleep 2
./bin/fundi create --detached --kind agent --model anthropic/claude-sonnet-5 --cwd /tmp/fundi-p1 p1-check
ps -o pid,ppid,command | grep 'fundid agent' | grep -v grep && echo "FAIL: a subprocess still exists" || echo "PASS: no 'fundid agent' subprocess"
./bin/fundi get p1-check | grep -E '"pid"'
```

Expected: no `fundid agent` process; the child's reported pid is `0`.

- [ ] **Step 3: Re-run the streaming assertion on intermediate state**

```bash
./bin/fundi tail p1-check --raw --no-deltas=false -n 0 > /tmp/p1-frames.jsonl &
sleep 1
./bin/fundi send p1-check '{"type":"prompt","message":"Explain unix domain sockets in three paragraphs. No tools."}'
sleep 45
jq -rc 'select(.type=="message_update")|([.message.content[]?|select(.type=="text")|.text|length]|add // 0)' /tmp/p1-frames.jsonl \
  | awk 'NR>1 && $1<p {print "DECREASE: "p" -> "$1; bad=1} $1==0 {print "EMPTY FRAME"; bad=1} {p=$1} END{if(!bad) print "PASS: monotonic, no empty frames ("NR" deltas)"}'
```

Expected: `PASS` with a delta count in the tens. A count of 0 or 1 means streaming regressed to whole-message delivery.

- [ ] **Step 4: Abort, liveness, and shutdown timing**

```bash
./bin/fundi send p1-check '{"type":"prompt","message":"Write a long essay on virtual memory. No tools."}'
sleep 5
./bin/fundi send p1-check '{"type":"abort"}'
sleep 3
./bin/fundi get p1-check | grep -E '"status"'     # must not be exited
./bin/fundi send p1-check '{"type":"prompt","message":"Say: still alive. No tools."}'
sleep 15
./bin/fundi recent p1-check | grep -c 'still alive'
time ./bin/fundi kill p1-check
```

Expected: abort leaves the child alive and idle (proving `Interrupt` maps to `HandleAbort`, not to termination); the next prompt answers; `kill` returns in about a second rather than tens of seconds. Confirm the daemon log has no `panic` and no `level=ERROR`.

- [ ] **Step 5: Confirm pi and claude are untouched**

```bash
./bin/fundi create --detached --kind pi --cwd /tmp/fundi-p1 p1-pi
./bin/fundi get p1-pi | grep -E '"pid"'   # must be a real, non-zero pid
./bin/fundi kill p1-pi
```

Expected: a non-zero pid. The pi and claude kinds must still be subprocesses; a pid of 0 here means the runner was wired for the wrong kinds.

- [ ] **Step 6: Update the design doc**

Record against the design's open questions: whether `renderRing` was affected (expected: untouched, since claude is still a subprocess), and the spill-dir scoping this phase settled. Note the two known behaviour changes from the section below so phase 2 does not rediscover them.

- [ ] **Step 7: Commit**

```bash
git add docs/plans/2026-07-30-execution-and-storage-design.md
git diff --cached --stat
git commit -m "docs(plans): record phase 1a verification outcomes"
```

---

## Known behaviour changes

Both are consequences of the design, not defects, but they are user-visible and must be in the phase's release notes.

1. **`fundi logs --err <agent-child>` returns empty.** An in-process agent has no separate stderr; its diagnostics are daemon log lines tagged with the child id. `err.log.gz` is no longer written for agent children. pi and claude are unaffected. The design intends this — `logs` becomes a query in phase 2 — but between the two phases there is a real gap in agent-child diagnostics, and the workaround is reading the daemon log.
2. **`fundi get <agent-child>` reports `pid: 0`.** There is no process. `loadOrphans` already guards with `if rec.PID > 0`, so nothing tries to signal it, but any external tooling that treats pid 0 as an error needs updating.

## Out of scope for this plan

Each is its own plan.

- **Step 1b — deleting the serialization.** The `io.Pipe` stays. Removing the JSON round-trip, the 2× payload duplication, and the `JSON.raw` bug class comes after 1a is verified in real use.
- **Telemetry wiring.** In-process execution *enables* per-conversation OTLP spans and Prometheus gauges; it does not add them. The design lists them as unlocked benefits, not deliverables.
- **Phase 2 — the database as source of truth.** The ring, `renderRing`, `exitedRing`, `persist.Record`, the grace sweeper, and `childClaimSet` all survive untouched. `forget` keeps its current meaning.
- **Making the database mandatory.** A nil pool still means an in-memory conversation. The `mem-…` fallback and the service-template env passthrough are phase 2.
- **pi/claude proxy capture and the correlation ref.** No `ANTHROPIC_BASE_URL` plumbing here.
