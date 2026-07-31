# Phase 0 — Configuration Ownership + Token Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make fundi judgeable as a daily-driver coding agent — it reads its own configuration instead of guessing at Claude's, and it streams tokens instead of dumping a finished message.

**Architecture:** Two independent workstreams. **A** (fundi repo) moves instructions/skills/presets discovery to fundi-owned XDG locations with `FUNDI_*` env overrides, keeping Claude-Code-compatible *schemas* while dropping hardcoded `~/.claude` and `~/.pi` *locations*. **B** (rafiki + fundi) adds an optional `StreamingSender` alongside the existing `Sender` so `Conversation` can stream, and splits fundi's `Emitter.AssistantTurn` into real start/update/end emission. B requires no protocol or TUI change — the frames already exist.

**Tech Stack:** Go 1.x, `anthropic-sdk-go v1.37.0`, `ssestream`, JSONL-over-UDS pi protocol.

**Spec:** `docs/plans/2026-07-28-fundi-cc-replacement-design.md` §Principle, §0.1, §0.2, §0.3.

## Global Constraints

- **Two repos.** Workstream A is entirely `~/home/fundi`. Workstream B Tasks B1–B2 are `~/home/rafiki`; B3–B5 are `~/home/fundi`. They are linked by `go.work` for local dev; CI resolves rafiki via the pinned `require` in `go.mod`.
- **Never silently swallow errors.** Log or return every failure. Empty `if err != nil {}` blocks are prohibited.
- **`go vet ./...` and `golangci-lint run ./...` must pass from the module root** before any commit.
- **Schemas stay Claude-Code-compatible; only locations become fundi-owned.** Do not rename `CLAUDE.md`, `AGENTS.md`, `.mcp.json`, `SKILL.md`, or the skills directory layout.
- **Deprecation pattern is established — follow it.** `internal/envvar` maps a current name to one prior spelling, reads the old one, warns, never errors. Mirror it exactly.
- **Do not touch `~/.pi/agent/extensions/`.** That is pi's genuine discovery contract and stays.
- **No `git add -A` / `git add .`.** Stage files by name. Run `git diff --cached --stat` and confirm only intended files are staged.
- **No `Co-Authored-By` trailers.**
- **A test must fail when the invariant it guards breaks — prove it, don't assume it.** Before committing any test described as locking something down, break the thing deliberately, watch the test fail, then restore. A test that passes either way is a defect and will be reviewed as one, no matter which document prescribed it. (Task A1 shipped exactly this defect from this plan's own prescribed code: the test drove the invariant through `envvar.Get()`, which early-returns on the primary variable and never reaches the branch under test.) If a test's assertion path cannot reach the behavior it names, fix the test rather than transcribing it.
- **Test helpers in this plan are illustrative of intent, not verified to exist.** Fakes and assertion helpers (`fakeFrontend`, `countOfType`, `typeSequence`, `framesOfType`, `lastOfType`, `newFakeStreamingSender`, `newTestEngine`, `newTestConversation`, `waitFor`) may need writing or adapting — check the existing `_test.go` files in the package first and follow whatever is already there. `internal/agent/faketurns.go` is the established scripted-turn seam; extend it rather than inventing a parallel one. Constructor signatures shown (e.g. `NewEmitter`) must be checked against the real code before use.

---

# Workstream A — Configuration Ownership (`~/home/fundi`)

### Task A1: Register the new environment variables

**Files:**
- Modify: `internal/envvar/envvar.go`
- Test: `internal/envvar/envvar_test.go`

**Interfaces:**
- Produces: `envvar.Instructions`, `envvar.SkillsDirs`, `envvar.MCPConfig` — string constants naming `FUNDI_INSTRUCTIONS`, `FUNDI_SKILLS_DIRS`, `FUNDI_MCP_CONFIG`. Read via the existing `envvar.Get(name) string`.

These are new names with no pre-rename spelling, so they get **no** `deprecated` map entry. That is the point of the test below — a future edit that adds one would silently resurrect a `PIC_*` path that never existed.

- [ ] **Step 1: Write the failing test**

```go
// Assert on the map directly. Going through Get() would NOT work: Get checks
// os.Getenv(name) first and returns immediately when it is non-empty, so a test
// that sets the primary variable never reaches the deprecated-map branch and
// would pass even after someone added deprecated[Instructions] = "PIC_...".
// This test must be an in-package test (package envvar) to see `deprecated`.
func TestNewVarsHaveNoDeprecatedSpelling(t *testing.T) {
	for _, name := range []string{Instructions, SkillsDirs, MCPConfig} {
		if _, ok := deprecated[name]; ok {
			t.Errorf("%s has a deprecated-map entry; these are new names with no pre-rename spelling", name)
		}
	}
}

func TestNewVarNamesAreFundiPrefixed(t *testing.T) {
	want := map[string]string{
		envvar.Instructions: "FUNDI_INSTRUCTIONS",
		envvar.SkillsDirs:   "FUNDI_SKILLS_DIRS",
		envvar.MCPConfig:    "FUNDI_MCP_CONFIG",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("constant = %q, want %q", got, expect)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./internal/envvar/ -run 'TestNewVar' -v`
Expected: FAIL — `undefined: envvar.Instructions`

- [ ] **Step 3: Add the constants**

In the `const` block in `internal/envvar/envvar.go`, after `AgentDB`:

```go
	// Instructions is the user-global instruction file. fundi's own config, so
	// it is NOT ~/.claude/CLAUDE.md — see internal/paths for why fundi does not
	// read its configuration out of another tool's directory. Point it at a
	// Claude profile explicitly if that is what you want.
	Instructions = "FUNDI_INSTRUCTIONS"

	// SkillsDirs is an OS-path-list of skill directories ($PATH convention:
	// ":" on unix). Ordered lowest-to-highest precedence, matching
	// agent.DiscoverSkills, so a later entry overrides an earlier one on name
	// collision. Non-existent entries are skipped, not errors.
	SkillsDirs = "FUNDI_SKILLS_DIRS"

	// MCPConfig is the path to a global .mcp.json, merged under the per-cwd one.
	MCPConfig = "FUNDI_MCP_CONFIG"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/home/fundi && go test ./internal/envvar/ -v`
