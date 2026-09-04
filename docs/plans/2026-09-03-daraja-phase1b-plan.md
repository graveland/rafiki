# daraja Phase 1b-i — the machine half

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An executor can launch, supervise and reap a daraja hosting a real
claude, with one stable process group per child and one shared argv builder —
all verifiable on one machine with no rafikid involved.

**Architecture:** A new `AdminService` is mounted as a second Connect handler on
the mux the executor already builds, keeping `ExecutorService` exactly the
executor. Its `Launch` verb spawns `rafiki daraja serve` in a fresh process
group and returns that pgid; daraja spawns claude **without** `Setpgid` so claude
inherits the group, which makes one handle reach both processes for the child's
whole life. Both `Launch` and daraja's existing `Restart` carry a typed
`ChildSpec` rather than raw argv, and both build claude's command line through
one `pkg/claudeargv.Build` — they are the same binary, so that is one builder by
construction.

**Tech Stack:** Go 1.26, connect-go v1.20.0, protobuf (`protoc` + committed
generated code), cobra, unencrypted HTTP/2 over a unix socket.

**Spec:** `docs/plans/2026-08-31-daraja-design.md` (amended 2026-09-03 — read the
sections "AdminService and the launch RPC", "Launchability", "Nothing is lost
across a disconnect", and "The child's process group" before starting).

## Global Constraints

- **`make check` is the only gate — there is no CI.** Run it before considering
  any task done. It is vet + golangci-lint + unit tests with `-race`.
- **`make check` fails with two `test/integration` failures if another rafiki
  daemon is running on the machine** (port `:8035` collision). Bypass with
  `RAFIKI_PROXY_LISTEN=127.0.0.1:18035 make check`. Those two failures are not
  yours.
- **Capture the gate's exit status directly:** `make check > /tmp/check.log 2>&1;
  echo $?`. Piping to `tail` reports *tail's* status and hides a lint failure.
- **`make check` reads the working tree** — nothing may edit it concurrently.
- **This dev environment wraps `go test` through `rtk`, which compacts output to
  a bare pass count and swallows SKIP lines.** When you need to prove a test
  actually ran, invoke the toolchain directly: `/usr/local/go/bin/go test ...`.
- **No `Co-Authored-By` trailers in commit messages.**
- **Generated protobuf code is committed.** `make proto` regenerates it; never
  hand-edit a `.pb.go` or `.connect.go`.
- **`cmd/rafiki` must link zero pgx packages** (`TestClientDoesNotLinkPostgres`).
  Everything in this plan lives in `cmd/rafiki`, `pkg/daraja`, `pkg/executor`,
  `pkg/child` and two new pgx-free packages, so this holds — but do not reach for
  a store or a DSN anywhere in it.
- **`gofmt -w` after touching any aligned `const`/`var` block** — gofmt realigns
  the whole block, and hand-aligning only your own line leaves pre-existing lines
  dirty and the lint gate red.
- **Do not add trivial comments.** Match the surrounding density, which in this
  repo is high but load-bearing: comments explain *why*, not *what*.
- **`errcheck` is on.** `defer h.Shutdown(time.Second)` fails the gate; the
  repo's pattern is `_, _ = c.Shutdown(...)` (see `pkg/child` tests).
- **`h2c.NewHandler` is deprecated and staticcheck fails on it.** Use
  `http.Protocols` + `SetUnencryptedHTTP2(true)`, as `cmd/rafiki/cmd_daraja.go`
  and `cmd/rafikid/connect_uds.go` already do.

---

## File Structure

**Created:**

| Path | Responsibility |
|---|---|
| `pkg/claudeargv/claudeargv.go` | The single argv builder. Pure function, no dependencies beyond stdlib. |
| `pkg/claudeargv/claudeargv_test.go` | Its tests. |
| `proto/rafiki/admin/v1/admin.proto` | `AdminService`: `Launch`, `Reap`. Imports daraja's `ChildSpec`. |
| `pkg/adminpb/` | Generated code for the above (committed). |
| `pkg/executor/admin.go` | `AdminServer`: launch, supervise, reap. Owns the pgid registry. |
| `pkg/executor/admin_test.go` | Its tests. |

**Modified:**

| Path | Change |
|---|---|
| `Makefile` | Unify proto generation on `--proto_path=proto`; add the admin block. |
| `proto/rafiki/daraja/v1/daraja.proto` | Add `ChildSpec`/`ClaudeParams`; `RestartRequest` carries `ChildSpec` instead of `repeated string argv`. |
| `pkg/child/child.go` | `SpawnSpec.InheritProcessGroup`. |
| `pkg/child/runner.go` | Honour it in `newProcessRunner`. |
| `pkg/daraja/host.go` | Hold a `ChildSpec`; build argv from it; crash-respawn with a cap. |
| `pkg/daraja/server.go` | Single Relay attachment; preserve an undelivered event. |
| `cmd/rafiki/cmd_daraja.go` | Typed `--kind/--model/--resume/--permission-mode` flags replace raw argv after `--`. |
| `cmd/rafiki/cmd_executor_serve.go` | `--launch` flag; mount `AdminService` on the mux. |
| `pkg/executor/server.go` | `Describe` reports `launch_kinds`. |
| `proto/rafiki/executor/v1/executor.proto` | `DescribeResponse.launch_kinds`. |

**Not in this plan** (that is 1b-ii): `upgradeconn.Daraja`, daraja's reverse dial
and reconnect credential, `pkg/darajapool`, rafikid mounting, and
`rafiki daraja launch`. Here `Launch` starts daraja on a **unix socket on the
executor's own machine** and returns the path, exactly as 1a dialled one — 1b-ii
replaces that transport without changing either service.

---

### Task 1: Unify proto generation on `--proto_path=proto`

No behaviour change. It is first because `admin.proto` must import
`rafiki/daraja/v1/daraja.proto`, and a proto file's **descriptor name** is its
path relative to `--proto_path`. Today `daraja.proto` is generated with
`--proto_path=proto/rafiki/daraja/v1`, so it registers itself as `daraja.proto`.
An `admin.pb.go` whose dependency list names `rafiki/daraja/v1/daraja.proto`
would then fail to resolve **at package init**, as a panic rather than a compile
error. Changing the path afterwards means regenerating twice and debugging a
panic in between.

**Files:**
- Modify: `Makefile:186-204`

**Interfaces:**
- Consumes: nothing.
- Produces: `pkg/darajapb` regenerated with descriptor name
  `rafiki/daraja/v1/daraja.proto`. Go import path and package name are unchanged
  (`go.graveland.dev/rafiki/pkg/darajapb`, package `darajapb`), so no Go source
  changes.

- [ ] **Step 1: Record the current descriptor name**

Run:
```bash
/usr/local/go/bin/go run - <<'EOF'
package main

import (
	"fmt"

	_ "go.graveland.dev/rafiki/pkg/darajapb"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func main() {
	protoregistry.GlobalFiles.RangeFiles(func(fd interface {
		Path() string
	}) bool {
		fmt.Println(fd.Path())
		return true
	})
}
EOF
```

If that helper is awkward to run, `grep -o 'rafiki/daraja/v1/daraja.proto\|daraja\.proto' pkg/darajapb/daraja.pb.go | sort -u` is enough.

Expected: `daraja.proto` (the un-prefixed name).

- [ ] **Step 2: Change the daraja generation block**

Replace `Makefile` lines 195-204 (the `mkdir -p pkg/darajapb` block through
`gofmt -w pkg/darajapb`) with:

```makefile
	mkdir -p pkg/darajapb
	$(PROTOC) \
		--plugin=protoc-gen-go=bin/protoc-gen-go \
		--plugin=protoc-gen-connect-go=bin/protoc-gen-connect-go \
		--proto_path=proto \
		--go_out=pkg/darajapb --go_opt=module=go.graveland.dev/rafiki/pkg/darajapb \
		--connect-go_out=pkg/darajapb --connect-go_opt=module=go.graveland.dev/rafiki/pkg/darajapb \
		proto/rafiki/daraja/v1/daraja.proto
	gofmt -w pkg/darajapb
```

The `module=` option strips the Go module prefix from the output path, which is
how `proto/rafiki/v1/*.proto` is already generated (line 210). Combined with
`option go_package = ".../pkg/darajapb;darajapb"`, the files land at
`pkg/darajapb/daraja.pb.go` and `pkg/darajapb/darajapbconnect/daraja.connect.go`
— the same layout as before.

- [ ] **Step 3: Regenerate and confirm the layout did not move**

Run:
```bash
make proto
git status --short pkg/darajapb
```

Expected: `pkg/darajapb/daraja.pb.go` and
`pkg/darajapb/darajapbconnect/daraja.connect.go` are MODIFIED, and no files are
added or deleted. If protoc emitted a nested `rafiki/daraja/v1/` directory, the
`module=` option is missing or misspelled — fix it and delete the stray tree
before continuing.

- [ ] **Step 4: Confirm the descriptor name changed**

Run: `grep -c 'rafiki/daraja/v1/daraja.proto' pkg/darajapb/daraja.pb.go`

Expected: at least 1. This is the whole point of the task.

- [ ] **Step 5: Run the gate**

Run: `make check > /tmp/check.log 2>&1; echo $?`
Expected: `0`. Nothing in Go changed, so a failure here means the layout moved.

- [ ] **Step 6: Commit**

```bash
git add Makefile pkg/darajapb
git commit -m "proto: generate darajapb from the repo-root proto path

A proto file's descriptor name is its path relative to --proto_path, and
daraja.proto registered itself as \"daraja.proto\". A second file importing it as
\"rafiki/daraja/v1/daraja.proto\" would fail to resolve the dependency at package
init — a panic, not a compile error. Generating from proto/ makes the descriptor
name match the import path, as proto/rafiki/v1 already does."
```

---

### Task 2: `pkg/claudeargv` — the one argv builder

**Files:**
- Create: `pkg/claudeargv/claudeargv.go`
- Test: `pkg/claudeargv/claudeargv_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  package claudeargv

  type Params struct {
      Model          string
      ResumeSession  string
      PermissionMode string
  }

  func Build(p Params) []string
  ```
  `Build` returns argv **excluding** the binary itself, matching
  `child.SpawnSpec.Argv`. Consumed by Task 3 (daraja's `Restart`) and Task 10
  (the executor's `Launch`).

- [ ] **Step 1: Write the failing test**

Create `pkg/claudeargv/claudeargv_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package claudeargv

import (
	"slices"
	"strings"
	"testing"
)

// The base flags are what makes claude speak the stream-json protocol daraja
// relays and rafikid parses. A build that omits any of them produces a child
// that runs and is unintelligible, which is far worse than one that fails.
func TestBuildAlwaysCarriesTheStreamJSONContract(t *testing.T) {
	got := Build(Params{})
	for _, want := range []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Build() = %v, missing %q", got, want)
		}
	}
}

func TestBuildOmitsEmptyOptionalFlags(t *testing.T) {
	got := strings.Join(Build(Params{}), " ")
	for _, unwanted := range []string{"--model", "--resume", "--permission-mode"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("Build(zero) = %q, should not carry %q", got, unwanted)
		}
	}
}

func TestBuildCarriesModelAndResume(t *testing.T) {
	got := Build(Params{Model: "claude-opus-5", ResumeSession: "abc-123"})
	assertPair(t, got, "--model", "claude-opus-5")
	assertPair(t, got, "--resume", "abc-123")
}

// bypassPermissions is the one permission mode that is a bare flag rather than
// a --permission-mode value; claude rejects `--permission-mode
// bypassPermissions`.
func TestBuildMapsBypassToItsOwnFlag(t *testing.T) {
	got := Build(Params{PermissionMode: "bypassPermissions"})
	if !slices.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("Build(bypassPermissions) = %v, want --dangerously-skip-permissions", got)
	}
	if slices.Contains(got, "--permission-mode") {
		t.Errorf("Build(bypassPermissions) = %v, should not also pass --permission-mode", got)
	}
}

func TestBuildPassesOtherPermissionModesThrough(t *testing.T) {
	assertPair(t, Build(Params{PermissionMode: "acceptEdits"}), "--permission-mode", "acceptEdits")
}

// Build must not hand its caller a slice that aliases package state; a caller
// appending to the result would corrupt the next build.
func TestBuildReturnsAFreshSlice(t *testing.T) {
	a := Build(Params{Model: "m1"})
	b := Build(Params{Model: "m2"})
	if slices.Equal(a, b) {
		t.Fatal("two builds with different models returned equal argv")
	}
	a = append(a, "--sentinel")
	if slices.Contains(Build(Params{Model: "m1"}), "--sentinel") {
		t.Error("appending to a returned slice leaked into a later build")
	}
}

func assertPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i, a := range argv {
		if a == flag {
			if i+1 >= len(argv) || argv[i+1] != value {
				t.Errorf("argv %v: %s is not followed by %q", argv, flag, value)
			}
			return
		}
	}
	t.Errorf("argv %v: missing %s", argv, flag)
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `/usr/local/go/bin/go test ./pkg/claudeargv/ -v`
Expected: FAIL — the package does not compile, `undefined: Build`.

- [ ] **Step 3: Write the implementation**

Create `pkg/claudeargv/claudeargv.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Package claudeargv builds claude's command line from typed parameters.
//
// It exists so there is exactly ONE builder. Two callers need it — the
// executor's AdminService when it launches a daraja, and daraja itself when it
// restarts or respawns the child — and both live in the `rafiki` binary, so a
// single function here is one builder by construction rather than two kept in
// step by discipline.
package claudeargv

// bypassPermissions is claude's own spelling for the mode that has its own
// flag rather than a --permission-mode value.
const bypassPermissions = "bypassPermissions"

// Params is what a caller may vary. Everything else about claude's invocation
// is fixed by the protocol daraja relays.
type Params struct {
	Model          string
	ResumeSession  string
	PermissionMode string
}

// Build returns argv EXCLUDING the binary itself, matching
// child.SpawnSpec.Argv.
//
// The four base flags are the stream-json contract: -p makes claude headless,
// the two format flags select the newline-delimited JSON protocol, and
// --verbose is what makes it emit the per-turn frames rather than only a final
// result. Dropping any of them yields a child that runs and cannot be parsed.
func Build(p Params) []string {
	argv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	if p.ResumeSession != "" {
		argv = append(argv, "--resume", p.ResumeSession)
	}
	switch p.PermissionMode {
	case "":
	case bypassPermissions:
		argv = append(argv, "--dangerously-skip-permissions")
	default:
		argv = append(argv, "--permission-mode", p.PermissionMode)
	}
	return argv
}
```

- [ ] **Step 4: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/claudeargv/ -v`
Expected: PASS, 6 tests.

- [ ] **Step 5: Commit**

```bash
git add pkg/claudeargv
git commit -m "claudeargv: one builder for claude's command line

The executor builds it at launch and daraja rebuilds it on restart and on
crash-respawn. Both are subcommands of the same binary, so a shared function is
one builder by construction — the alternative was a copy on each side of an RPC
boundary, which is the cross-boundary drift this repo keeps getting bitten by."
```

---

### Task 3: `ChildSpec` on the wire; `Restart` stops taking raw argv

**Files:**
- Modify: `proto/rafiki/daraja/v1/daraja.proto:66-73`
- Modify: `pkg/daraja/host.go:242` (`Restart` signature)
- Modify: `pkg/daraja/server.go:47-55` (`Restart` handler)
- Modify: `pkg/daraja/host_test.go` (callers of `Restart`)

**Interfaces:**
- Consumes: `claudeargv.Build` (Task 2).
- Produces:
  - proto `rafiki.daraja.v1.ChildSpec{Kind kind, ClaudeParams claude}` and
    `ClaudeParams{model, resume_session, permission_mode}`, imported by Task 8's
    `admin.proto`.
  - Go: `func (h *Host) Restart(spec ChildSpec, grace time.Duration) (int, error)`
    where `ChildSpec` is the daraja-package struct defined below. Consumed by
    Tasks 5, 6 and 7.

- [ ] **Step 1: Amend the proto**

In `proto/rafiki/daraja/v1/daraja.proto`, replace `message RestartRequest`
(lines 66-73) with:

```protobuf
// Kind names the child protocol daraja is hosting. daraja's RELAY is entirely
// protocol-blind; only its spawn path consults this, to pick an argv builder.
// A second stdio kind is therefore additive rather than a breaking change.
enum Kind {
  KIND_UNSPECIFIED = 0;
  KIND_CLAUDE = 1;
}

message ClaudeParams {
  string model = 1;
  // resume_session is the claude session id to continue. It is the whole
  // recovery story: a daraja and its child are disposable precisely because
  // this id is a persisted column on conversations.child, so a replacement can
  // always pick the conversation back up.
  string resume_session = 2;
  string permission_mode = 3;
}

// ChildSpec is everything needed to (re)build the child's command line.
//
// Typed rather than raw argv because the executor's Launch and daraja's Restart
// would otherwise each need an argv builder, on opposite sides of an RPC. Both
// call pkg/claudeargv instead.
message ChildSpec {
  Kind kind = 1;
  ClaudeParams claude = 2;
}

message RestartRequest {
  // spec replaces the child. When absent, daraja reuses the spec it currently
  // holds — which is how a caller restarts without restating everything it
  // already knows.
  ChildSpec spec = 1;
  // grace_ms to wait after the interrupt before escalating to a hard kill.
  // Zero means the server default.
  int32 grace_ms = 2;
}
```

- [ ] **Step 2: Regenerate and confirm the build breaks where expected**

Run:
```bash
make proto
/usr/local/go/bin/go build ./... 2>&1 | head -20
```

Expected: FAIL, citing `req.Msg.GetArgv` in `pkg/daraja/server.go`. That is the
one call site the wire change reaches, and seeing it is the point of this step.

- [ ] **Step 3: Give `Host` a spec it holds**

In `pkg/daraja/host.go`, add to the imports `"go.graveland.dev/rafiki/pkg/claudeargv"`,
and add above `HostOptions`:

```go
// ChildSpec is the typed description of the child process, held so daraja can
// rebuild the command line without being told it again — on a caller's Restart
// that omits one, and on its own respawn after an unexpected exit.
type ChildSpec struct {
	Kind          string
	Model         string
	ResumeSession string
	PermissionMode string
}

// KindClaude is the only child protocol daraja hosts today.
const KindClaude = "claude"

// argv builds the child's command line for this spec. An unknown kind returns
// nil, which startLocked reports as an error rather than launching a bare
// binary with no arguments — a claude with no --output-format runs, and emits
// something nothing downstream can parse.
func (s ChildSpec) argv() []string {
	if s.Kind != KindClaude {
		return nil
	}
	return claudeargv.Build(claudeargv.Params{
		Model:          s.Model,
		ResumeSession:  s.ResumeSession,
		PermissionMode: s.PermissionMode,
	})
}
```

Replace the `Argv []string` field in `HostOptions` with `Spec ChildSpec`, and add
a `spec ChildSpec` field to `Host` guarded by `h.mu`. In `NewHost`, seed it:
`h.spec = opts.Spec`.

- [ ] **Step 4: Build argv from the spec**

In `Host.Start`, replace `h.startLocked(h.opts.Argv)` with `h.startLocked(h.spec)`.

Change `startLocked` to take a `ChildSpec` and resolve argv itself:

```go
func (h *Host) startLocked(spec ChildSpec) (io.ReadCloser, error) {
	if h.running {
		return nil, errors.New("daraja: already running")
	}
	argv := spec.argv()
	if argv == nil {
		return nil, fmt.Errorf("daraja: unsupported child kind %q", spec.Kind)
	}
	h.spec = spec
	runner, err := child.NewProcessRunner(child.SpawnSpec{
		PiBinary:    h.opts.Binary,
		Argv:        argv,
		Cwd:         h.opts.Cwd,
		Env:         h.opts.Env,
		EnvOverride: h.opts.EnvOverride,
	})
	// ... rest unchanged
}
```

Change `Restart`'s signature and body head:

```go
// Restart signals the process, waits for it, and launches a replacement.
//
// A zero spec means "reuse the one I am holding", which is what a caller
// restarting for any reason other than changing the child's parameters wants.
func (h *Host) Restart(spec ChildSpec, grace time.Duration) (int, error) {
	h.mu.Lock()
	if spec == (ChildSpec{}) {
		spec = h.spec
	}
	if h.running {
		h.stopLocked(grace, true)
	}
	stdout, err := h.startLocked(spec)
	// ... rest unchanged
```

- [ ] **Step 5: Update the Connect handler**

In `pkg/daraja/server.go`, replace the body of `Restart`:

```go
func (s *Server) Restart(
	ctx context.Context, req *connect.Request[darajapb.RestartRequest],
) (*connect.Response[darajapb.RestartResponse], error) {
	pid, err := s.host.Restart(
		specFromProto(req.Msg.GetSpec()),
		time.Duration(req.Msg.GetGraceMs())*time.Millisecond,
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&darajapb.RestartResponse{Pid: int32(pid)}), nil
}

// specFromProto maps the wire spec onto the host's. A nil message yields the
// zero ChildSpec, which Restart reads as "reuse what you hold".
func specFromProto(p *darajapb.ChildSpec) ChildSpec {
	if p == nil || p.GetKind() != darajapb.Kind_KIND_CLAUDE {
		return ChildSpec{}
	}
	c := p.GetClaude()
	return ChildSpec{
		Kind:           KindClaude,
		Model:          c.GetModel(),
		ResumeSession:  c.GetResumeSession(),
		PermissionMode: c.GetPermissionMode(),
	}
}
```

- [ ] **Step 6: Fix the existing tests and add one for spec reuse**

Update every `HostOptions{... Argv: ...}` and `Restart(argv, ...)` call in
`pkg/daraja/host_test.go` and `pkg/daraja/server_test.go` to the new shapes. The
existing tests use a fake binary rather than claude, so give them
`Spec: ChildSpec{Kind: KindClaude}` and let `Build` supply the base flags — the
fake ignores them.

Add to `pkg/daraja/host_test.go`:

```go
// A Restart with no spec must reuse the held one. The alternative — treating an
// absent spec as an empty one — would relaunch claude with no --output-format
// and no --resume: a running process that emits nothing parseable and has lost
// the conversation.
func TestRestartWithNoSpecReusesTheHeldOne(t *testing.T) {
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "m1", ResumeSession: "s1"},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	if _, err := h.Restart(ChildSpec{}, time.Second); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	h.mu.Lock()
	got := h.spec
	h.mu.Unlock()
	if got.Model != "m1" || got.ResumeSession != "s1" {
		t.Errorf("after a spec-less Restart the host holds %+v, want the original", got)
	}
}
```

`testEchoBinary` is whatever helper `host_test.go` already uses to get a
long-lived fake child; reuse it rather than adding a second one. If none exists,
`/bin/cat` is a process that stays up until its stdin closes.

- [ ] **Step 7: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/daraja/ -race -count=1 -v`
Expected: PASS, including `TestRestartWithNoSpecReusesTheHeldOne` and 1a's
`TestHostRestartEmitsBoundaryMarker`.

- [ ] **Step 8: Commit**

```bash
git add proto/rafiki/daraja/v1/daraja.proto pkg/darajapb pkg/daraja
git commit -m "daraja: Restart takes a typed ChildSpec, not raw argv

1a shipped RestartRequest{repeated string argv}, which put one argv builder in
the caller and would have put a second in the executor's Launch handler. Both
verbs are typed now and both go through pkg/claudeargv.

An absent spec means \"reuse the one you hold\" so a caller restarting for any
other reason need not restate the model and session it is not changing."
```

---

### Task 4: `SpawnSpec.InheritProcessGroup`

The child's process group is the whole reaping design: the executor makes daraja
a group leader, daraja spawns claude **without** `Setpgid` so claude joins that
group, and one `kill(-pgid)` then reaches both for the child's whole life —
including after a SIGKILLed daraja orphans claude to launchd, because a process
group outlives its leader while any member remains.

`newProcessRunner` hardcodes `Setpgid: true`, which would put claude in its own
group and silently restore the split-group problem with no visible symptom.

**Files:**
- Modify: `pkg/child/child.go:24-31` (`SpawnSpec`)
- Modify: `pkg/child/runner.go:52-66` (`newProcessRunner`)
- Test: `pkg/child/runner_test.go` (create if absent)

**Interfaces:**
- Consumes: nothing.
- Produces: `SpawnSpec.InheritProcessGroup bool`. Consumed by Task 5.

- [ ] **Step 1: Write the failing test**

Add to `pkg/child/runner_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package child

import (
	"os"
	"syscall"
	"testing"
)

// Setpgid is the default because a child that spawns subprocesses must be
// signallable as a group. daraja needs the opposite: its claude must JOIN
// daraja's group, so that one kill(-pgid) from the executor reaches both and
// keeps reaching claude after daraja is SIGKILLed.
//
// Nothing else observes this, so a regression is silent — hence a test that
// reads the real pgid out of the kernel.
func TestInheritProcessGroupPutsTheChildInOurGroup(t *testing.T) {
	r, err := newProcessRunner(SpawnSpec{
		PiBinary:            "/bin/cat",
		InheritProcessGroup: true,
	})
	if err != nil {
		t.Fatalf("newProcessRunner: %v", err)
	}
	stdin, _, _, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stdin.Close(); _, _ = r.Wait() }()

	got, err := syscall.Getpgid(r.PID())
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", r.PID(), err)
	}
	if want := syscall.Getpgrp(); got != want {
		t.Errorf("child pgid = %d, want our own %d", got, want)
	}
	_ = os.Getpid()
}

func TestDefaultStillGivesTheChildItsOwnGroup(t *testing.T) {
	r, err := newProcessRunner(SpawnSpec{PiBinary: "/bin/cat"})
	if err != nil {
		t.Fatalf("newProcessRunner: %v", err)
	}
	stdin, _, _, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stdin.Close(); _, _ = r.Wait() }()

	got, err := syscall.Getpgid(r.PID())
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if got != r.PID() {
		t.Errorf("child pgid = %d, want its own pid %d", got, r.PID())
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `/usr/local/go/bin/go test ./pkg/child/ -run TestInheritProcessGroup -v`
Expected: FAIL to compile — `unknown field InheritProcessGroup`.

- [ ] **Step 3: Add the field**

In `pkg/child/child.go`, after `EnvOverride`:

```go
	// InheritProcessGroup leaves the child in the PARENT's process group
	// instead of giving it its own.
	//
	// The default (its own group) is what lets shutdown signal a child's whole
	// subprocess tree. daraja needs the inverse: the executor makes daraja a
	// group leader and daraja's claude joins that group, so one kill(-pgid)
	// reaches both — and keeps reaching claude after a SIGKILLed daraja orphans
	// it to launchd, because a process group outlives its leader while any
	// member remains. darwin has no PR_SET_CHILD_SUBREAPER, so the group is the
	// only handle that survives that.
	InheritProcessGroup bool
```

- [ ] **Step 4: Honour it**

In `pkg/child/runner.go`, replace the unconditional assignment:

```go
	if !spec.InheritProcessGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
```

- [ ] **Step 5: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/child/ -race -count=1 -run TestInherit\|TestDefaultStill -v`
Expected: PASS, 2 tests. Confirm both actually RAN — `rtk` swallows skips, so use
the direct toolchain path as written.

- [ ] **Step 6: Run the gate**

Run: `make check > /tmp/check.log 2>&1; echo $?`
Expected: `0`. `pkg/child` is load-bearing for every child kind; a regression
here breaks fundi too.

- [ ] **Step 7: Commit**

```bash
git add pkg/child
git commit -m "child: let a spawn inherit the parent's process group

daraja's claude must join daraja's group rather than lead its own, so that one
kill(-pgid) from the executor reaches both processes — and keeps reaching claude
after a SIGKILLed daraja orphans it to launchd. darwin has no
PR_SET_CHILD_SUBREAPER, so the group is the only handle that survives that.

Nothing observes the pgid at runtime, so the tests read it out of the kernel."
```

---

### Task 5: `rafiki daraja serve` takes typed flags

**Files:**
- Modify: `cmd/rafiki/cmd_daraja.go:33-104`
- Modify: `cmd/rafiki/cmd_daraja_test.go:24-30`

**Interfaces:**
- Consumes: `daraja.ChildSpec` (Task 3), `SpawnSpec.InheritProcessGroup` (Task 4).
- Produces: the `daraja serve` CLI contract Task 10's `Launch` execs:
  `--socket`, `--binary`, `--cwd`, `--kind`, `--model`, `--resume`,
  `--permission-mode`. No positional args and no `--` passthrough.

- [ ] **Step 1: Replace the flags**

In `newDarajaServeCmd`, replace the `Use`/`Long`/flag block:

```go
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run one child process and serve its stdio over a socket",
		Long: "Hosts exactly one child process and relays its stdio. daraja dies with\n" +
			"its child and the child dies with daraja: there is no state to keep on\n" +
			"either side of that pair.\n\n" +
			"The child's command line is built from the typed flags below rather than\n" +
			"passed through, so that this and the executor's Launch RPC share one\n" +
			"builder (pkg/claudeargv) instead of keeping two in step.",
		Args: cobra.NoArgs,
		RunE: runDarajaServe,
	}
	cmd.Flags().String("socket", "", "unix socket to listen on (required)")
	cmd.Flags().String("binary", "", "child binary to run (required)")
	cmd.Flags().String("cwd", "", "working directory for the child")
	cmd.Flags().String("kind", "claude", "child protocol to host")
	cmd.Flags().String("model", "", "model to pass to the child")
	cmd.Flags().String("resume", "", "session id to resume")
	cmd.Flags().String("permission-mode", "", "child permission mode")
```

- [ ] **Step 2: Build the spec in `runDarajaServe`**

Replace the `host := daraja.NewHost(...)` line and the signature's use of `args`:

```go
func runDarajaServe(cmd *cobra.Command, _ []string) error {
	socket, _ := cmd.Flags().GetString("socket")
	binary, _ := cmd.Flags().GetString("binary")
	cwd, _ := cmd.Flags().GetString("cwd")
	kind, _ := cmd.Flags().GetString("kind")
	model, _ := cmd.Flags().GetString("model")
	resume, _ := cmd.Flags().GetString("resume")
	permMode, _ := cmd.Flags().GetString("permission-mode")
	if socket == "" {
		return errors.New("--socket is required")
	}
	if binary == "" {
		return errors.New("--binary is required")
	}

	host := daraja.NewHost(daraja.HostOptions{
		Binary: binary,
		Cwd:    cwd,
		Spec: daraja.ChildSpec{
			Kind:           kind,
			Model:          model,
			ResumeSession:  resume,
			PermissionMode: permMode,
		},
	})
```

The rest of the function is unchanged.

- [ ] **Step 3: Make the host inherit the group**

In `pkg/daraja/host.go`'s `startLocked`, add `InheritProcessGroup: true` to the
`child.SpawnSpec` literal, with:

```go
		// claude joins DARAJA's process group rather than leading its own, so
		// the executor's single kill(-pgid) reaches both — and still reaches
		// claude if daraja is SIGKILLed and cannot clean up. See
		// SpawnSpec.InheritProcessGroup.
		InheritProcessGroup: true,
```

- [ ] **Step 4: Update the CLI test**

In `cmd/rafiki/cmd_daraja_test.go`, replace the flag list the test asserts with
`socket`, `binary`, `cwd`, `kind`, `model`, `resume`, `permission-mode`.

- [ ] **Step 5: Run the tests**

Run: `/usr/local/go/bin/go test ./cmd/rafiki/ -run TestDaraja -race -count=1 -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/rafiki/cmd_daraja.go cmd/rafiki/cmd_daraja_test.go pkg/daraja/host.go
git commit -m "daraja serve: typed child flags, and claude joins daraja's group

The child's command line is built from --kind/--model/--resume/--permission-mode
through pkg/claudeargv rather than passed through after --, so the executor's
Launch and this share one builder.

claude now inherits daraja's process group, which is what makes a single
kill(-pgid) from the executor reach both processes for the child's whole life."
```

---

### Task 6: daraja respawns a child that dies on its own

`Host.watch` already emits `Event{Exited}` for an unexpected death
(`host.go:188`, guarded by `unexpected := h.runner == runner`). Today nothing
acts on it. When rafikid is reachable it could issue a `Restart` — but when the
connection is also down, nobody can, and the child is simply gone. daraja
respawning from its held spec covers both.

A cap is not optional: a claude that fails instantly on a bad `--resume` would
otherwise be respawned forever, at whatever rate the kernel can fork.

**Files:**
- Modify: `pkg/daraja/host.go`
- Test: `pkg/daraja/host_test.go`

**Interfaces:**
- Consumes: `ChildSpec` (Task 3).
- Produces: `HostOptions.RespawnLimit int` (0 means the default) and
  `HostOptions.RespawnBackoff time.Duration` (0 means the default). Consumed by
  Task 10 only through defaults.

- [ ] **Step 1: Write the failing tests**

Add to `pkg/daraja/host_test.go`:

```go
// A child that dies on its own must come back: when the controller's connection
// is also down, nothing else can restart it, and the alternative is a daraja
// hosting nothing.
func TestUnexpectedExitRespawnsTheChild(t *testing.T) {
	h := NewHost(HostOptions{
		Binary:         testShortLivedBinary(t),
		Spec:           ChildSpec{Kind: KindClaude},
		RespawnBackoff: time.Millisecond,
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	// The first exit is reported, then a replacement is announced.
	var sawExit, sawRestart bool
	deadline := time.After(10 * time.Second)
	for !sawRestart {
		select {
		case ev := <-h.Events():
			switch {
			case ev.Exited != nil:
				sawExit = true
			case ev.Restarted != nil:
				sawRestart = true
			}
		case <-deadline:
			t.Fatalf("timed out; sawExit=%v sawRestart=%v", sawExit, sawRestart)
		}
	}
	if !sawExit {
		t.Error("a respawn was announced without the exit that caused it")
	}
}

// A child that dies instantly and forever — a bad --resume, a missing binary —
// must stop being respawned, or daraja forks at whatever rate the kernel
// allows for as long as it lives.
func TestRespawnStopsAtTheLimit(t *testing.T) {
	h := NewHost(HostOptions{
		Binary:         testShortLivedBinary(t),
		Spec:           ChildSpec{Kind: KindClaude},
		RespawnBackoff: time.Millisecond,
		RespawnLimit:   2,
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	var restarts int
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.Events():
			if ev.Restarted != nil {
				restarts++
				if restarts > 2 {
					t.Fatalf("respawned %d times, want at most 2", restarts)
				}
			}
		case <-h.Done():
			if restarts != 2 {
				t.Errorf("host finished after %d respawns, want 2", restarts)
			}
			return
		case <-deadline:
			t.Fatalf("host never gave up; restarts=%d", restarts)
		}
	}
}
```

Add the helper if `host_test.go` has none:

```go
// testShortLivedBinary returns a binary that exits immediately and
// successfully, standing in for a claude that dies on its own.
func testShortLivedBinary(t *testing.T) string {
	t.Helper()
	return "/usr/bin/true"
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `/usr/local/go/bin/go test ./pkg/daraja/ -run TestUnexpectedExitRespawns\|TestRespawnStops -race -count=1 -v`
Expected: FAIL — `unknown field RespawnBackoff`, and once that compiles, a
timeout because nothing respawns.

- [ ] **Step 3: Implement**

Add the constants near `defaultGrace`:

```go
// defaultRespawnLimit bounds consecutive respawns after unexpected exits. A
// deterministically-failing child — a bad --resume, a binary that is gone —
// would otherwise be forked forever.
const defaultRespawnLimit = 5

// defaultRespawnBackoff paces those respawns.
const defaultRespawnBackoff = 2 * time.Second
```

Add to `HostOptions`:

```go
	// RespawnLimit and RespawnBackoff bound recovery from unexpected exits.
	// Zero means the package default.
	RespawnLimit   int
	RespawnBackoff time.Duration
```

Add `respawns int` to `Host` (guarded by `h.mu`), and rewrite the tail of
`watch`:

```go
	if !unexpected {
		return
	}
	h.emit(Event{Exited: &info})
	h.respawn()
}

// respawn brings the child back after it died on its own.
//
// It runs on the watcher's goroutine, after the Exited event, so a consumer
// sees the death before the replacement. Giving up closes done: a daraja whose
// child cannot be kept alive has nothing left to host, and the invariant is
// that a daraja always has exactly one child.
func (h *Host) respawn() {
	limit, backoff := h.opts.RespawnLimit, h.opts.RespawnBackoff
	if limit <= 0 {
		limit = defaultRespawnLimit
	}
	if backoff <= 0 {
		backoff = defaultRespawnBackoff
	}

	h.mu.Lock()
	h.respawns++
	over := h.respawns > limit
	h.mu.Unlock()
	if over {
		slog.Error("daraja: child kept exiting; giving up", "respawns", h.respawns)
		h.doneOnce.Do(func() { close(h.done) })
		return
	}

	select {
	case <-time.After(backoff):
	case <-h.done:
		return
	}

	if _, err := h.Restart(ChildSpec{}, 0); err != nil {
		slog.Error("daraja: respawn failed", "error", err)
		h.doneOnce.Do(func() { close(h.done) })
	}
}
```

Reset the counter on a caller-driven restart, at the top of `Restart` after the
lock is taken: `h.respawns = 0`. A deliberate restart is evidence the child is
wanted, and it must not inherit a crash streak.

Add `"log/slog"` to the imports.

- [ ] **Step 4: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/daraja/ -race -count=1 -v`
Expected: PASS, all of them. `-race` matters: `respawn` runs on the watcher
goroutine and touches `h.respawns`.

- [ ] **Step 5: Commit**

```bash
git add pkg/daraja
git commit -m "daraja: respawn a child that dies on its own, with a cap

Host.watch has always emitted Exited for an unexpected death and nothing acted
on it. When the controller is reachable it could send a Restart; when the
connection is down too, nothing can, and the child is simply gone.

The cap is not optional: a child that fails instantly and deterministically — a
bad --resume, a binary that moved — would otherwise be forked for as long as
daraja lives."
```

---

### Task 7: Relay must not lose an event, and must admit one stream

Two defects that are harmless while a dead stream means a dead daraja, and are
silent data loss the moment daraja reconnects (1b-ii):

1. `server.go:99-107` takes an event off `Events()` and then sends it. If the
   send fails, that event is gone — the exact drain-and-discard hole the design's
   "Nothing is lost across a disconnect" section forbids.
2. Nothing stops two concurrent `Relay` streams. Two consumers of one
   `Events()` channel split the byte stream between them at random.

**Files:**
- Modify: `pkg/daraja/server.go`
- Test: `pkg/daraja/server_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: no exported change. `Server` gains unexported `pending` and
  `attached` state.

- [ ] **Step 1: Write the failing tests**

Add to `pkg/daraja/server_test.go`:

```go
// An event pulled off the host's channel and not delivered must be redelivered
// on the next stream. Without this, a reconnecting controller silently loses
// whatever was in flight when its connection broke — and nothing errors.
func TestUndeliveredEventSurvivesAFailedSend(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/cat", Spec: ChildSpec{Kind: KindClaude}})
	s := NewServer(h)

	s.stash(&darajapb.RelayResponse{
		Event: &darajapb.RelayResponse_Stdout{Stdout: []byte("in flight")},
	})

	got := s.takePending()
	if got == nil {
		t.Fatal("stashed event was not returned")
	}
	if string(got.GetStdout()) != "in flight" {
		t.Errorf("got %q, want %q", got.GetStdout(), "in flight")
	}
	if s.takePending() != nil {
		t.Error("pending event was returned twice")
	}
}

// Two Relay streams would split one event channel between them, giving each
// consumer a random half of the child's output.
func TestSecondRelayIsRefused(t *testing.T) {
	h := NewHost(HostOptions{Binary: "/bin/cat", Spec: ChildSpec{Kind: KindClaude}})
	s := NewServer(h)

	if !s.attach() {
		t.Fatal("first attach was refused")
	}
	if s.attach() {
		t.Error("second attach was admitted; one Relay at a time is the contract")
	}
	s.detach()
	if !s.attach() {
		t.Error("attach was refused after detach")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `/usr/local/go/bin/go test ./pkg/daraja/ -run TestUndelivered\|TestSecondRelay -v`
Expected: FAIL to compile — `s.stash`, `s.takePending`, `s.attach`, `s.detach`
undefined.

- [ ] **Step 3: Implement**

Add to `Server`:

```go
	mu       sync.Mutex
	pending  *darajapb.RelayResponse
	attached bool
```

and the four helpers:

```go
// attach claims the single Relay slot. Two streams consuming one event channel
// would split the child's output between them at random.
func (s *Server) attach() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached {
		return false
	}
	s.attached = true
	return true
}

func (s *Server) detach() {
	s.mu.Lock()
	s.attached = false
	s.mu.Unlock()
}

// stash keeps an event that was taken off the host's channel and not
// delivered, so the next stream sends it first.
//
// This is what keeps the backpressure guarantee honest across a reconnect. The
// host blocks rather than dropping output (see Host.emit), but that protects
// only events still IN the channel; one already dequeued is the server's
// responsibility and was previously discarded on a failed send.
func (s *Server) stash(resp *darajapb.RelayResponse) {
	s.mu.Lock()
	s.pending = resp
	s.mu.Unlock()
}

func (s *Server) takePending() *darajapb.RelayResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = nil
	return p
}
```

Rewrite `Relay`'s body:

```go
func (s *Server) Relay(
	ctx context.Context,
	stream *connect.BidiStream[darajapb.RelayRequest, darajapb.RelayResponse],
) error {
	if !s.attach() {
		return connect.NewError(connect.CodeAlreadyExists,
			errors.New("daraja: a relay stream is already attached"))
	}
	defer s.detach()

	go func() {
		for {
			req, err := stream.Receive()
			if err != nil {
				return
			}
			if b := req.GetStdin(); len(b) > 0 {
				if werr := s.host.WriteStdin(b); werr != nil {
					return
				}
			}
		}
	}()

	if p := s.takePending(); p != nil {
		if err := stream.Send(p); err != nil {
			s.stash(p)
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.host.Done():
			return nil
		case ev := <-s.host.Events():
			resp, err := relayResponse(ev)
			if err != nil {
				return err
			}
			if err := stream.Send(resp); err != nil {
				// Taken off the channel and not delivered: hold it for the
				// next stream rather than dropping it on the floor.
				s.stash(resp)
				return err
			}
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/daraja/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/daraja/server.go pkg/daraja/server_test.go
git commit -m "daraja: never drop a dequeued event, and admit one relay at a time

Relay took an event off the host's channel and then sent it; a failed send
discarded it. Host.emit's backpressure protects only events still IN the
channel, so this was a hole the moment a controller reconnects rather than dies.

Nothing stopped two concurrent Relay streams either, which would split one
child's output between two consumers at random."
```

---

### Task 8: `AdminService` proto

**Files:**
- Create: `proto/rafiki/admin/v1/admin.proto`
- Modify: `Makefile` (add the admin generation block)
- Create: `pkg/adminpb/` (generated, committed)

**Interfaces:**
- Consumes: `rafiki.daraja.v1.ChildSpec` (Task 3), reachable now that Task 1
  unified the proto paths.
- Produces: `adminpbconnect.NewAdminServiceHandler`, and the request/response
  messages Task 10 and Task 11 implement.

- [ ] **Step 1: Write the proto**

Create `proto/rafiki/admin/v1/admin.proto`:

```protobuf
syntax = "proto3";

package rafiki.admin.v1;

import "rafiki/daraja/v1/daraja.proto";

option go_package = "go.graveland.dev/rafiki/pkg/adminpb;adminpb";

// AdminService is the machine-admin surface on a rafiki executor host.
//
// It is deliberately NOT part of ExecutorService. The executor is the executor:
// a data path for one child's filesystem and shell tools, with its own
// lifecycle hazards. Launching a daraja is a different concern with a different
// lifetime — a daraja outlives both the turn and the connection that asked for
// it — and it merely shares the executor's process and its reverse-dialled
// connection. Future admin verbs belong here, not there.
service AdminService {
  // Launch starts one daraja hosting one child and returns the handles needed
  // to reach and to reap it.
  rpc Launch(LaunchRequest) returns (LaunchResponse);

  // Reap ends a launched daraja and its child: SIGTERM to the process group,
  // wait, then SIGKILL.
  rpc Reap(ReapRequest) returns (ReapResponse);
}

message LaunchRequest {
  // child_id is the daemon-stamped id this daraja hosts. It scopes the socket
  // path and is the key the executor tracks the launch under.
  string child_id = 1;
  // cwd is the working directory for the hosted child.
  string cwd = 2;
  // spec is typed rather than argv: the executor and daraja both build the
  // child's command line through pkg/claudeargv, so neither side invents one.
  rafiki.daraja.v1.ChildSpec spec = 3;
}

message LaunchResponse {
  // pid of the daraja process.
  int32 pid = 1;
  // pgid is the process GROUP that daraja leads and its child joins. This is
  // the reaping handle, and unlike a pid it is stable for the child's whole
  // life: restarts stay in the group, and the group outlives daraja itself, so
  // it still reaches a claude orphaned by a SIGKILLed daraja.
  int32 pgid = 2;
  // socket is the unix socket daraja serves DarajaService on. Phase 1b-ii
  // replaces this with a reverse dial; until then the caller dials it directly.
  string socket = 3;
}

message ReapRequest {
  // child_id names a launch this executor is still tracking. It is deliberately
  // NOT a bare pgid: a process group id is recycled once its group empties, so
  // signalling a number supplied by a peer could reach an unrelated group.
  string child_id = 1;
  // grace_ms between SIGTERM and SIGKILL. Zero means the server default.
  int32 grace_ms = 2;
}

message ReapResponse {
  // reaped is false when child_id named no tracked launch, which is the normal
  // answer for something already gone. Reaping is idempotent.
  bool reaped = 1;
}
```

- [ ] **Step 2: Add the Makefile block**

Insert after the daraja block (`gofmt -w pkg/darajapb`):

```makefile
	mkdir -p pkg/adminpb
	$(PROTOC) \
		--plugin=protoc-gen-go=bin/protoc-gen-go \
		--plugin=protoc-gen-connect-go=bin/protoc-gen-connect-go \
		--proto_path=proto \
		--go_out=pkg/adminpb --go_opt=module=go.graveland.dev/rafiki/pkg/adminpb \
		--connect-go_out=pkg/adminpb --connect-go_opt=module=go.graveland.dev/rafiki/pkg/adminpb \
		proto/rafiki/admin/v1/admin.proto
	gofmt -w pkg/adminpb
```

Also update the target's help text on line 187 to mention adminpb.

- [ ] **Step 3: Generate**

Run:
```bash
make proto
ls pkg/adminpb pkg/adminpb/adminpbconnect
```

Expected: `admin.pb.go` and `adminpbconnect/admin.connect.go`.

- [ ] **Step 4: Prove the cross-file import resolves at init**

This is what Task 1 existed for, and a compile success does not prove it — the
failure mode is a panic in `init`.

Run:
```bash
/usr/local/go/bin/go test ./pkg/adminpb/... -run XXX -count=1 -v
```

Expected: `ok ... [no tests to run]` — not a panic. If it panics with a
descriptor-resolution message, Task 1's regeneration did not take; rerun
`make proto` and confirm `grep -c 'rafiki/daraja/v1/daraja.proto' pkg/darajapb/daraja.pb.go`
is non-zero.

- [ ] **Step 5: Commit**

```bash
git add proto/rafiki/admin/v1 pkg/adminpb Makefile
git commit -m "proto: add AdminService, the machine-admin surface

Launching a daraja is not an executor concern. The executor is a data path for
one child's tools; a daraja outlives both the turn and the connection that asked
for it. AdminService shares the executor's process and connection and nothing
else, and future admin verbs belong here rather than widening ExecutorService."
```

---

### Task 9: The executor declares what it can launch

Follows `--proxy` exactly: an operator flag at executor startup, advertised in
`Describe`, consumed by the daemon as capability ∩ selector. Safe to self-report
for the same reason the `proxies` field already documents — it only ever
NARROWS, so a wrong entry costs a failed launch rather than opening a machine.

**Files:**
- Modify: `proto/rafiki/executor/v1/executor.proto:71` (after `proxies`)
- Modify: `pkg/executor/server.go:183-204` (`Options`, `Describe`)
- Modify: `cmd/rafiki/cmd_executor_serve.go`
- Test: `pkg/executor/admin_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `executor.Options.LaunchKinds []string`,
  `(*Server).LaunchKinds() []string`, `DescribeResponse.launch_kinds`. Consumed
  by 1b-ii's selection code, and by Task 10's guard.

- [ ] **Step 1: Add the proto field**

In `proto/rafiki/executor/v1/executor.proto`, inside `DescribeResponse` after
`proxies`:

```protobuf
  // launch_kinds are the child protocols this executor will host via
  // AdminService.Launch, declared by its operator with --launch at startup.
  // Safe to self-report for the same reason as proxies: it only ever NARROWS
  // what this executor will do. The daemon still requires the child's ordinary
  // executor selector to match, so a wrong entry costs a failed launch rather
  // than admitting anyone.
  repeated string launch_kinds = 11;
```

Run `make proto`.

- [ ] **Step 2: Write the failing test**

Create `pkg/executor/admin_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

func TestDescribeAdvertisesLaunchKinds(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), LaunchKinds: []string{"claude"}})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !slices.Contains(resp.Msg.GetLaunchKinds(), "claude") {
		t.Errorf("launch_kinds = %v, want claude", resp.Msg.GetLaunchKinds())
	}
}

// An executor with no --launch flag hosts nothing. The default must be empty
// rather than "claude": a machine volunteering to host other people's children
// because someone forgot a flag is the self-report-gates-placement shape the
// isolation and workspace_mode rules exist to forbid.
func TestDescribeAdvertisesNoLaunchKindsByDefault(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir()})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := resp.Msg.GetLaunchKinds(); len(got) != 0 {
		t.Errorf("launch_kinds = %v, want empty", got)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `/usr/local/go/bin/go test ./pkg/executor/ -run TestDescribeAdvertises -v`
Expected: FAIL — `unknown field LaunchKinds in Options`.

- [ ] **Step 4: Implement**

Add to `executor.Options`:

```go
	// LaunchKinds are the child protocols this executor will host, from
	// --launch. Empty means none, deliberately: hosting is opt-in.
	LaunchKinds []string
```

Add the accessor beside `ProxyNames`:

```go
// LaunchKinds returns the declared launchable kinds, for DescribeResponse.
func (s *Server) LaunchKinds() []string {
	return append([]string(nil), s.opts.LaunchKinds...)
}
```

Add `LaunchKinds: s.LaunchKinds(),` to the `DescribeResponse` literal in
`Describe`.

- [ ] **Step 5: Add the CLI flag**

In `cmd/rafiki/cmd_executor_serve.go`, add `launchKinds []string` to the var
block, pass `LaunchKinds: launchKinds` into `executor.NewServer`, and register:

```go
	cmd.Flags().StringArrayVar(&launchKinds, "launch", nil,
		"child protocol this executor will host for the daemon, e.g. --launch claude "+
			"(repeatable). Opt-in: with no --launch this executor hosts nothing")
```

- [ ] **Step 6: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/executor/ -race -count=1 -run TestDescribe -v`
Expected: PASS, 2 tests.

- [ ] **Step 7: Update the docs**

`docs/reference/executor-protocol.md` documents `Describe`. Add `launch_kinds`
beside `proxies` with the narrowing-only rationale, and document `--launch` in
`README.md` where `--proxy` is described. Keeping docs in sync in the same
change is a project rule, not a nicety.

- [ ] **Step 8: Commit**

```bash
git add proto/rafiki/executor/v1 pkg/executorpb pkg/executor cmd/rafiki/cmd_executor_serve.go docs README.md
git commit -m "executor: declare launchable child kinds with --launch

The --proxy pattern verbatim: an operator flag at startup, advertised in
Describe, consumed by the daemon as capability intersected with the child's
ordinary selector. Safe to self-report for the same reason proxies is — it only
ever narrows, so a wrong entry costs a failed launch rather than opening a
machine. Opt-in, so a forgotten flag hosts nothing rather than everything."
```

---

### Task 10: `AdminService.Launch` and daraja supervision

**Files:**
- Create: `pkg/executor/admin.go`
- Modify: `pkg/executor/admin_test.go`
- Modify: `cmd/rafiki/cmd_executor_serve.go:58-63` (`executorHandler`)

**Interfaces:**
- Consumes: `adminpb` (Task 8), `claudeargv` (Task 2), `Options.LaunchKinds`
  (Task 9), the `daraja serve` CLI contract (Task 5).
- Produces:
  ```go
  func NewAdminServer(o AdminOptions) *AdminServer
  type AdminOptions struct {
      SelfBinary  string   // path to this `rafiki` binary
      ChildBinary string   // path to `claude`
      LaunchKinds []string
      SocketDir   string
  }
  func (a *AdminServer) Routes() (string, http.Handler)
  ```
  Consumed by Task 11 (`Reap`) and Task 12.

- [ ] **Step 1: Write the failing test**

Add to `pkg/executor/admin_test.go`:

```go
// The launched daraja must lead its own process group, because that group is
// the reaping handle for the whole child — daraja plus the claude that joins
// it. An executor that forgets Setpgid leaves daraja in the EXECUTOR's group,
// where a reap would signal the executor itself.
func TestLaunchGivesDarajaItsOwnGroup(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pid, pgid := int(resp.Msg.GetPid()), int(resp.Msg.GetPgid())
	if pgid != pid {
		t.Errorf("pgid = %d, want it to equal daraja's own pid %d", pgid, pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatal("daraja was left in the executor's process group; a reap would signal us")
	}
	if resp.Msg.GetSocket() == "" {
		t.Error("Launch returned no socket path")
	}
}

// An undeclared kind must be refused. The flag is the operator's declaration
// and the RPC is a peer's request; the declaration wins.
func TestLaunchRefusesAnUndeclaredKind(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: nil,
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err == nil {
		t.Fatal("Launch admitted a kind this executor never declared")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// buildSelfStub compiles a stand-in for the `rafiki` binary that sleeps until
// signalled, so a launch produces a real long-lived process to inspect without
// needing a working daraja or claude.
func buildSelfStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main
import ("os";"os/signal";"syscall")
func main() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "stub")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	return bin
}
```

Add the imports the test needs: `os`, `os/exec`, `path/filepath`, `syscall`,
`go.graveland.dev/rafiki/pkg/adminpb`, `go.graveland.dev/rafiki/pkg/darajapb`.

- [ ] **Step 2: Run it to verify it fails**

Run: `/usr/local/go/bin/go test ./pkg/executor/ -run TestLaunch -v`
Expected: FAIL to compile — `undefined: NewAdminServer`.

- [ ] **Step 3: Implement**

Create `pkg/executor/admin.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/adminpb/adminpbconnect"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

// defaultReapGrace is the window between SIGTERM and SIGKILL, matching
// daraja's own stopLocked rather than inventing a second policy.
const defaultReapGrace = 3 * time.Second

// AdminOptions configures the machine-admin surface.
type AdminOptions struct {
	// SelfBinary is the path to this `rafiki` binary, which is re-executed as
	// `rafiki daraja serve`. daraja is a subcommand rather than a third
	// artifact, so the executor already has it.
	SelfBinary string
	// ChildBinary is the hosted child's binary, e.g. claude.
	ChildBinary string
	// LaunchKinds is the operator's declaration, from --launch.
	LaunchKinds []string
	// SocketDir is where daraja's unix sockets are created.
	SocketDir string
}

// launched is one daraja this executor started and is responsible for.
type launched struct {
	cmd    *exec.Cmd
	pgid   int
	socket string
}

// AdminServer launches, supervises and reaps darajas.
//
// It tracks what it launched because a process group id is RECYCLED once its
// group empties: signalling a number a peer handed us could reach an unrelated
// group. Reap therefore takes a child_id and resolves the pgid here.
type AdminServer struct {
	opts AdminOptions

	mu sync.Mutex
	m  map[string]*launched
}

func NewAdminServer(o AdminOptions) *AdminServer {
	return &AdminServer{opts: o, m: map[string]*launched{}}
}

func (a *AdminServer) Routes() (string, http.Handler) {
	return adminpbconnect.NewAdminServiceHandler(a)
}

// Close reaps everything still running. The executor's own shutdown must not
// strand a daraja: nothing else on this machine knows the pgid.
func (a *AdminServer) Close() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.m))
	for id := range a.m {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.reap(id, defaultReapGrace)
	}
}

func (a *AdminServer) Launch(
	ctx context.Context, req *connect.Request[adminpb.LaunchRequest],
) (*connect.Response[adminpb.LaunchResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("child_id is required"))
	}
	kind, err := a.kindFor(req.Msg.GetSpec())
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	_, dup := a.m[childID]
	a.mu.Unlock()
	if dup {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("child %s is already hosted here", childID))
	}

	socket := filepath.Join(a.opts.SocketDir, "daraja-"+childID+".sock")
	_ = os.Remove(socket)

	c := req.Msg.GetSpec().GetClaude()
	argv := []string{
		"daraja", "serve",
		"--socket", socket,
		"--binary", a.opts.ChildBinary,
		"--cwd", req.Msg.GetCwd(),
		"--kind", kind,
	}
	if c.GetModel() != "" {
		argv = append(argv, "--model", c.GetModel())
	}
	if c.GetResumeSession() != "" {
		argv = append(argv, "--resume", c.GetResumeSession())
	}
	if c.GetPermissionMode() != "" {
		argv = append(argv, "--permission-mode", c.GetPermissionMode())
	}

	cmd := exec.Command(a.opts.SelfBinary, argv...)
	// daraja LEADS a new group and its claude joins it, so this pgid is the one
	// handle that reaches the whole child — and keeps reaching claude after a
	// SIGKILLed daraja orphans it to launchd. Without Setpgid, daraja would sit
	// in the EXECUTOR's group and a reap would signal the executor itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start daraja: %w", err))
	}
	pid := cmd.Process.Pid

	l := &launched{cmd: cmd, pgid: pid, socket: socket}
	a.mu.Lock()
	a.m[childID] = l
	a.mu.Unlock()

	go a.supervise(childID, l)

	slog.Info("admin: launched daraja", "childID", childID, "pid", pid, "pgid", pid, "socket", socket)
	return connect.NewResponse(&adminpb.LaunchResponse{
		Pid:    int32(pid),
		Pgid:   int32(pid),
		Socket: socket,
	}), nil
}

// supervise waits on daraja, which is this process's DIRECT child, so it never
// zombies and its exit is logged.
//
// It cannot wait on claude: darwin has no PR_SET_CHILD_SUBREAPER, so a claude
// orphaned by a SIGKILLed daraja reparents to launchd rather than here. The
// process group is what covers that case, not this goroutine.
func (a *AdminServer) supervise(childID string, l *launched) {
	err := l.cmd.Wait()
	code := l.cmd.ProcessState.ExitCode()
	slog.Info("admin: daraja exited", "childID", childID, "pid", l.pgid, "exitCode", code, "error", err)

	// Dropping the entry is what stops a recycled pgid from being signalled
	// later: once the group is likely empty, this executor no longer claims it.
	a.mu.Lock()
	if a.m[childID] == l {
		delete(a.m, childID)
	}
	a.mu.Unlock()
	_ = os.Remove(l.socket)
}

// kindFor validates the requested kind against the operator's declaration.
func (a *AdminServer) kindFor(spec *darajapb.ChildSpec) (string, error) {
	if spec.GetKind() != darajapb.Kind_KIND_CLAUDE {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported child kind %v", spec.GetKind()))
	}
	const kind = "claude"
	if !slices.Contains(a.opts.LaunchKinds, kind) {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("this executor does not host %q; start it with --launch %s", kind, kind))
	}
	return kind, nil
}
```

`Reap` arrives in Task 11; until then add a stub that returns
`connect.CodeUnimplemented` so the handler compiles.

- [ ] **Step 4: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/executor/ -race -count=1 -run TestLaunch -v`
Expected: PASS, 2 tests.

- [ ] **Step 5: Mount it on the executor's mux**

In `cmd/rafiki/cmd_executor_serve.go`, change `executorHandler` and its caller:

```go
func executorHandler(srv *executor.Server, admin *executor.AdminServer) http.Handler {
	mux := http.NewServeMux()
	interceptor := executor.NewRPCInterceptor()
	mux.Handle(executorpbconnect.NewExecutorServiceHandler(srv, connect.WithInterceptors(interceptor)))
	// A second service on the SAME mux and the same reverse-dialled connection.
	// The executor stays the executor; this is the machine-admin surface.
	mux.Handle(admin.Routes())
	return mux
}
```

In `RunE`, build the admin server after `srv` and wire its shutdown:

```go
			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve own binary: %w", err)
			}
			childBin, err := exec.LookPath("claude")
			if err != nil && len(launchKinds) > 0 {
				return fmt.Errorf("--launch claude given but claude is not on PATH: %w", err)
			}
			admin := executor.NewAdminServer(executor.AdminOptions{
				SelfBinary:  self,
				ChildBinary: childBin,
				LaunchKinds: launchKinds,
				SocketDir:   os.TempDir(),
			})
			defer admin.Close()
			handler := executorHandler(srv, admin)
```

Add `os/exec` to the imports.

- [ ] **Step 6: Run the gate**

Run: `make check > /tmp/check.log 2>&1; echo $?`
Expected: `0`.

- [ ] **Step 7: Commit**

```bash
git add pkg/executor/admin.go pkg/executor/admin_test.go cmd/rafiki/cmd_executor_serve.go
git commit -m "executor: AdminService.Launch, on the same mux as the executor

Launch spawns \`rafiki daraja serve\` as a process group LEADER; daraja's claude
joins that group, so one pgid reaches the whole child for its whole life and
still reaches claude after a SIGKILLed daraja orphans it to launchd.

The executor waits on daraja, its direct child, so it never zombies and its exit
is logged. It cannot wait on claude — darwin has no PR_SET_CHILD_SUBREAPER — and
the group is what covers that, not the waiter."
```

---

### Task 11: `AdminService.Reap`

**Files:**
- Modify: `pkg/executor/admin.go`
- Modify: `pkg/executor/admin_test.go`

**Interfaces:**
- Consumes: `AdminServer` (Task 10).
- Produces: `func (a *AdminServer) reap(childID string, grace time.Duration) bool`,
  used by `Reap` and by `Close`.

- [ ] **Step 1: Write the failing tests**

```go
// Reap must end daraja AND the child that joined its group. The stub sleeps
// until signalled, so a surviving process is an observable failure.
func TestReapEndsTheWholeGroup(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pgid := int(resp.Msg.GetPgid())

	rr, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{
		ChildId: "c1", GraceMs: 500,
	}))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !rr.Msg.GetReaped() {
		t.Error("Reap reported nothing reaped for a live launch")
	}

	// The group must be gone. ESRCH from a zero-signal probe is the proof.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d still alive after Reap", pgid)
}

// Reaping something already gone is the normal case — the daemon reaps on kill
// without knowing whether the machine already cleaned up — and must not error.
func TestReapIsIdempotent(t *testing.T) {
	a := NewAdminServer(AdminOptions{SocketDir: t.TempDir()})
	defer a.Close()

	resp, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{ChildId: "ghost"}))
	if err != nil {
		t.Fatalf("Reap of an unknown child errored: %v", err)
	}
	if resp.Msg.GetReaped() {
		t.Error("Reap claimed to reap a child it never launched")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `/usr/local/go/bin/go test ./pkg/executor/ -run TestReap -v`
Expected: FAIL — the stub returns `CodeUnimplemented`.

- [ ] **Step 3: Implement**

Replace the stub:

```go
func (a *AdminServer) Reap(
	ctx context.Context, req *connect.Request[adminpb.ReapRequest],
) (*connect.Response[adminpb.ReapResponse], error) {
	grace := time.Duration(req.Msg.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = defaultReapGrace
	}
	return connect.NewResponse(&adminpb.ReapResponse{
		Reaped: a.reap(req.Msg.GetChildId(), grace),
	}), nil
}

// reap signals the child's process group: SIGTERM, wait out grace, then
// SIGKILL. It mirrors daraja's own stopLocked rather than inventing a second
// escalation policy.
//
// It resolves the pgid from this executor's own launch table and never from the
// request, because a pgid is recycled once its group empties — signalling a
// number a peer supplied could reach an unrelated group. An unknown child_id is
// not an error: reaping something already gone is the normal case.
func (a *AdminServer) reap(childID string, grace time.Duration) bool {
	a.mu.Lock()
	l := a.m[childID]
	a.mu.Unlock()
	if l == nil {
		return false
	}

	if err := syscall.Kill(-l.pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("admin: SIGTERM to child group failed", "childID", childID, "pgid", l.pgid, "error", err)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-l.pgid, 0); errors.Is(err, syscall.ESRCH) {
			slog.Info("admin: child group ended on SIGTERM", "childID", childID, "pgid", l.pgid)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	slog.Warn("admin: child group outlived its grace; escalating", "childID", childID, "pgid", l.pgid)
	if err := syscall.Kill(-l.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Error("admin: SIGKILL to child group failed", "childID", childID, "pgid", l.pgid, "error", err)
	}
	return true
}
```

- [ ] **Step 4: Run the tests**

Run: `/usr/local/go/bin/go test ./pkg/executor/ -race -count=1 -run TestReap\|TestLaunch -v`
Expected: PASS, 4 tests.

- [ ] **Step 5: Run the gate**

Run: `make check > /tmp/check.log 2>&1; echo $?`
Expected: `0`.

- [ ] **Step 6: Commit**

```bash
git add pkg/executor
git commit -m "executor: AdminService.Reap ends the child's process group

SIGTERM, wait out grace, SIGKILL — mirroring daraja's own stopLocked rather than
a second escalation policy.

The pgid is resolved from this executor's launch table and never from the
request: a process group id is recycled once its group empties, so signalling a
number a peer supplied could reach an unrelated group. An unknown child is not
an error, because reaping something already gone is the normal case."
```

---

### Task 12: Prove it against a real claude

1a was signed off against the real binary, and the interesting failures here are
not unit-testable: whether `claudeargv`'s flags are the ones claude 2.1.252
actually accepts, and whether the group really covers both processes.

**Files:**
- Modify: `docs/plans/2026-09-03-daraja-phase1b-plan.md` (append the results)

- [ ] **Step 1: Build and start an executor that will launch**

```bash
make build
./bin/rafiki executor serve \
  --connect-socket /tmp/nonexistent.sock \
  --launch claude \
  --root "$PWD" 2>&1 | tee /tmp/exec-1b.log &
```

The connect target does not need to exist — `AdminService` is exercised directly
in the next step. If serve refuses to start without a reachable daemon, run the
executor's `Server` and `AdminServer` from a small `go run` harness on a local
socket instead, and say so in the results.

- [ ] **Step 2: Launch a daraja through AdminService**

Write a throwaway client under `/private/tmp/claude-501/.../scratchpad` (module
with a `replace` to this repo) that dials the executor's handler and calls
`Launch` with `ChildId: "p1bi-01"`, `Cwd: $PWD`, and
`Spec{Kind: KIND_CLAUDE, Claude: {PermissionMode: "bypassPermissions"}}`.

Record: the returned `pid`, `pgid` and `socket`.

- [ ] **Step 3: Confirm the group covers BOTH processes**

```bash
ps -o pid,pgid,command -p <daraja pid>
pgrep -f 'claude' | while read p; do ps -o pid,pgid,command -p "$p"; done
```

Expected: daraja's `PGID` equals its own `PID`, and claude's `PGID` equals the
**same** value. This is the single assertion the whole reaping design rests on;
if claude shows its own pgid, Task 4's `InheritProcessGroup` is not reaching
`newProcessRunner`.

- [ ] **Step 4: Drive one turn over the returned socket**

Extend the throwaway client to dial `socket` with
`http2.Transport{AllowHTTP: true}` and a `DialTLSContext` that dials `unix`
(1a's verification section has the shape), open `Relay`, and send:

```json
{"type":"user","message":{"role":"user","content":"Reply with exactly the word: ok"}}
```

Expected: the `system/init` frame carrying a `session_id`, an assistant frame
with `"text":"ok"`, and a `result` frame. Record the session id.

- [ ] **Step 5: Confirm the argv builder produced a working command line**

If step 4 returned nothing parseable, the base flags are wrong for this claude
build. Check the real flag set with `claude --help` and correct
`pkg/claudeargv.Build`, then re-run steps 2-4. Record any correction — the
plan's `--permission-mode` / `--dangerously-skip-permissions` mapping is the
most likely thing to be wrong.

- [ ] **Step 6: Reap and confirm nothing survives**

Call `Reap{ChildId: "p1bi-01", GraceMs: 3000}`, then:

```bash
pgrep -fl 'daraja serve' || echo no daraja
pgrep -fl 'claude -p'    || echo no claude orphans
ls /tmp/daraja-p1bi-01.sock 2>&1
```

Expected: both `pgrep`s report nothing, and the socket is gone.

- [ ] **Step 7: Confirm the orphan case**

Relaunch, then `kill -9` **daraja only**, and confirm claude survives and is
still reachable by the group:

```bash
kill -9 <daraja pid>
sleep 1
pgrep -fl 'claude -p'            # expect: still running
kill -TERM -<pgid>; sleep 1
pgrep -fl 'claude -p' || echo reaped via the group
```

This is the case that motivated the whole design — a pid-based reap cannot do
it. If claude survives the group signal, the group is not what we think it is.

- [ ] **Step 8: Record the results**

Append a `## Phase 1b-i verification` section to this plan with the literal
command output, the claude version tested, and every deviation from the plan as
written — following the format 1a's plan used, which is what made its
deviations (`exit_code=143`, `darajapbconnect`, the connect-go `Receive`
signature) useful later.

- [ ] **Step 9: Final gate and commit**

```bash
make check > /tmp/check.log 2>&1; echo $?
git add docs/plans/2026-09-03-daraja-phase1b-plan.md
git commit -m "docs: record Phase 1b-i verification against claude"
```

---

## Self-Review

**Spec coverage.** The amended design's 1b sections map as follows.
"AdminService and the launch RPC" → Tasks 8, 10. "Launchability" → Task 9.
"Nothing is lost across a disconnect" → Task 7 (the negative requirement: no
drain-and-discard). "The child's process group" → Tasks 4, 5, 10, 11, and the
step-7 orphan check in Task 12. The `ChildSpec` amendment under `Restart` →
Tasks 2, 3. The lifecycle table's "claude dies, daraja survives" row → Task 6.

**Deliberately deferred to 1b-ii**, and therefore absent here: `upgradeconn.Daraja`,
daraja's reverse dial, the ticket and reconnect credential, `pkg/darajapool`,
rafikid mounting, `rafiki daraja launch`, and the `rafiki/executor-state`
unreachable label. `Launch` returns a unix socket path in this plan; 1b-ii
replaces that field's meaning without changing either service, exactly as 1a's
socket was replaced.

**Placeholder scan.** No TBDs. Two steps carry a conditional with its resolution
named rather than a guess: Task 12 Step 1 (if `executor serve` refuses to start
without a reachable daemon, run the servers from a harness) and Task 12 Step 5
(if the flags are wrong, `claude --help` and record the correction). Task 3 Step
6 says to reuse whatever long-lived-child helper `host_test.go` already has, and
names `/bin/cat` as the fallback, rather than assuming a helper name I have not
read.

**Type consistency.** `claudeargv.Params{Model, ResumeSession, PermissionMode}`
(Task 2) is consumed by `daraja.ChildSpec.argv` (Task 3) with the same field
names. `daraja.ChildSpec` (Go, Task 3) and `rafiki.daraja.v1.ChildSpec` (proto,
Task 3) are distinct types bridged by `specFromProto`, and the proto one is what
Task 8's `admin.proto` imports. `Host.Restart(ChildSpec, time.Duration)` is used
consistently in Tasks 3, 6 and 7. `AdminOptions{SelfBinary, ChildBinary,
LaunchKinds, SocketDir}` and `NewAdminServer` match between Tasks 10, 11 and the
tests in both. `executor.Options.LaunchKinds` (Task 9) is the source for
`AdminOptions.LaunchKinds` (Task 10 Step 5).

**One risk worth flagging to the executor of this plan.** Task 5 removes
`daraja serve`'s `-- child-args` passthrough, which 1a's verification recipe
used. Anyone re-running that recipe verbatim will find it broken; the
replacement is `--kind claude --permission-mode bypassPermissions`. This is
intentional (it is what makes one argv builder possible) but it invalidates a
documented procedure, so Task 12 Step 8 should note it.

---

## Phase 1b-i verification

Verified against `claude` **2.1.260** (the plan cites 2.1.252; the pre-flight
ledger said 2.1.259 — the machine moved again before this run) on
`daraja-phase1b-i` @ `e4fb74d`, executor binary `bin/rafiki` reporting version
`pre-rafiki-tui-249-ge4fb74d`.

### Harness (Task 12 Step 1, with the brief's conditional exercised)

`rafiki executor serve --connect-socket /tmp/nonexistent-daraja-p1bi.sock
--launch claude --root "$PWD"` **starts but serves nothing locally** — it
enters its reconnect loop and the handler exists only on a reverse dial:

```
WARN executor: connection lost; reconnecting error="dial /tmp/nonexistent-daraja-p1bi.sock: dial unix /tmp/nonexistent-daraja-p1bi.sock: connect: no such file or directory" in=1s
WARN executor: connection lost; reconnecting error="..." in=2s
WARN executor: connection lost; reconnecting error="..." in=4s
WARN executor: connection lost; reconnecting error="..." in=8s
```

The brief's Step-1 conditional ("if serve refuses to start without a reachable
daemon, run the servers from a harness") therefore applies in substance. The
fallback chosen is **stronger than the named one**: instead of a go-run harness
mounting `Server`+`AdminServer` on a local socket, the throwaway client plays
the DAEMON's half of the executor link — it listens on the executor's
`--connect-socket` path, completes the `/executor/connect` HTTP/1.1 Upgrade +
`executor_hello` handshake exactly as `pkg/execpool`'s pool side does
(`upgradeconn.Handler`, hello frame read byte-at-a-time,
`execpool.ClientForConn`), and calls `AdminService` over the executor's own
inverted h2 connection. The REAL `rafiki executor serve` binary does the
Launch. Two recipes the next person needs:

- **`--credential` is mandatory**: `buildHello` refuses an unauthenticated
  hello, so serve exits its handshake immediately without one. The run used
  `--credential stub-cred` (a stateless credential the stub accepts; nothing is
  written to disk because the stub returns no new credential).
- **A test stub must RPC immediately after hello.** The h2 SERVER side has a
  hard-coded `prefaceTimeout = 10 * time.Second` (x/net `http2`); the
  executor's `ServeInverted` kills the connection when no client preface ever
  arrives, which surfaces on the executor as `connection lost; reconnecting
  error=<nil>` every ~10s. The real daemon RPCs immediately so it never sees
  this; the stub fires a harmless `Launch{}` (refused `invalid_argument:
  child_id is required`) purely to write the preface. Recorded because any
  future 1b-ii test double will hit the same 10s cycle.

### Step 2 — Launch through AdminService

`Launch{child_id: "p1bi-01", cwd: <worktree>, spec: {KIND_CLAUDE, claude:
{permission_mode: "bypassPermissions"}}}` returned:

```json
{ "pid": 71651, "pgid": 71651, "socket": "/tmp/daraja-p1bi-01.sock" }
```

`pgid == pid` — daraja leads its own group (`Setpgid` in `AdminServer.Launch`).

### Step 3 — the group covers BOTH processes

```
  PID  PPID  PGID COMMAND
71651 71593 71651 bin/rafiki daraja serve --socket /tmp/daraja-p1bi-01.sock --binary /Users/brent/.local/bin/claude --cwd <worktree> --kind claude --permission-mode bypassPermissions
71653 71651 71651 /Users/brent/.local/bin/claude -p --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions
```

daraja's `PGID` equals its own `PID`, and claude's `PGID` equals the same
value. This is the assertion the whole reaping design rests on, and it held on
every one of the four launches this session. The daraja argv also shows the
`-- child-args` invalidation flagged in Self-Review is real and the typed-flag
replacement works: `--kind claude --permission-mode bypassPermissions` is on
the command line, and claude's own argv shows the `bypassPermissions` →
`--dangerously-skip-permissions` mapping landing.

### Step 4/5 — one turn over the returned socket (auth-blocked; argv CONFIRMED)

The Relay round trip works end to end — the user frame in, and claude's
stream-json back out through daraja verbatim:

```
{"type":"system","subtype":"init",...,"cwd":"<worktree>","session_id":"865d419d-e4ee-4f80-88c3-92d0543ca3b0",...}
{"type":"assistant",...}   (synthetic error message, see below)
{"type":"result",...,"is_error":true,...}
```

**The model's answer could not be verified: the machine has no working claude
credential right now.** Two failures, both independent of daraja:

1. With the machine's own OAuth store (no env credential), the result frame was
   `"Failed to authenticate: OAuth session expired and could not be refreshed"`
   (`session_id 1e395138-…`, `model: <synthetic>`). This was reproduced VERBATIM
   by driving `claude -p --input-format stream-json …` directly in a plain
   shell, outside daraja — so it is not a daraja or argv problem.
2. The plan-pre-flight assumption that `/Users/brent/home/rafiki/.env` supplies
   a usable `ANTHROPIC_API_KEY` is wrong: the line is **commented out**
   (`#ANTHROPIC_API_KEY=…`), so sourcing `.env` sets nothing. The commented
   value was extracted into the executor's environment only; the key is valid
   but the account has no credits — the turn then returned
   `"Credit balance is too low"` (`session_id 865d419d-…`), the same account
   state the Task 1 pre-flight recorded for subagent dispatch.

Step 5's branch ("if step 4 returned nothing parseable, fix claudeargv") did
NOT fire — every frame was parseable and claude accepted the whole argv.
`claude --help` on 2.1.260 confirms every flag `claudeargv.Build` emits:
`-p`, `--input-format`, `--output-format`, `--verbose`,
`--dangerously-skip-permissions` (and `--permission-mode <mode>` exists for the
general case). **No correction to `pkg/claudeargv` was needed.** Two side
observations worth keeping: a claude whose turn fails stays ALIVE (waiting on
stdin, `is_error: true` on the result), so Task 6's respawn does not fire for
auth failures; and daraja exits 0 through its own SIGTERM handler when Reap's
SIGTERM arrives (`admin: daraja exited childID=p1bi-01 pid=72467 exitCode=0`).

### Step 6 — Reap and confirm nothing survives

`Reap{child_id: "p1bi-01", grace_ms: 3000}` → `{"reaped": true}` (twice, on
both the OAuth-failed and the relaunch): after each,

```
$ pgrep -fl 'rafiki daraja serve'   || echo no daraja
no daraja
$ pgrep -fl 'claude -p'             || echo no claude orphans
no claude orphans
$ ls /tmp/daraja-p1bi-01.sock
No such file or directory
```

Relaunching the SAME child_id after a reap works (supervise dropped the entry
when the reaped daraja exited) — no `AlreadyExists`.

### Step 7 — the orphan case

`kill -9` daraja (pid 73309) only. One second later claude (pid 73311) is
orphaned to launchd **with the group intact**, exactly as `admin.go`'s
no-`PR_SET_CHILD_SUBREAPER`-on-darwin comment predicts:

```
  PID  PPID PGID  STAT COMMAND
73311     1 73309 S    /Users/brent/.local/bin/claude -p --input-format stream-json ...
```

Polled at 0.25s intervals after the kill: alive at 0.0s/0.4s/0.9s with
`ppid=1 pgid=73309`, gone by 1.4s. **A claude orphan self-exits ~1s after its
daraja is SIGKILLed**: daraja's death closes the child's stdin pipe and
`claude -p` (stream-json) exits at EOF — before the group signal could land.
The follow-up `kill -TERM -73309` correctly reported ESRCH (empty group), which
is the same answer `AdminServer.reap`'s loop treats as success. So for THIS
child the group signal is belt-and-braces rather than load-bearing, and the
stdin-EOF behaviour is the actual cleanup path. The group mechanics themselves
were proven with a stub child (`exec sleep 300` via `--binary`) under the real
`rafiki daraja serve` given its own group (as Launch's `Setpgid` would):

```
  PID  PPID  PGID COMMAND
73210  ... 73210 ./bin/rafiki daraja serve --socket /tmp/p1bi-orphan.sock --binary <stub> --kind claude
73211 73210 73210 sleep 300
$ kill -9 73210          # daraja only
  PID  PPID PGID  STAT COMMAND
73211     1 73210 S    sleep 300      # orphaned, group intact
$ kill -TERM -73210
$ ps -p 73211 → reaped via the group
```

A pid-based reap cannot do this (pid 73210 was dead); the group is the only
handle, and it reached the orphan.

### Deviations from the plan as written

- **claude 2.1.260, not 2.1.252** (and not the pre-flight ledger's 2.1.259).
- **The executor-harness shape differs from the brief's named fallback** — a
  stub daemon completing the real executor's reverse dial, so the real
  `rafiki executor serve` binary launches the daraja. The brief's trigger
  ("serve refuses to start") was met in substance: serve starts but never
  serves the handler locally, so `AdminService` is unreachable without a peer.
- **Throwaway client location**: this workspace dir
  (`p1bi-client/`, module with `replace` → the worktree), not
  `/private/tmp/claude-501/.../scratchpad` — the pre-flight ruling stands.
- **`-- child-args` passthrough is gone, as Self-Review flagged**: the 1a
  recipe is invalid on this branch; `--kind claude --permission-mode
  bypassPermissions` is the replacement and is what Launch emits.
- **The `.env` ANTHROPIC credential is commented out** — the pre-flight
  instruction "source it and claude can reach the API" is a no-op. Neither of
  the machine's two claude auth paths works (expired OAuth that cannot
  refresh; API key with zero credits), so the model's literal reply is the one
  thing this section cannot attest to. Everything else — argv, protocol,
  group, reap, orphan handling — is verified against real processes.
- **`--credential` had to be passed to `executor serve`** (hello refuses
  without one) — absent from the brief's recipe.
- **`RAFIKI_PROXY_LISTEN=127.0.0.1:0` is a no-op for the executor** (it
  listens nowhere locally); harmless, kept for form.
- **The plan doc itself is gitignored** (`/docs/plans/` since the gitignore
  rule landed; this file existed only untracked in the main checkout). It was
  copied into the worktree for this append, committed with `git add -f`, and
  copied back so the operator's untracked copy stays current — the same
  force-add the pre-gitignore-era plans implicitly relied on.
- **Machine debris noted, not created here**: an orphaned
  `stub daraja serve` from an old `TestLaunchGivesDarajaItsOwnGroup` run (pid
  62930, PPID 1, `/usr/bin/true` child) was found by Step 3's `pgrep` and
  killed at cleanup. It matches the Task 10/11 parked minor ("Close returns
  without waiting on supervise") — that test can leave a daraja behind.

**Cleanup**: executor SIGTERMed (its `defer admin.Close()` reaped stragglers),
stub shut down, sockets `/tmp/nonexistent-daraja-p1bi.sock`,
`/tmp/p1bi-ctl.sock`, `/tmp/daraja-p1bi-01.sock` gone, `pgrep -fl 'daraja
serve'` and `pgrep -fl 'claude -p'` both empty. Logs kept at
`/tmp/p1bi-*.log`; the throwaway client stays in this scratch dir.

**Final gate**: `make check` was NOT re-run for this task. The only change is
this doc append (no Go code changed — the Step-5 argv-correction branch never
fired), and the gate was green at `e4fb74d` in Task 11.
