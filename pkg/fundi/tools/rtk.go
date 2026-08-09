package tools

import (
	"os/exec"
	"strings"
	"sync"
)

// rtkVersionProbe caches the result of `rtk --version` so every bash call does
// not spawn an extra process. The sync.OnceValue is overridable by tests via
// resetRTKCache.
type rtkCache struct {
	path string // empty means not found
	ok   bool   // true when version >= 0.23.0
}

var rtkProbe = sync.OnceValue(func() rtkCache {
	p, err := exec.LookPath("rtk")
	if err != nil {
		return rtkCache{}
	}
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		return rtkCache{}
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(out)), "rtk ")
	version, _, _ = strings.Cut(version, " ")
	return rtkCache{path: p, ok: semverGTE(version, 0, 23, 0)}
})

var rtkProbeMu sync.Mutex
var rtkProbeOverride func() rtkCache // set by resetRTKCache in tests

// rtkIsAvailable reports whether rtk is installed and its version is >= 0.23.0.
// The result is cached after the first call (sync.OnceValue).
func rtkIsAvailable() bool {
	rtkProbeMu.Lock()
	override := rtkProbeOverride
	rtkProbeMu.Unlock()
	if override != nil {
		c := override()
		return c.ok
	}
	return rtkProbe().ok
}

// semverGTE parses a "MAJOR.MINOR.PATCH" string and returns true if it is
// >= the given version components. Malformed input returns false (fail open —
// an unparseable version is treated as too old).
func semverGTE(v string, maj, min, pat int) bool {
	if v == "" {
		return false
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	var n [3]int
	for i, s := range parts {
		n[i] = int(atoi(s))
	}
	if n[0] != maj {
		return n[0] > maj
	}
	if n[1] != min {
		return n[1] > min
	}
	return n[2] >= pat
}

// atoi converts a string to an int, returning 0 on failure. This is only used
// for version component parsing where a malformed component should fail the
// comparison (0 < any real version component).
func atoi(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// shellUnsafeChars are the characters that make a command unsafe to rewrite
// into an rtk argv when they appear OUTSIDE any quoting (or inside single
// quotes' cousin, double quotes, for the chaining/redirection subset —
// see doubleQuoteUnsafeChars for the rest of that story). Two distinct
// hazards, both fatal, both fail open:
//
//   - Chaining and redirection (; | & > < ` and newline) mean the string is
//     more than one command. Rewriting the first token then changes what
//     runs: "git add -A\ngit commit -m wip" became a single git invocation
//     with a pathspec of "-A\ngit", so the add failed and the commit
//     silently never happened — while the model saw one command's output
//     and no sign that a second command had ever existed.
//   - Expansion ($ ~ * ? [ { # !) is the shell's job, and the rewritten
//     path execs argv directly with no shell. "cat ~/.zshrc" reached rtk as
//     a literal tilde and failed with ENOENT; "ls *.go" as an unexpanded
//     glob; "git status # note" with the comment as trailing arguments.
//
// Refusing too much costs nothing: every refusal is just plain bash, which
// is the behaviour without rtk at all.
const shellUnsafeChars = ";|&><`\n\r$~*?[{#!"

// doubleQuoteUnsafeChars are the characters that stay live inside a
// DOUBLE-quoted string, unlike everything in shellUnsafeChars. Single
// quotes make every character genuinely inert — bash does no further
// interpretation of anything between them, which is why hasShellChaining
// can skip a single-quoted span outright. Double quotes are not that:
// bash still performs parameter expansion, command substitution, and
// backslash escaping inside them. Treating "…" the same as '…' let
//
//	git commit -m "release $VERSION"
//	git commit -m "built at `date`"
//
// rewrite to an argv containing the literal, unexpanded text — no error,
// no sign anything should have been substituted, just a commit with the
// wrong message. `$` and the backtick are the two expansion triggers bash
// honours inside double quotes; backslash is included too, even though it
// is not a hazard by itself, because it can escape INTO one (`\$(cmd)`
// etc.) and untangling "this backslash escapes something dangerous" from
// "this one doesn't" is exactly the kind of nuance this file's design note
// says to skip — refusing the whole command is free. The pure
// chaining/redirection characters (; | & > < and newline) are NOT in this
// set: bash does not treat them specially inside double quotes, so they
// stay inert there. `!` (history expansion) is absent everywhere — it is
// inactive in non-interactive bash, the only way this argv ever runs.
const doubleQuoteUnsafeChars = "$`\\"

// hasShellChaining reports whether command must not be rewritten. Despite
// the name, the set it checks covers expansion as well as chaining — see
// shellUnsafeChars. It tracks quote state itself in a single pass (rather
// than calling a per-position "am I inside a quote" helper) because the
// answer to "is this character safe" now depends on WHICH kind of quote:
// single quotes make shellUnsafeChars fully inert, double quotes only
// silence the chaining/redirection subset while leaving
// doubleQuoteUnsafeChars live.
func hasShellChaining(command string) bool {
	inSingle := false
	inDouble := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if strings.IndexByte(doubleQuoteUnsafeChars, c) >= 0 {
				return true
			}
			if c == '"' {
				inDouble = false
			}
		default:
			switch c {
			case '\\':
				// A backslash escapes the next character outside quotes
				// too. Without this, `grep a\"b file | wc -l` was read as
				// opening a quote at the escaped ", so the following | was
				// judged to be inside a quote and the guard let a pipeline
				// through — the exact false negative it exists to prevent.
				i++
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			default:
				if strings.IndexByte(shellUnsafeChars, c) >= 0 {
					return true
				}
			}
		}
	}
	return false
}