Expected: PASS

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add internal/envvar/envvar.go internal/envvar/envvar_test.go
git diff --cached --stat
git commit -m "feat(envvar): add FUNDI_INSTRUCTIONS, FUNDI_SKILLS_DIRS, FUNDI_MCP_CONFIG"
```

---

### Task A2: Add fundi-owned config path resolvers

**Files:**
- Modify: `internal/paths/paths.go`
- Test: `internal/paths/paths_test.go` (create if absent)

**Interfaces:**
- Consumes: `envvar.Instructions`, `envvar.SkillsDirs`, `envvar.MCPConfig` (Task A1).
- Produces:
  - `paths.InstructionsFile() string` — `$FUNDI_INSTRUCTIONS`, else `<ConfigDir>/instructions.md`
  - `paths.SkillsDirs() []string` — split `$FUNDI_SKILLS_DIRS` on `os.PathListSeparator`, else `[<ConfigDir>/skills]`
  - `paths.PresetsFile() string` — `<ConfigDir>/presets.json`
  - `paths.GlobalMCPConfig() string` — `$FUNDI_MCP_CONFIG`, else `<ConfigDir>/mcp.json`

`internal/paths` does not currently import `internal/envvar`. Adding that import is correct and creates no cycle (`envvar` imports only `log/slog` and `os`). Verify with `go vet` after.

- [ ] **Step 1: Write the failing test**

```go
func TestSkillsDirs_DefaultIsConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	t.Setenv("FUNDI_SKILLS_DIRS", "")
	got := paths.SkillsDirs()
	want := []string{"/tmp/cfg/fundi/skills"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v", got, want)
	}
}

func TestSkillsDirs_SplitsPathList(t *testing.T) {
	t.Setenv("FUNDI_SKILLS_DIRS", "/a"+string(os.PathListSeparator)+"/b")
	got := paths.SkillsDirs()
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v", got, want)
	}
}

func TestSkillsDirs_DropsEmptySegments(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("FUNDI_SKILLS_DIRS", sep+"/a"+sep+sep+"/b"+sep)
	got := paths.SkillsDirs()
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v", got, want)
	}
}

func TestInstructionsFile_EnvWins(t *testing.T) {
	t.Setenv("FUNDI_INSTRUCTIONS", "/custom/inst.md")
	if got := paths.InstructionsFile(); got != "/custom/inst.md" {
		t.Fatalf("InstructionsFile() = %q, want /custom/inst.md", got)
	}
}

