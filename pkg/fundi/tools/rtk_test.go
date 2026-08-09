package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRTK creates a fake rtk binary in a temp directory, adds it to PATH, and
// resets the version probe cache to reflect the new PATH. Returns a cleanup
// function that restores the original PATH.
func fakeRTK(t *testing.T) (cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "rtk"))
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/bash
if [ "$1" = "--version" ]; then
  echo "rtk 0.45.0"
  exit 0
fi
echo "$@"
exit 0
`
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		t.Fatal(err)
	}

	// t.Setenv, not os.Setenv: the manual save/restore leaked. PATH was
	// swapped by several tests and restored to whatever it happened to be
	// when *their* cleanup ran, so with -shuffle=on a later test could find
	// an empty PATH and fail with `exec: "bash": executable file not found`.
	// t.Setenv restores per-test, in order, automatically.
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Override the probe so it actually checks the new PATH
	resetRTKCache(func() rtkCache {
		p, err := exec.LookPath("rtk")
		if err != nil {
			return rtkCache{}
		}
		// Probe the binary to populate the cache
		_, _ = exec.Command(p, "--version").Output()
		return rtkCache{path: p, ok: true}
	})
	t.Cleanup(func() { resetRTKCache(nil) })

	return func() {}
}

// fakeRTKVersion creates a fake rtk that reports a specific version string.
// versionOutput is the full line that `rtk --version` prints (e.g. "rtk 0.22.0").
func fakeRTKVersion(t *testing.T, versionOutput string) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "rtk"))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/bash\nif [ \"$1\" = \"--version\" ]; then\n  echo \"" + versionOutput + "\"\n  exit 0\nfi\necho \"$@\"\nexit 0\n"
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Parse the version from the output to determine ok status.
	v := strings.TrimPrefix(versionOutput, "rtk ")
	if idx := strings.IndexByte(v, ' '); idx >= 0 {
		v = v[:idx]
	}
	versionOK := semverGTE(v, 0, 23, 0)

	resetRTKCache(func() rtkCache {
		p, _ := exec.LookPath("rtk")
		return rtkCache{path: p, ok: versionOK}
	})
	t.Cleanup(func() { resetRTKCache(nil) })
}

func TestHasShellChaining(t *testing.T) {
	cases := []struct {
		command  string
		chaining bool
	}{
		// Simple chaining operators
		{"git log | head", true},
		{"git log|head", true},
		{"git log || echo fail", true},
		{"echo a && echo b", true},
		{"echo a; echo b", true},
		{"echo a ; echo b", true},

		// Redirections
		{"echo hi > file.txt", true},
		{"echo hi >> file.txt", true},
		{"cat < file.txt", true},
		{"grep foo < file.txt", true},

		// Backtick substitution
		{"echo `date`", true},

		// $( ) substitution
		{"echo $(date)", true},
		{"echo $(pwd)", true},

		// Background
		{"sleep 10 &", true},
		{"echo hi & echo there", true},

		// Safe commands (no chaining)
		{"git status", false},
		{"git log -10", false},
		{"git commit -m 'hello world'", false},
		{"find . -name '*.go'", false},
		{"grep -rn 'pattern' src/", false},
		{"ls -la", false},
		{"docker ps", false},
		{"make build", false},
		{"echo hello", false},
		{"", false},

		// Operators inside quotes should NOT trigger chaining
		{`echo "hello | world"`, false},
		{`echo 'hello && world'`, false},
		{`git commit -m "fix && refactor"`, false},
		{`echo "pipe: |"`, false},
		{`echo 'semi;colon'`, false},

		// Escaped characters in double quotes
		{`echo "escaped \" quote"`, false},

		// $( inside quotes is safe
		{`echo "$(not expandable)"`, false},
		{`echo '$(not expandable)'`, false},

		// Operators in single quotes are safe
		{`awk '{print $1}'`, false},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got := hasShellChaining(tc.command)
			if got != tc.chaining {
				t.Errorf("hasShellChaining(%q) = %v, want %v", tc.command, got, tc.chaining)
			}
		})
	}
}

func TestRtkRewriteChainingGuard(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	chained := []string{
		"git log | head",
		"git branch && git status",
		"echo hi; ls",
		"cat file > out.txt",
		"echo `whoami`",
		"echo $(date)",
		"ls &",
	}
	for _, cmd := range chained {
		t.Run(cmd, func(t *testing.T) {
			_, applied := rtkRewrite(RTKAuto, cmd)
			if applied {
				t.Errorf("rtkRewrite(%q) should not rewrite a chained command", cmd)
			}
		})
	}
}

func TestRtkRewriteRTKOff(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	_, applied := rtkRewrite(RTKOff, "git status")
	if applied {
		t.Error("rtkRewrite with RTKOff should return applied=false")
	}
}

func TestRtkRewriteMapping(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	cases := []struct {
		command string
		want    []string
	}{
		{"git status", []string{"rtk", "git", "status"}},
		{"git log -10", []string{"rtk", "git", "log", "-10"}},
		{"git diff --cached", []string{"rtk", "git", "diff", "--cached"}},
		{"yadm status", []string{"rtk", "git", "status"}},
		{"ls -la", []string{"rtk", "ls", "-la"}},
		{"tree -L 2", []string{"rtk", "tree", "-L", "2"}},
		{"cat file.txt", []string{"rtk", "read", "file.txt"}},
		// head and tail are deliberately not here; see
		// TestRtkRewriteLeavesHeadAndTailAlone.
		{"find . -name '*.go'", []string{"rtk", "find", ".", "-name", "*.go"}},
		{"grep pattern file", []string{"rtk", "grep", "pattern", "file"}},
		{"rg pattern", []string{"rtk", "rg", "pattern"}},
		{"diff a.txt b.txt", []string{"rtk", "diff", "a.txt", "b.txt"}},
		{"docker ps", []string{"rtk", "docker", "ps"}},
		{"kubectl get pods", []string{"rtk", "kubectl", "get", "pods"}},
		{"oc get pods", []string{"rtk", "oc", "get", "pods"}},
		{"aws s3 ls", []string{"rtk", "aws", "s3", "ls"}},
		{"gh pr list", []string{"rtk", "gh", "pr", "list"}},
		{"glab mr list", []string{"rtk", "glab", "mr", "list"}},
		{"cargo build", []string{"rtk", "cargo", "build"}},
		{"pnpm list", []string{"rtk", "pnpm", "list"}},
		{"npx tsc --noEmit", []string{"rtk", "npx", "tsc", "--noEmit"}},
		{"psql -c 'select 1'", []string{"rtk", "psql", "-c", "select 1"}},
		{"go test ./...", []string{"rtk", "go", "test", "./..."}},
		{"make build", []string{"rtk", "make", "build"}},
		{"dotnet build", []string{"rtk", "dotnet", "build"}},
		{"curl -s https://example.com", []string{"rtk", "curl", "-s", "https://example.com"}},
		{"ruff check src/", []string{"rtk", "ruff", "check", "src/"}},
		{"tsc --noEmit", []string{"rtk", "tsc", "--noEmit"}},
		{"jest --coverage", []string{"rtk", "jest", "--coverage"}},
		{"vitest run", []string{"rtk", "vitest", "run"}},
		{"playwright test", []string{"rtk", "playwright", "test"}},
		{"pytest -xvs", []string{"rtk", "pytest", "-xvs"}},
		{"mypy src/", []string{"rtk", "mypy", "src/"}},
		{"pip list", []string{"rtk", "pip", "list"}},
		{"pip3 install requests", []string{"rtk", "pip", "install", "requests"}},
		{"bundle install", []string{"rtk", "bundle", "install"}},
		{"rspec spec/", []string{"rtk", "rspec", "spec/"}},
		{"rubocop -a", []string{"rtk", "rubocop", "-a"}},
		{"composer install", []string{"rtk", "composer", "install"}},
		{"df -h", []string{"rtk", "df", "-h"}},
		{"du -sh .", []string{"rtk", "du", "-sh", "."}},
		{"gcloud auth list", []string{"rtk", "gcloud", "auth", "list"}},
		{"gradlew build", []string{"rtk", "gradlew", "build"}},
		{"helm list", []string{"rtk", "helm", "list"}},
		{"ping -c 3 example.com", []string{"rtk", "ping", "-c", "3", "example.com"}},
		{"poetry install", []string{"rtk", "poetry", "install"}},
		{"pre-commit run", []string{"rtk", "pre-commit", "run"}},
		{"ps aux", []string{"rtk", "ps", "aux"}},
		{"pulumi up", []string{"rtk", "pulumi", "up"}},
		{"rsync -av src/ dst/", []string{"rtk", "rsync", "-av", "src/", "dst/"}},
		{"shellcheck script.sh", []string{"rtk", "shellcheck", "script.sh"}},
		{"swift build", []string{"rtk", "swift", "build"}},
		{"terraform plan", []string{"rtk", "terraform", "plan"}},
		{"tofu init", []string{"rtk", "tofu", "init"}},
		{"wc -l file.txt", []string{"rtk", "wc", "-l", "file.txt"}},
		{"gt log", []string{"rtk", "gt", "log"}},
		{"liquibase update", []string{"rtk", "liquibase", "update"}},
		{"biome check src/", []string{"rtk", "lint", "check", "src/"}},
		{"eslint src/", []string{"rtk", "lint", "src/"}},
		{"prettier --check .", []string{"rtk", "prettier", "--check", "."}},
		{"prisma generate", []string{"rtk", "prisma", "generate"}},
		{"mvn compile", []string{"rtk", "mvn", "compile"}},
		{"mvnw test", []string{"rtk", "mvn", "test"}},
		{"systemctl status nginx", []string{"rtk", "systemctl", "status", "nginx"}},
		{"uv sync", []string{"rtk", "uv", "sync"}},
		{"ansible-playbook deploy.yml", []string{"rtk", "ansible-playbook", "deploy.yml"}},
		{"brew install ripgrep", []string{"rtk", "brew", "install", "ripgrep"}},
		{"yamllint config.yaml", []string{"rtk", "yamllint", "config.yaml"}},
		{"iptables -L", []string{"rtk", "iptables", "-L"}},
		{"shopify theme push", []string{"rtk", "shopify", "theme", "push"}},
		{"sops decrypt secrets.yaml", []string{"rtk", "sops", "decrypt", "secrets.yaml"}},
		{"markdownlint README.md", []string{"rtk", "markdownlint", "README.md"}},
		{"hadolint Dockerfile", []string{"rtk", "hadolint", "Dockerfile"}},
		{"fail2ban-client status", []string{"rtk", "fail2ban-client", "status"}},
		{"mix compile", []string{"rtk", "mix", "compile"}},
		{"pio run", []string{"rtk", "pio", "run"}},
		{"quarto render", []string{"rtk", "quarto", "render"}},
		{"trunk build", []string{"rtk", "trunk", "build"}},
		{"wget https://example.com", []string{"rtk", "wget", "https://example.com"}},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			argv, applied := rtkRewrite(RTKAuto, tc.command)
			if !applied {
				t.Fatalf("rtkRewrite(%q) should be applied", tc.command)
			}
			if !stringSlicesEqual(argv, tc.want) {
				t.Errorf("rtkRewrite(%q) = %v, want %v", tc.command, argv, tc.want)
			}
		})
	}
}

func TestRtkRewriteUnmapped(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	unmapped := []string{
		"echo hello",
		"cd /tmp",
		"which rtk",
		"npm test",    // npm requires exec/run/run-script/rum/urn/x
		"npm install", // same
		"python -c 'print(1)'",
		"node -e '1+1'",
		"mkdir foo",
		"rm -rf /tmp/foo",
		"cp a b",
		"mv a b",
		"chmod +x script.sh",
		"touch file.txt",
		"sleep 10",
		"kill -9 1234",
		"export FOO=bar",
		"htop",
		"",
		"   ",
	}
	for _, cmd := range unmapped {
		t.Run(cmd, func(t *testing.T) {
			_, applied := rtkRewrite(RTKAuto, cmd)
			if applied {
				t.Errorf("rtkRewrite(%q) should NOT be applied (unmapped command)", cmd)
			}
		})
	}
}

func TestRtkRewriteNpmSubcommand(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	// npm exec, run, run-script, rum, urn, x → rewritten
	mapped := []string{
		"npm run build",
		"npm exec tsc",
		"npm run-script lint",
		"npm rum test",
		"npm urn start",
		"npm x vitest",
	}
	for _, cmd := range mapped {
		t.Run(cmd, func(t *testing.T) {
			_, applied := rtkRewrite(RTKAuto, cmd)
			if !applied {
				t.Errorf("rtkRewrite(%q) should be applied (valid npm subcommand)", cmd)
			}
		})
	}

	// npm without subcommand, or with unrecognized subcommand → not rewritten
	unmapped := []string{
		"npm",
		"npm test",
		"npm install",
		"npm ci",
		"npm publish",
	}
	for _, cmd := range unmapped {
		t.Run(cmd, func(t *testing.T) {
			_, applied := rtkRewrite(RTKAuto, cmd)
			if applied {
				t.Errorf("rtkRewrite(%q) should NOT be applied", cmd)
			}
		})
	}
}

func TestRtkRewriteVersionGuardTooOld(t *testing.T) {
	fakeRTKVersion(t, "rtk 0.22.0")
	_, applied := rtkRewrite(RTKAuto, "git status")
	if applied {
		t.Error("rtkRewrite should return applied=false for rtk < 0.23.0")
	}
}

func TestRtkRewriteVersionGuardUnparseable(t *testing.T) {
	fakeRTKVersion(t, "rtk nope")
	_, applied := rtkRewrite(RTKAuto, "git status")
	if applied {
		t.Error("rtkRewrite should return applied=false for unparseable version")
	}
}

func TestRtkRewriteMissingRtk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	resetRTKCache(func() rtkCache { return rtkCache{} })
	t.Cleanup(func() { resetRTKCache(nil) })

	_, applied := rtkRewrite(RTKAuto, "git status")
	if applied {
		t.Error("rtkRewrite should return applied=false when rtk is missing")
	}
}

func TestRtkRewriteRTKOnNoRtk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	resetRTKCache(func() rtkCache { return rtkCache{} })
	t.Cleanup(func() { resetRTKCache(nil) })

	_, applied := rtkRewrite(RTKOn, "git status")
	if applied {
		t.Error("rtkRewrite should fail open even in RTKOn mode when rtk is missing")
	}
}

func TestRtkRewriteRTKOnWithVersionOk(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	_, applied := rtkRewrite(RTKOn, "git status")
	if !applied {
		t.Error("rtkRewrite should return applied=true in RTKOn mode with valid rtk")
	}
}

func TestFailOpenGuards(t *testing.T) {
	cleanup := fakeRTK(t)
	defer cleanup()

	cases := []struct {
		name string
		cmd  string
	}{
		{"pipe", "git log | head"},
		{"double ampersand", "a && b"},
		{"semicolon", "a; b"},
		{"redirect out", "echo hi > file"},
		{"redirect in", "cat < file"},
		{"backtick", "echo `date`"},
		{"dollar paren", "echo $(date)"},
		{"background", "sleep 10 &"},
		{"double pipe", "a || b"},
		{"empty command", ""},
		{"whitespace only", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, applied := rtkRewrite(RTKAuto, tc.cmd)
			if applied {
				t.Errorf("rtkRewrite(%q) = (%v, true), want (nil, false)", tc.cmd, argv)
			}
		})
	}
}

func TestShellSplit(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"git status", []string{"git", "status"}},
		{"git log -10", []string{"git", "log", "-10"}},
		{`git commit -m "hello world"`, []string{"git", "commit", "-m", "hello world"}},
		{`echo 'hello world'`, []string{"echo", "hello world"}},
		{`find . -name "*.go"`, []string{"find", ".", "-name", "*.go"}},
		{"", nil},
		{"   ", nil},
		{"  ls  -la  ", []string{"ls", "-la"}},
		{`awk '{print $1}'`, []string{"awk", "{print $1}"}},
		{`echo "escaped \"quote\""`, []string{"echo", `escaped "quote"`}},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := shellSplit(tc.input)
			if !stringSlicesEqual(got, tc.want) {
				t.Errorf("shellSplit(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSemverGTE(t *testing.T) {
	cases := []struct {
		v   string
		maj int
		min int
		pat int
		ok  bool
	}{
		{"0.45.0", 0, 23, 0, true},
		{"0.23.0", 0, 23, 0, true},
		{"0.23.1", 0, 23, 0, true},
		{"0.22.9", 0, 23, 0, false},
		{"0.22.0", 0, 23, 0, false},
		{"1.0.0", 0, 23, 0, true},
		{"0.23", 0, 23, 0, true}, // missing patch
		{"0.22", 0, 23, 0, false},
		{"", 0, 23, 0, false},
		{"nope", 0, 23, 0, false},
		{"rtk 0.45.0", 0, 23, 0, true}, // with prefix (should be stripped by caller)
	}
	for _, tc := range cases {
		t.Run(tc.v, func(t *testing.T) {
			got := semverGTE(tc.v, tc.maj, tc.min, tc.pat)
			if got != tc.ok {
				t.Errorf("semverGTE(%q, %d, %d, %d) = %v, want %v",
					tc.v, tc.maj, tc.min, tc.pat, got, tc.ok)
			}
		})
	}
}

// TestRtkRewriteLeavesHeadAndTailAlone: head/tail were mapped to "read",
// producing `rtk read -20 app.log`. That reaches bash's builtin read, which
// answers "read: -2: invalid option" and exits 2 — so `head -50 server.log`
// returned a usage error instead of the file. Upstream's rtk-rewrite.sh
// does not map them, and the real binary translates the count flag
// (`rtk read README.md --max-lines 20`), which a bare prefix swap cannot
// do. Falling through to plain bash is the correct behaviour.
func TestRtkRewriteLeavesHeadAndTailAlone(t *testing.T) {
	// Without a working rtk on PATH every rewrite fails open, which
	// would make the negative assertions below pass for the wrong
	// reason.
	cleanup := fakeRTK(t)
	defer cleanup()

	for _, cmd := range []string{
		"head README.md",
		"head -20 README.md",
		"tail -20 app.log",
		"tail -f app.log",
	} {
		if argv, applied := rtkRewrite(RTKAuto, cmd); applied {
			t.Errorf("rtkRewrite(%q) was rewritten to %v; it must fall through to bash", cmd, argv)
		}
	}
}

// TestRtkRewriteRefusesMultilineCommands is the regression test for the
// worst rtk defect: newline was not in the guard set and shellSplit breaks
// only on space and tab, so a newline was absorbed into a token.
// "git add -A\ngit commit -m wip" became one git call with a pathspec of
// "-A\ngit" — the add failed, the commit never ran, and the model saw no
// sign a second command had existed.
func TestRtkRewriteRefusesMultilineCommands(t *testing.T) {
	// Without a working rtk on PATH every rewrite fails open, which
	// would make the negative assertions below pass for the wrong
	// reason.
	cleanup := fakeRTK(t)
	defer cleanup()

	for _, cmd := range []string{
		"git add -A\ngit commit -m wip",
		"git status\r\ngit log",
		"docker ps\ndocker images",
	} {
		if argv, applied := rtkRewrite(RTKAuto, cmd); applied {
			t.Errorf("rtkRewrite(%q) was rewritten to %v; a multi-line command must never be rewritten", cmd, argv)
		}
	}
}

// TestRtkRewriteRefusesShellExpansion: the rewritten path execs argv with
// no shell, so anything the shell would have expanded arrives literally.
// `cat ~/.zshrc` reached rtk as a literal tilde and failed with ENOENT.
func TestRtkRewriteRefusesShellExpansion(t *testing.T) {
	// Without a working rtk on PATH every rewrite fails open, which
	// would make the negative assertions below pass for the wrong
	// reason.
	cleanup := fakeRTK(t)
	defer cleanup()

	for _, cmd := range []string{
		"cat ~/.zshrc",
		"ls $HOME",
		"ls *.go",
		"cat file?.txt",
		"git status # check the tree",
		"ls {a,b}",
	} {
		if argv, applied := rtkRewrite(RTKAuto, cmd); applied {
			t.Errorf("rtkRewrite(%q) was rewritten to %v; the no-shell path cannot expand it", cmd, argv)
		}
	}
}

// TestRtkRewriteEscapedQuoteDoesNotHidePipeline: inQuote ignored a
// backslash outside quotes, so the escaped " in `grep a\"b file | wc -l`
// was read as opening a quote and the following | looked quoted. The guard
// then let a pipeline through and rtk ran one grep with a garbage pattern.
func TestRtkRewriteEscapedQuoteDoesNotHidePipeline(t *testing.T) {
	// Without a working rtk on PATH every rewrite fails open, which
	// would make the negative assertions below pass for the wrong
	// reason.
	cleanup := fakeRTK(t)
	defer cleanup()

	cmd := `grep a\"b file | wc -l`
	if argv, applied := rtkRewrite(RTKAuto, cmd); applied {
		t.Fatalf("rtkRewrite(%q) was rewritten to %v; the pipeline must be detected", cmd, argv)
	}
}

// TestRtkRewriteStillHandlesQuotedMetacharacters guards the other
// direction: a metacharacter genuinely inside quotes is not a pipeline, and
// widening the refusal set must not make every ordinary command fall
// through — that would silently disable rtk rather than break it.
func TestRtkRewriteStillHandlesQuotedMetacharacters(t *testing.T) {
	// Without a working rtk on PATH every rewrite fails open, which
	// would make the negative assertions below pass for the wrong
	// reason.
	cleanup := fakeRTK(t)
	defer cleanup()

	for _, cmd := range []string{
		"git status",
		"docker ps",
		"find . -name '*.go'",
		"kubectl get pods",
	} {
		if _, applied := rtkRewrite(RTKAuto, cmd); !applied {
			t.Errorf("rtkRewrite(%q) was not applied; the guard is now too broad", cmd)
		}
	}
}