// rtkRewriteMapping maps the first whitespace-delimited token of a command
// to its rtk subcommand. Ported from rtk's Rust rewrite_prefixes and
// rtk_cmd in src/discover/rules.rs.
//
// Single-token prefixes only. Multi-token prefixes (like "npm exec tsc") are
// not in this table — the follow-on token is checked at match time.
var rtkRewriteMapping = map[string]string{
	"git":   "git",
	"yadm":  "git",
	"gh":    "gh",
	"glab":  "glab",
	"cargo": "cargo",
	"pnpm":  "pnpm",
	"npm":   "npm",
	"npx":   "npx",
	"cat":   "read",
	// head and tail are deliberately absent. They were mapped to "read"
	// here, but upstream's rtk-rewrite.sh does not map them and the real
	// binary refuses "tail -f" outright: `rtk rewrite "head -20 README.md"`
	// emits `rtk read README.md --max-lines 20`, translating the count
	// flag, which a bare prefix swap cannot do. The unswapped form ran as
	// `rtk read -20 README.md` and hit bash's builtin read:
	// "read: -2: invalid option", exit 2 — so `head -50 server.log` broke
	// instead of being compressed. Falling through to plain bash is the
	// correct behaviour until the flag translation is ported.
	"grep":             "grep",
	"rg":               "rg",
	"ls":               "ls",
	"find":             "find",
	"docker":           "docker",
	"kubectl":          "kubectl",
	"oc":               "oc",
	"tree":             "tree",
	"diff":             "diff",
	"curl":             "curl",
	"wget":             "wget",
	"go":               "go",
	"make":             "make",
	"aws":              "aws",
	"psql":             "psql",
	"ruff":             "ruff",
	"tsc":              "tsc",
	"biome":            "lint",
	"eslint":           "lint",
	"lint":             "lint",
	"prettier":         "prettier",
	"jest":             "jest",
	"vitest":           "vitest",
	"playwright":       "playwright",
	"prisma":           "prisma",
	"pytest":           "pytest",
	"pip3":             "pip",
	"pip":              "pip",
	"mypy":             "mypy",
	"bundle":           "bundle",
	"rspec":            "rspec",
	"rubocop":          "rubocop",
	"ansible-playbook": "ansible-playbook",
	"brew":             "brew",
	"composer":         "composer",
	"df":               "df",
	"dotnet":           "dotnet",
	"du":               "du",
	"fail2ban-client":  "fail2ban-client",
	"gcloud":           "gcloud",
	"gradle":           "gradlew",
	"gradlew":          "gradlew",
	"hadolint":         "hadolint",
	"helm":             "helm",
	"iptables":         "iptables",
	"markdownlint":     "markdownlint",
	"mix":              "mix",
	"mvn":              "mvn",
	"mvnw":             "mvn",
	"ping":             "ping",
	"pio":              "pio",
	"poetry":           "poetry",
	"pre-commit":       "pre-commit",
	"ps":               "ps",
	"pulumi":           "pulumi",
	"quarto":           "quarto",
	"rsync":            "rsync",
	"shellcheck":       "shellcheck",
	"shopify":          "shopify",
	"sops":             "sops",
	"swift":            "swift",
	"systemctl":        "systemctl",
	"terraform":        "terraform",
	"tofu":             "tofu",
	"trunk":            "trunk",
	"uv":               "uv",
	"yamllint":         "yamllint",
	"wc":               "wc",
	"gt":               "gt",
	"liquibase":        "liquibase",
}