func TestNoClaudeOrPiPathsLeak(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	for _, env := range []string{"FUNDI_INSTRUCTIONS", "FUNDI_SKILLS_DIRS", "FUNDI_MCP_CONFIG"} {
		t.Setenv(env, "")
	}
	all := append(paths.SkillsDirs(),
		paths.InstructionsFile(), paths.PresetsFile(), paths.GlobalMCPConfig())
	for _, p := range all {
		if strings.Contains(p, "/.claude") || strings.Contains(p, "/.pi/") {
			t.Errorf("fundi config path leaks into a foreign tool's directory: %s", p)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./internal/paths/ -v`
Expected: FAIL — `undefined: paths.SkillsDirs`

- [ ] **Step 3: Implement**

Append to `internal/paths/paths.go` (add `"strings"` and the `envvar` import):

```go
// InstructionsFile is the user-global instruction file: $FUNDI_INSTRUCTIONS,
// else <ConfigDir>/instructions.md. Deliberately not ~/.claude/CLAUDE.md —
// that directory belongs to Claude Code, and fundi reads its own configuration
// from its own directory. Point the variable at a Claude profile to use one.
func InstructionsFile() string {
	if v := envvar.Get(envvar.Instructions); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "instructions.md")
}

// SkillsDirs is the ordered skill search path: $FUNDI_SKILLS_DIRS split on the
// OS path-list separator, else [<ConfigDir>/skills]. Order is
// lowest-to-highest precedence, matching agent.DiscoverSkills. Empty segments
// are dropped so a leading, trailing, or doubled separator is not read as the
// current directory.
func SkillsDirs() []string {
	v := envvar.Get(envvar.SkillsDirs)
	if v == "" {
		return []string{filepath.Join(ConfigDir(), "skills")}
	}
	var out []string
	for _, d := range strings.Split(v, string(os.PathListSeparator)) {
		if d != "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return []string{filepath.Join(ConfigDir(), "skills")}
	}
	return out
}

// PresetsFile is the presets file: <ConfigDir>/presets.json. It used to live at
// ~/.pi/agent/fundi-presets.json — fundi's own file inside pi's directory.
func PresetsFile() string { return filepath.Join(ConfigDir(), "presets.json") }

// GlobalMCPConfig is the machine-wide .mcp.json: $FUNDI_MCP_CONFIG, else
// <ConfigDir>/mcp.json. The per-cwd .mcp.json remains the primary source and
// takes precedence; this is the fallback for servers you want everywhere.
func GlobalMCPConfig() string {
	if v := envvar.Get(envvar.MCPConfig); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "mcp.json")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/home/fundi && go test ./internal/paths/ -v && go vet ./internal/paths/`
Expected: PASS, no import cycle.

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add internal/paths/paths.go internal/paths/paths_test.go
git diff --cached --stat
git commit -m "feat(paths): add fundi-owned instructions, skills, presets, mcp paths"
```

---

### Task A3: Read global instructions from fundi's own path

**Files:**
- Modify: `internal/agent/contextfiles.go:44-48`
- Test: `internal/agent/contextfiles_test.go`

**Interfaces:**
- Consumes: `paths.InstructionsFile()` (Task A2).
- Produces: no signature change. `LoadContextFiles(cwd string) (string, error)` keeps its contract; only the user-global source moves.

Per-repo `CLAUDE.md`/`AGENTS.md` at git root and cwd are **unchanged** — they are project files, not profile config.

- [ ] **Step 1: Write the failing test**

```go
func TestLoadContextFiles_UsesFundiInstructionsNotClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A Claude profile that must NOT be read.
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"),
		[]byte("CLAUDE-PROFILE-MARKER"), 0o644); err != nil {
		t.Fatal(err)
	}

	// fundi's own instructions, which must be read.
	inst := filepath.Join(t.TempDir(), "instructions.md")
	if err := os.WriteFile(inst, []byte("FUNDI-INSTRUCTIONS-MARKER"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUNDI_INSTRUCTIONS", inst)

	got, err := agent.LoadContextFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "FUNDI-INSTRUCTIONS-MARKER") {
		t.Error("did not load $FUNDI_INSTRUCTIONS")
	}
	if strings.Contains(got, "CLAUDE-PROFILE-MARKER") {
		t.Error("read ~/.claude/CLAUDE.md; fundi must not read its config from Claude's directory")
	}
}

func TestLoadContextFiles_MissingInstructionsIsNotAnError(t *testing.T) {
	t.Setenv("FUNDI_INSTRUCTIONS", filepath.Join(t.TempDir(), "absent.md"))
	if _, err := agent.LoadContextFiles(t.TempDir()); err != nil {
		t.Fatalf("missing instructions file must be skipped silently, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./internal/agent/ -run TestLoadContextFiles_Uses -v`
Expected: FAIL — "did not load $FUNDI_INSTRUCTIONS"

- [ ] **Step 3: Replace the home-relative block**

In `LoadContextFiles`, replace lines 44-48 (the `os.UserHomeDir()` / `.claude/CLAUDE.md` block) with:

```go
	if s := loadInstructionFile(paths.InstructionsFile()); s != "" {
		sections = append(sections, s)
	}
```

Update the doc comment's first line from `~/.claude/CLAUDE.md (user-global) first` to:

```go
// LoadContextFiles returns the concatenated instruction-file content for cwd:
// the user-global instruction file (paths.InstructionsFile) first, then
// CLAUDE.md and AGENTS.md at the git root, then CLAUDE.md and AGENTS.md at cwd
```

Remove the now-unused `os` import only if nothing else in the file uses it — check first; `loadInstructionFile` likely still does.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/home/fundi && go test ./internal/agent/ -v`
Expected: PASS, including pre-existing context-file tests.

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add internal/agent/contextfiles.go internal/agent/contextfiles_test.go
git diff --cached --stat
git commit -m "refactor(agent): read global instructions from fundi's own path, not ~/.claude"
```

---

### Task A4: Discover skills from fundi's own path

**Files:**
- Modify: `cmd/fundid/agent.go:129-140`
- Test: `cmd/fundid/agent_test.go`

**Interfaces:**
- Consumes: `paths.SkillsDirs()` (Task A2).
- Produces: no signature change. `agent.DiscoverSkills(dirs, only)` is unchanged — only the assembled `dirs` slice changes.

Precedence, lowest to highest: `paths.SkillsDirs()`, then `<cwd>/.fundi/skills`, then `--skills-dir` flags. The per-project directory moves from `.claude/skills` to `.fundi/skills` — it is fundi's config, and a repo can carry both.

- [ ] **Step 1: Write the failing test**

```go
func TestAssembleSkillDirs_NoClaudeHomeDir(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("FUNDI_SKILLS_DIRS", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")

	dirs := assembleSkillDirs("/work/repo", nil)

	for _, d := range dirs {
		if strings.Contains(d, "/.claude") {
			t.Errorf("skill dir must not be under a Claude profile: %s", d)
		}
	}
	if dirs[0] != "/tmp/cfg/fundi/skills" {
		t.Errorf("dirs[0] = %q, want /tmp/cfg/fundi/skills", dirs[0])
	}
	if dirs[1] != "/work/repo/.fundi/skills" {
		t.Errorf("dirs[1] = %q, want /work/repo/.fundi/skills", dirs[1])
	}
}

func TestAssembleSkillDirs_FlagsWinLast(t *testing.T) {
	t.Setenv("FUNDI_SKILLS_DIRS", "/env/skills")
	dirs := assembleSkillDirs("/work/repo", []string{"/flag/skills"})
	if dirs[len(dirs)-1] != "/flag/skills" {
		t.Errorf("--skills-dir must have highest precedence, got %v", dirs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./cmd/fundid/ -run TestAssembleSkillDirs -v`
Expected: FAIL — `undefined: assembleSkillDirs`

- [ ] **Step 3: Extract and reimplement**

Add to `cmd/fundid/agent.go`:

```go
// assembleSkillDirs builds the skill search path, lowest precedence first:
// the configured dirs (paths.SkillsDirs), then the project's own
// <cwd>/.fundi/skills, then any --skills-dir flags. Pure so it is testable.
//
// Deliberately excludes ~/.claude/skills: fundi does not read its
// configuration out of another tool's directory. Point $FUNDI_SKILLS_DIRS at a
// Claude skills tree (or a plugin cache) to use one.
func assembleSkillDirs(cwd string, flagDirs []string) []string {
	dirs := paths.SkillsDirs()
	dirs = append(dirs, filepath.Join(cwd, ".fundi", "skills"))
	return append(dirs, flagDirs...)
}
```

Replace the body of the `if !f.noSkills {` block's directory assembly (the `home, herr := os.UserHomeDir()` block through `dirs = append(dirs, f.skillsDir...)`) with:

```go
		dirs := assembleSkillDirs(cwd, f.skillsDir)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/home/fundi && go test ./cmd/fundid/ -v`
Expected: PASS

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add cmd/fundid/agent.go cmd/fundid/agent_test.go
git diff --cached --stat
git commit -m "refactor(agent): discover skills from fundi's own dirs, not ~/.claude/skills"
```

---

### Task A5: Move presets to fundi's config directory

**Files:**
- Modify: `cmd/fundi/presets.go:26-78`, `cmd/fundid/models_presets.go:36-76`
- Test: `cmd/fundi/presets_test.go`

**Interfaces:**
- Consumes: `paths.PresetsFile()` (Task A2).
- Produces: `PresetsFileName` is **deleted**; callers use `paths.PresetsFile()`. `loadPresets() (*PresetsFile, error)` keeps its signature.

The daemon-side copy in `cmd/fundid/models_presets.go` reads the same file and **must** move in the same commit, or `fundi presets` and the daemon's preset resolution will disagree about where presets live.

The pre-rename `pic-presets.json` probe is preserved but re-aimed: it now reports a file left at the *old directory* too. Both legacy locations are reported, never read, never deleted.

- [ ] **Step 1: Write the failing test**

```go
func TestPresetsPath_IsUnderFundiConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	got := paths.PresetsFile()
	if got != "/tmp/cfg/fundi/presets.json" {
		t.Fatalf("presets path = %q, want /tmp/cfg/fundi/presets.json", got)
	}
	if strings.Contains(got, "/.pi/") {
		t.Error("presets must not live in pi's directory")
	}
}

func TestMissingPresets_ReportsLegacyPiLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	legacyDir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, "fundi-presets.json")
	if err := os.WriteFile(legacy, []byte(`{"presets":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadPresets()
	if err == nil {
		t.Fatal("expected an error for a missing presets file")
	}
	if !strings.Contains(err.Error(), legacy) {
		t.Errorf("error must name the legacy file so it does not look like data loss; got: %v", err)
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Error("the legacy file must never be deleted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./cmd/fundi/ -run 'TestPresetsPath|TestMissingPresets' -v`
Expected: FAIL — path is still under `~/.pi/agent`.

- [ ] **Step 3: Implement**

In `cmd/fundi/presets.go`, delete `PresetsFileName`, `legacyPresetsFileName`, and `presetsPath`. Replace `loadPresets` and `missingPresetsError` with:

```go
// legacyPresetsPaths are pre-move locations. They are probed only to turn "no
// presets file" into an error that says what to do; they are never read and
// never deleted. ~/.pi/agent held fundi's own presets file inside pi's
// directory; the pic- spelling predates the binary rename.
func legacyPresetsPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".pi", "agent", "fundi-presets.json"),
		filepath.Join(home, ".pi", "agent", "pic-presets.json"),
	}
}

// loadPresets reads the presets file at paths.PresetsFile().
func loadPresets() (*PresetsFile, error) {
	path := paths.PresetsFile()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, missingPresetsError(path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pf PresetsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &pf, nil
}

// missingPresetsError reports the absent presets file, naming any pre-move file
// still on disk. Failing with a bare "not found" while the user's presets sit
// at the old path would look like data loss.
func missingPresetsError(path string) error {
	for _, legacy := range legacyPresetsPaths() {
		if _, statErr := os.Stat(legacy); statErr == nil {
			return fmt.Errorf("no presets file at %s; %s still exists and is no longer read — move it:\n    mkdir -p %s && mv %s %s",
				path, legacy, filepath.Dir(path), legacy, path)
		}
	}
	return fmt.Errorf("no presets file at %s", path)
}
```

In `cmd/fundid/models_presets.go`, delete the `presetsFileName` const and replace the `filepath.Join(home, ".pi", "agent", presetsFileName)` resolution with `paths.PresetsFile()`. Update the three `fmt.Errorf` sites that referenced `presetsFileName` to use the resolved path variable instead.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ~/home/fundi && go test ./cmd/fundi/ ./cmd/fundid/ -v`
Expected: PASS. Any pre-existing test asserting the `~/.pi` location must be **updated**, not deleted — the assertion moves to the new path.

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add cmd/fundi/presets.go cmd/fundi/presets_test.go cmd/fundid/models_presets.go
git diff --cached --stat
git commit -m "refactor(presets): move presets.json to fundi's config dir"
```

---

### Task A6: Surface the agent-kind knobs on `fundi create`

**Files:**
- Modify: `cmd/fundi/cmd_create.go:52-85`, `cmd/fundi/cmd_resume.go:60-71`, `cmd/fundid/controller.go:2116-2128` (`buildAgentArgv`), `protocol/types.go` (`SpawnRequest`)
- Test: `cmd/fundi/cmd_create_test.go`, `cmd/fundid/spawn_labels_test.go`

**Interfaces:**
- Consumes: nothing from A1–A5.
- Produces: `SpawnRequest.SkillsDirs []string` and `SpawnRequest.MCPConfig string`, JSON-tagged `skills_dirs` / `mcp_config`, both `omitempty`. `buildAgentArgv` renders them as repeated `--skills-dir` and a single `--mcp-config`.

Both are additive optional fields, so older clients keep working.

- [ ] **Step 1: Write the failing test**

```go
func TestBuildAgentArgv_RendersSkillsDirsAndMCPConfig(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:       "agent",
		Model:      "anthropic/claude-sonnet-5",
		SkillsDirs: []string{"/a/skills", "/b/skills"},
		MCPConfig:  "/cfg/.mcp.json",
	}
	argv := buildAgentArgv(req, "child-1", "/state")
	joined := strings.Join(argv, " ")

	if strings.Count(joined, "--skills-dir") != 2 {
		t.Errorf("want one --skills-dir per entry, got: %v", argv)
	}
	if !strings.Contains(joined, "--skills-dir /a/skills") ||
		!strings.Contains(joined, "--skills-dir /b/skills") {
		t.Errorf("skills dirs missing from argv: %v", argv)
	}
	if !strings.Contains(joined, "--mcp-config /cfg/.mcp.json") {
		t.Errorf("mcp config missing from argv: %v", argv)
	}
}

func TestBuildAgentArgv_OmitsUnsetKnobs(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent", Model: "anthropic/claude-sonnet-5"}
	joined := strings.Join(buildAgentArgv(req, "child-1", "/state"), " ")
	if strings.Contains(joined, "--skills-dir") || strings.Contains(joined, "--mcp-config") {
		t.Errorf("unset knobs must not appear: %s", joined)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./cmd/fundid/ -run TestBuildAgentArgv -v`
Expected: FAIL — `unknown field SkillsDirs`

- [ ] **Step 3: Implement**

Add to `SpawnRequest` in `protocol/types.go`:

```go
	// SkillsDirs are additional skill directories for an agent-kind child,
	// appended after the configured and project dirs (highest precedence).
	SkillsDirs []string `json:"skills_dirs,omitempty"`
	// MCPConfig overrides the .mcp.json path for an agent-kind child.
	MCPConfig string `json:"mcp_config,omitempty"`
```

In `buildAgentArgv`, before the trailing `req.ExtraArgs` append:

```go
	for _, d := range req.SkillsDirs {
		argv = append(argv, "--skills-dir", d)
	}
	if req.MCPConfig != "" {
		argv = append(argv, "--mcp-config", req.MCPConfig)
	}
```

Add to `addSpawnFlags` in `cmd/fundi/cmd_create.go` (shared with `cmd_resume.go`, so both gain the flags):

```go
	cmd.Flags().StringSlice("skills-dir", nil, "Additional skills directory for --kind agent (repeatable)")
	cmd.Flags().String("mcp-config", "", "Path to .mcp.json for --kind agent (default: <cwd>/.mcp.json)")
```

Wire both into the `SpawnRequest` construction alongside the existing flag reads.

- [ ] **Step 4: Consume the global MCP config, and correct the `--config-dir` help**

`paths.GlobalMCPConfig()` (Task A2) is otherwise dead code. In `cmd/fundid/usage.go`, change the `-mcp-config` flag's default resolution so an unset flag falls back to `<cwd>/.mcp.json` **then** `paths.GlobalMCPConfig()` — per-cwd still wins, per spec §Principle. Add a test asserting that precedence:

```go
func TestMCPConfigPrecedence_CwdBeatsGlobal(t *testing.T) {
	cwd := t.TempDir()
	cwdCfg := filepath.Join(cwd, ".mcp.json")
	if err := os.WriteFile(cwdCfg, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUNDI_MCP_CONFIG", filepath.Join(t.TempDir(), "global.json"))

	if got := resolveMCPConfig("", cwd); got != cwdCfg {
		t.Fatalf("resolveMCPConfig = %q, want the cwd file %q", got, cwdCfg)
	}
}

func TestMCPConfigPrecedence_GlobalUsedWhenNoCwdFile(t *testing.T) {
	global := filepath.Join(t.TempDir(), "global.json")
	if err := os.WriteFile(global, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUNDI_MCP_CONFIG", global)

	if got := resolveMCPConfig("", t.TempDir()); got != global {
		t.Fatalf("resolveMCPConfig = %q, want the global file %q", got, global)
	}
}
```

Separately, `cmd/fundi/cmd_create.go:73`'s `--config-dir` help currently reads as though it were general. Per spec §0.1 it is claude-kind-only — make that unmissable:

```go
	cmd.Flags().String("config-dir", "", "CLAUDE_CONFIG_DIR for --kind claude ONLY; ignored by --kind agent and --kind pi")
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd ~/home/fundi && go test ./cmd/... ./protocol/... -v`
Expected: PASS

- [ ] **Step 6: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add protocol/types.go cmd/fundid/controller.go cmd/fundid/spawn_labels_test.go cmd/fundid/usage.go cmd/fundi/cmd_create.go cmd/fundi/cmd_create_test.go
git diff --cached --stat
git commit -m "feat(cli): add --skills-dir and --mcp-config to create/resume"
```

---

### Task A7: Document the variables and wire the dev loop

**Files:**
- Modify: `.env.example`, `Makefile`, `README.md`
- Test: manual verification (documentation task)

**Interfaces:**
- Consumes: every variable from A1–A6.
- Produces: no code.

`FUNDI_AGENT_DB` is spec §0.3's requirement — without it the dogfood records no cost data, which is the metric the whole exercise exists to produce. It is documented here and must be set in the daemon's service environment before daily use begins.

- [ ] **Step 1: Add the variables to `.env.example`**

```sh
# --- Agent runtime configuration -------------------------------------------
# fundi reads its own configuration from its own directories. These point it
# elsewhere — e.g. at a Claude Code profile or a plugin skills cache.

# User-global instruction file. Default: ~/.config/fundi/instructions.md
#FUNDI_INSTRUCTIONS=~/.claude-personal/CLAUDE.md

# Skill directories, OS path-list separated (":" on unix), lowest precedence
# first. Default: ~/.config/fundi/skills
#FUNDI_SKILLS_DIRS=~/.claude-personal/skills:~/.claude-personal/plugins/cache/superpowers-marketplace/superpowers/6.2.0/skills

# Global .mcp.json. The per-cwd .mcp.json still wins.
# Default: ~/.config/fundi/mcp.json
#FUNDI_MCP_CONFIG=~/.config/fundi/mcp.json

# Postgres URL for agent conversation persistence. REQUIRED for per-turn cost
# accounting — unset means in-memory conversations and no cost data at all.
#FUNDI_AGENT_DB=postgres://localhost/fundi
```

- [ ] **Step 2: Add a Makefile target**

```make
print-config: build-cli # Show the resolved agent config paths
	@printf "instructions : %s\n" "$${FUNDI_INSTRUCTIONS:-~/.config/fundi/instructions.md}"
	@printf "skills       : %s\n" "$${FUNDI_SKILLS_DIRS:-~/.config/fundi/skills}"
	@printf "mcp          : %s\n" "$${FUNDI_MCP_CONFIG:-~/.config/fundi/mcp.json}"
	@printf "agent db     : %s\n" "$${FUNDI_AGENT_DB:-<unset — NO COST DATA>}"
```

- [ ] **Step 3: Update the README environment table**

Add four rows to the existing `## Environment` table:

```markdown
| `FUNDI_INSTRUCTIONS` | user-global instruction file (default `~/.config/fundi/instructions.md`) |
| `FUNDI_SKILLS_DIRS` | skill directories, path-list separated (default `~/.config/fundi/skills`) |
| `FUNDI_MCP_CONFIG` | global `.mcp.json` (default `~/.config/fundi/mcp.json`) |
| `FUNDI_AGENT_DB` | postgres URL for conversation persistence; **required for cost accounting** |
```

Also correct the existing "Presets are read from `~/.pi/agent/fundi-presets.json`" sentence in `## Paths` to `~/.config/fundi/presets.json`, and drop it from the "one thing fundi writes outside its own directories" claim — after Task A5 the extension is the only such thing.

- [ ] **Step 4: Verify**

Run: `cd ~/home/fundi && make print-config && grep -c FUNDI_SKILLS_DIRS .env.example README.md`
Expected: paths print; both files match.

- [ ] **Step 5: Commit**

```bash
cd ~/home/fundi
git add .env.example Makefile README.md
git diff --cached --stat
git commit -m "docs: document fundi-owned agent config variables"
```

---

# Workstream B — Token Streaming

**Independent of Workstream A.** B1–B2 are `~/home/rafiki`; B3–B5 are `~/home/fundi`.

### Task B1: Add the optional `StreamingSender` interface (rafiki)

**Files:**
- Modify: `llm/sender.go`
- Test: `llm/sender_test.go`

**Interfaces:**
- Produces: `llm.StreamingSender` interface embedding `Sender` and adding `NewStreaming(ctx, params) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error)`. `sdkSender` implements it, so `Anthropic()`, `OpenRouter()`, and `FromSDK()` all satisfy it.

`Sender` is **not** modified. Existing fakes keep compiling, and the upstream PR to `timescale/rafiki` stays purely additive.

- [ ] **Step 1: Write the failing test**

```go
func TestSdkSenderImplementsStreamingSender(t *testing.T) {
	var _ llm.StreamingSender = llm.Anthropic("test-key").(llm.StreamingSender)
	var _ llm.StreamingSender = llm.OpenRouter("test-key").(llm.StreamingSender)
}

// A Sender that does NOT stream must still satisfy Sender, so existing fakes
// and the non-streaming path keep working.
type nonStreamingSender struct{}

func (nonStreamingSender) New(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, nil
}

func TestNonStreamingSenderStillSatisfiesSender(t *testing.T) {
	var s llm.Sender = nonStreamingSender{}
	if _, ok := s.(llm.StreamingSender); ok {
		t.Fatal("nonStreamingSender must not satisfy StreamingSender")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/rafiki && go test ./llm/ -run Streaming -v`
Expected: FAIL — `undefined: llm.StreamingSender`

- [ ] **Step 3: Implement**

Add to `llm/sender.go` (import `github.com/anthropics/anthropic-sdk-go/packages/ssestream`):

```go
// StreamingSender is an optional capability: a Sender that can also open a
// streaming Messages call. Callers type-assert for it and fall back to New,
// so a Sender that cannot stream (test fakes, custom transports) stays valid.
// Kept separate from Sender so this remains an additive change upstream.
type StreamingSender interface {
	Sender
	NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error)
}

func (s sdkSender) NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	return s.client.Messages.NewStreaming(ctx, params), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/home/rafiki && go test ./llm/ -v`
Expected: PASS

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/rafiki
go vet ./... && golangci-lint run ./...
git add llm/sender.go llm/sender_test.go
git diff --cached --stat
git commit -m "feat(llm): add optional StreamingSender capability"
```

---

### Task B2: Stream through `Conversation` (rafiki)

**Files:**
- Modify: `llm/client.go:161+` (extract shared prep), `llm/conversation.go:259-290,445-482`
- Test: `llm/conversation_stream_test.go`

**Interfaces:**
- Consumes: `llm.StreamingSender` (Task B1).
- Produces:
  - `type StreamHandler func(ev anthropic.MessageStreamEventUnion)`
  - `func WithStreamHandler(h StreamHandler) SendOption`
  - `Conversation.Send` unchanged in signature; when a handler is set **and** the resolved sender implements `StreamingSender`, it streams and invokes the handler per event, still returning the accumulated `*anthropic.Message`.

**Do not duplicate `SendParams`.** It carries model-prefix stripping, slash-id OpenRouter routing, `applyProviderPrefs`, the `llm.send` tracing span, and breaker state. Extract that prologue into a shared unexported helper and build the streaming path on top, or the two will drift and streaming will silently route differently from non-streaming.

- [ ] **Step 1: Write the failing test**

```go
func TestSend_StreamHandlerReceivesEventsAndAccumulates(t *testing.T) {
	// fakeStreamingSender replays a scripted event sequence.
	sender := newFakeStreamingSender(
		textDelta("Hel"), textDelta("lo"), messageStop(),
	)
	conv := newTestConversation(t, sender)

	var seen []string
	msg, err := conv.Send(ctx, llm.UserText("hi"),
		llm.WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			if d := textOf(ev); d != "" {
				seen = append(seen, d)
			}
		}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, "") != "Hello" {
		t.Errorf("handler saw %q, want Hello", strings.Join(seen, ""))
	}
	if got := textOfMessage(msg); got != "Hello" {
		t.Errorf("accumulated message = %q, want Hello", got)
	}
}

func TestSend_FallsBackWhenSenderCannotStream(t *testing.T) {
	conv := newTestConversation(t, nonStreamingFake{reply: "Hello"})
	called := false
	msg, err := conv.Send(ctx, llm.UserText("hi"),
		llm.WithStreamHandler(func(anthropic.MessageStreamEventUnion) { called = true }))
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("handler must not fire for a non-streaming sender")
	}
	if textOfMessage(msg) != "Hello" {
		t.Error("non-streaming fallback must still return the message")
	}
}

func TestSend_NoHandlerUsesNonStreamingPath(t *testing.T) {
	sender := newFakeStreamingSender(textDelta("x"), messageStop())
	conv := newTestConversation(t, sender)
	if _, err := conv.Send(ctx, llm.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if sender.streamCalls != 0 {
		t.Error("no handler means no streaming call")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/rafiki && go test ./llm/ -run TestSend_Stream -v`
Expected: FAIL — `undefined: llm.WithStreamHandler`

- [ ] **Step 3: Implement**

Add the option to `llm/conversation.go`:

```go
// StreamHandler receives each streaming event as it arrives.
type StreamHandler func(ev anthropic.MessageStreamEventUnion)

// WithStreamHandler streams the response, invoking h per event. Ignored when
// the resolved sender does not implement StreamingSender — the call then
// behaves exactly as an unstreamed one, so a caller never has to branch.
func WithStreamHandler(h StreamHandler) SendOption {
	return func(c *sendConfig) { c.streamHandler = h }
}
```

Add `streamHandler StreamHandler` to `sendConfig`. In `sendWithTrim`, when `scfg.streamHandler != nil` and the upstream sender asserts to `StreamingSender`, call the streaming path instead of `c.client.SendParams`; accumulate with `Message.Accumulate(ev)` and return the accumulated message. The trim-retry loop is unchanged: it still retries on `isPromptTooLarge`.

Extract the routing prologue from `SendParams` into `func (c *Client) prepareSend(meta SendMeta, params anthropic.MessageNewParams) (upstream Upstream, fallbacks []Upstream, out anthropic.MessageNewParams)` and call it from both paths.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ~/home/rafiki && go test ./llm/ ./routing/ -v`
Expected: PASS, including all pre-existing `llm` tests.

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/rafiki
go vet ./... && golangci-lint run ./...
git add llm/client.go llm/conversation.go llm/conversation_stream_test.go
git diff --cached --stat
git commit -m "feat(llm): stream responses through Conversation via WithStreamHandler"
```

---

### Task B3: Emit real incremental frames (fundi)

**Files:**
- Modify: `internal/agent/emit.go:67-78`
- Test: `internal/agent/emit_test.go`

**Interfaces:**
- Consumes: nothing from B1–B2 directly (this task is pure emission).
- Produces:
  - `(*Emitter).StreamStart(msg child.PiAssistantMessage)` — emits `PiMessageStart` **once**
  - `(*Emitter).StreamDelta(msg child.PiAssistantMessage)` — emits `PiMessageUpdate`
  - `(*Emitter).StreamEnd(msg child.PiAssistantMessage)` — emits `PiMessageEnd`, accumulates, folds usage
  - `AssistantTurn` is retained unchanged for the non-streaming fallback path.

**Spec constraint §0.2:** `message_start` must not be emitted until the first content event arrives, so a `sendWithTrim` retry cannot leave abandoned text in the TUI. `StreamStart` is therefore idempotent-guarded by a `started bool` on `Emitter`, reset in `StreamEnd`.

- [ ] **Step 1: Write the failing test**

```go
func TestStreamStart_EmitsOnlyOnce(t *testing.T) {
	fe := &fakeFrontend{}
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamStart(msg)

	if n := fe.countOfType("message_start"); n != 1 {
		t.Fatalf("message_start emitted %d times, want 1", n)
	}
}

func TestStreamEnd_ResetsSoNextTurnStartsAgain(t *testing.T) {
	fe := &fakeFrontend{}
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamEnd(msg)
	e.StreamStart(msg)

	if n := fe.countOfType("message_start"); n != 2 {
		t.Fatalf("message_start emitted %d times across two turns, want 2", n)
	}
}

func TestStreamSequence_OrdersStartUpdatesEnd(t *testing.T) {
	fe := &fakeFrontend{}
	e := NewEmitter(fe, "anthropic", nil)
	msg := child.PiAssistantMessage{Role: "assistant"}

	e.StreamStart(msg)
	e.StreamDelta(msg)
	e.StreamDelta(msg)
	e.StreamEnd(msg)

	want := []string{"message_start", "message_update", "message_update", "message_end"}
	if got := fe.typeSequence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("frame order = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./internal/agent/ -run TestStream -v`
Expected: FAIL — `e.StreamStart undefined`

> **2026-07-30 audit note (task H7):** as of this correction, `-run TestStream`
> matches 5 tests in `internal/agent` (`TestStreamEndFoldsCostIdenticallyToAssistantTurn`,
> `TestStreamStart_EmitsOnlyOnce`, `TestStreamEnd_ResetsSoNextTurnStartsAgain`,
> `TestStreamSequence_OrdersStartUpdatesEnd`, `TestStreamDelta_DoesNotAccumulateOrFoldUsage`)
> — two more than this step's own three, added by later work. Verified with
> `-v`: every name prints `=== RUN` and `PASS`, so this is drift (over-broad),
> not the silent zero-match failure this audit was looking for. No correction
> needed; left as-is.

- [ ] **Step 3: Implement**

Add a `started bool` field to `Emitter`, then append to `internal/agent/emit.go`:

```go
// StreamStart emits message_start for a streaming assistant turn. It is
// idempotent within a turn: the caller invokes it on the first CONTENT event,
// not on the API's message_start, so that a sendWithTrim retry (which fails
// before any content arrives) cannot leave an orphaned start frame — and hence
// abandoned text — in an attached TUI.
func (e *Emitter) StreamStart(msg child.PiAssistantMessage) {
	if e.started {
		return
	}
	e.started = true
	e.fe.Emit(child.PiMessageStart(msg, ""))
}

// StreamDelta emits one message_update carrying the message accumulated so far.
func (e *Emitter) StreamDelta(msg child.PiAssistantMessage) {
	e.fe.Emit(child.PiMessageUpdate(msg, ""))
}

// StreamEnd emits message_end, accumulates the finished message for agent_end,
// and folds its usage into the turn total — the same bookkeeping AssistantTurn
// does, so cost accounting is identical on both paths.
func (e *Emitter) StreamEnd(msg child.PiAssistantMessage) {
	e.fe.Emit(child.PiMessageEnd(msg, ""))
	e.accumulate(msg)
	e.addUsage(msg.Usage)
	e.started = false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ~/home/fundi && go test ./internal/agent/ -v`
Expected: PASS, including the existing `emit_cost_test.go`.

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add internal/agent/emit.go internal/agent/emit_test.go
git diff --cached --stat
git commit -m "feat(agent): add incremental stream emission to Emitter"
```

---

### Task B4: Drive streaming from the engine (fundi)

**Files:**
- Modify: `internal/agent/engine.go`, `internal/agent/config.go`
- Test: `internal/agent/engine_test.go`

**Interfaces:**
- Consumes: `llm.WithStreamHandler` (B2); `Emitter.StreamStart/StreamDelta/StreamEnd` (B3).
- Produces: no exported signature change. The engine passes a handler that maps SDK events onto emitter calls and accumulates via `Message.Accumulate`.

**Spec constraint §0.2:** tool-call inputs arrive as partial JSON (`input_json_delta`) and must fully accumulate before dispatch. Dispatch already happens after the turn — do **not** move it, and do not fire `ToolStart` on first sight of a `tool_use` block.

- [ ] **Step 1: Write the failing test**

```go
func TestEngine_StreamsDeltasThenPricesAccumulatedMessage(t *testing.T) {
	sender := newFakeStreamingSender(
		textDelta("Hel"), textDelta("lo"),
		usageDelta(10, 20), messageStop(),
	)
	fe := &fakeFrontend{}
	eng := newTestEngine(t, sender, fe)

	if err := eng.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}

	if n := fe.countOfType("message_update"); n < 2 {
		t.Errorf("want a message_update per delta, got %d", n)
	}
	end := fe.lastOfType("agent_end")
	if end.Usage.InputTokens != 10 || end.Usage.OutputTokens != 20 {
		t.Errorf("usage = %+v, want in=10 out=20", end.Usage)
	}
}

func TestEngine_ToolUseDispatchesOnlyAfterInputFullyAccumulates(t *testing.T) {
	sender := newFakeStreamingSender(
		toolUseStart("call-1", "bash"),
		inputJSONDelta(`{"comm`), inputJSONDelta(`and":"ls"}`),
		messageStop(),
	)
	fe := &fakeFrontend{}
	eng := newTestEngine(t, sender, fe)

	if err := eng.Prompt(ctx, "list files"); err != nil {
		t.Fatal(err)
	}

	starts := fe.framesOfType("tool_execution_start")
	if len(starts) != 1 {
		t.Fatalf("want exactly one tool_execution_start, got %d", len(starts))
	}
	if starts[0].Args["command"] != "ls" {
		t.Errorf("tool dispatched with partial input: %+v", starts[0].Args)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/home/fundi && go test ./internal/agent/ -run TestEngine_Stream -v`
Expected: FAIL — no `message_update` per delta (all three frames still fire at once).

> **2026-07-30 audit note (task H7):** `-run TestEngine_Stream` still matches
> `TestEngine_StreamsDeltasAndPricesFinalMessageOnce` and
> `TestEngine_StreamsMessageUpdateBeforeTurnCompletes` today (both `=== RUN` +
> `PASS`) — nonzero, just not exactly this step's original single test. Not the
> silent zero-match failure; no correction needed.

- [ ] **Step 3: Implement**

At the engine's send site, replace the bare `conv.Send(...)` with a streaming variant:

```go
	var acc anthropic.Message
	resp, err := conv.Send(ctx, content,
		llm.WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			if err := acc.Accumulate(ev); err != nil {
				slog.Warn("agent: accumulate stream event", "error", err)
				return
			}
			// Emit only once content actually exists: a trim-retry fails before
			// any content event, so this keeps message_start off the wire until
			// the attempt is real.
			if !hasContent(&acc) {
				return
			}
			msg := MapAssistantMessage(&acc, e.provider, e.pricer)
			e.StreamStart(msg)
			e.StreamDelta(msg)
		}))
	if err != nil {
		return err
	}
	e.StreamEnd(MapAssistantMessage(resp, e.provider, e.pricer))
```

Note `StreamEnd` prices the **final** `resp`, so cost still resolves once against the served model exactly as before.

Also add the helper this depends on, in the same file:

```go
// hasContent reports whether any content block has arrived yet. Guards the
// first emission: sendWithTrim can retry a prompt-too-large failure, and that
// failure lands before any content event, so gating on this keeps an abandoned
// attempt from putting a message_start (and therefore text) into an attached
// TUI. See spec §0.2.
func hasContent(m *anthropic.Message) bool {
	for _, b := range m.Content {
		switch {
		case b.Type == "text" && b.Text != "":
			return true
		case b.Type == "tool_use":
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ~/home/fundi && go test ./internal/agent/... -v`
Expected: PASS, including `engine_test.go`, `resume_test.go`, `emit_cost_test.go`.

- [ ] **Step 5: Vet, lint, commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add internal/agent/engine.go internal/agent/config.go internal/agent/engine_test.go
git diff --cached --stat
git commit -m "feat(agent): stream assistant turns token-by-token"
```

---

### Task B5: End-to-end streaming verification

**Files:**
- Test: `internal/agent/engine_test.go` (add), plus a manual live check

**Interfaces:**
- Consumes: everything from B1–B4.
- Produces: no code.

- [ ] **Step 1: Write the regression test**

```go
// The whole point of Workstream B: deltas must arrive spread across the turn,
// not batched at the end. A regression here is invisible in every other test,
// because the frame TYPES are identical either way — only the timing differs.
func TestEngine_DeltasArriveBeforeTurnCompletes(t *testing.T) {
	release := make(chan struct{})
	sender := newBlockingStreamingSender(release, textDelta("a"), textDelta("b"), messageStop())
	fe := &fakeFrontend{}
	eng := newTestEngine(t, sender, fe)

	done := make(chan error, 1)
	go func() { done <- eng.Prompt(ctx, "hi") }()

	// One delta has been sent; the turn is deliberately not finished.
	waitFor(t, func() bool { return fe.countOfType("message_update") >= 1 })
	if fe.countOfType("agent_end") != 0 {
		t.Fatal("agent_end arrived before the turn completed")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails against the pre-B4 emitter**

Run: `cd ~/home/fundi && git stash && go test ./internal/agent/ -run DeltasArriveBefore -v; git stash pop`
Expected: FAIL before B4, PASS after. If it passes both ways the test proves nothing — fix the test.

> **2026-07-30 audit correction (task H7):** `TestEngine_DeltasArriveBeforeTurnCompletes`
> was later renamed to `TestEngine_StreamsMessageUpdateBeforeTurnCompletes`
> (`internal/agent/engine_stream_test.go`). `-run DeltasArriveBefore` now matches
> zero tests and `go test` prints `ok`/exits 0 with no `=== RUN` line — exactly
> the silent-pass failure this plan's own preamble warns about. Re-running this
> proof step today requires `-run TestEngine_StreamsMessageUpdateBeforeTurnCompletes`.
> Left the code block above as originally implemented (historical record); only
> the run command is corrected.

- [ ] **Step 3: Live verification**

```bash
cd ~/home/fundi && make build
./bin/fundi create --kind agent --model anthropic/claude-sonnet-5 stream-check
```

Ask it to write a few paragraphs. Confirm text appears progressively rather than in one block. Then, per spec §Risks, exercise the rendering path that has no test coverage:

```bash
./bin/fundi tail stream-check --no-deltas=false
```

Watch for wide-character or box-drawing corruption in `render_tail.go` — this is smoke step 9, which streaming stresses far harder than batch emission did.

- [ ] **Step 4: Exercise resume**

Per spec §Risks, agent-kind `ctrl_resume` was broken until `e583dc1` and is the least-proven path:

```bash
./bin/fundi kill stream-check
./bin/fundi resume stream-check
```

Confirm the resumed child streams normally.

- [ ] **Step 5: Commit**

```bash
cd ~/home/fundi
go vet ./... && golangci-lint run ./...
git add internal/agent/engine_test.go
git diff --cached --stat
git commit -m "test(agent): assert deltas arrive before turn completion"
```

---

## Definition of Done

- [ ] `go vet ./...` and `golangci-lint run ./...` pass in **both** repos.
- [ ] No fundi-owned config path resolves under `~/.claude` or `~/.pi` (asserted by `TestNoClaudeOrPiPathsLeak`).
- [ ] `FUNDI_SKILLS_DIRS` pointed at a Claude skills tree discovers those skills.
- [ ] Text appears progressively in an attached TUI.
- [ ] `FUNDI_AGENT_DB` is set in the daemon's service environment — **spec §0.3; without it the dogfood produces no cost data at all.**
- [ ] Deferred to Phase 1, explicitly NOT in scope here: `PreToolUse` hooks, subagents, compaction.