// npmRewriteTokens lists npm subcommands that rtk maps. An `npm <sub>` command
// is only rewritten when <sub> is in this set.
var npmRewriteTokens = map[string]bool{
	"exec":       true,
	"run":        true,
	"run-script": true,
	"rum":        true,
	"urn":        true,
	"x":          true,
}

// rtkRewrite returns the argv to execute for command, and whether rtk was
// applied. mode==RTKOff, a missing rtk, or an unmapped command all return
// (nil, false), meaning "run bash -c command unchanged".
func rtkRewrite(mode RTKMode, command string) (argv []string, applied bool) {
	if mode == RTKOff {
		return nil, false
	}

	if hasShellChaining(command) {
		return nil, false
	}

	// Tokenize the command into words.
	tokens := shellSplit(command)
	if len(tokens) == 0 {
		return nil, false
	}

	first := tokens[0]

	// npm requires subcommand matching
	if first == "npm" {
		if len(tokens) < 2 {
			return nil, false
		}
		if !npmRewriteTokens[tokens[1]] {
			return nil, false
		}
	}

	rtkSub, ok := rtkRewriteMapping[first]
	if !ok {
		return nil, false
	}

	// Check availability and version
	if !rtkIsAvailable() {
		return nil, false
	}

	// Build argv: ["rtk", subcommand, ...original args after the first token...]
	argv = make([]string, 0, 2+len(tokens))
	argv = append(argv, "rtk", rtkSub)
	argv = append(argv, tokens[1:]...)
	return argv, true
}

// shellSplit splits a command string into tokens, handling single and double
// quotes and backslash escaping. This is a simplified shell tokenizer; it is
// only used for the rewrite mapping, which needs the first few tokens
// correctly identified. That output is NOT cosmetic: rtkRewrite appends
// tokens[1:] straight onto the rtk argv that gets exec'd, so a bug here
// reaches the command that actually runs, not just the yes/no decision of
// whether to rewrite.
//
// current is only flushed into a token when it is non-empty OR quoted was
// set — a bare `len(current) > 0` check drops an explicitly quoted empty
// argument entirely. `git commit -m ""` used to tokenize to ["git" "commit"
// "-m"], three tokens instead of four; rewritten, that became `rtk git
// commit -m`, which swallows whatever argument follows (or errors) instead
// of committing an empty message.
func shellSplit(s string) []string {
	var tokens []string
	var current []byte
	quoted := false
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		if len(current) > 0 || quoted {
			tokens = append(tokens, string(current))
		}
		current = current[:0]
		quoted = false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			current = append(current, c)
			escaped = false
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			} else {
				current = append(current, c)
			}
			continue
		}
		if inDouble {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inDouble = false
			default:
				current = append(current, c)
			}
			continue
		}
		switch c {
		case '\'':
			inSingle = true
			quoted = true
			continue
		case '"':
			inDouble = true
			quoted = true
			continue
		case ' ', '\t':
			flush()
			continue
		}
		current = append(current, c)
	}

	flush()

	return tokens
}

// resetRTKCache resets the cached rtk version probe. Exported for tests only.
func resetRTKCache(fn func() rtkCache) {
	rtkProbeMu.Lock()
	defer rtkProbeMu.Unlock()
	rtkProbeOverride = fn
}
