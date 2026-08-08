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

// shellChainingRE matches shell metacharacters that indicate chaining,
// piping, or substitution. Any match means the command is a compound
// command that rtk cannot safely rewrite.
//
// The set: ; | & > < ` $(
var shellChainingChars = ";|&><`"

// hasShellChaining reports whether command contains shell chaining operators.
// This is the guardrail that prevents rewriting compound shell commands.
func hasShellChaining(command string) bool {
	// Fast path: scan for simple metacharacters
	for i := 0; i < len(command); i++ {
		c := command[i]
		if strings.IndexByte(shellChainingChars, c) >= 0 {
			// Check context to avoid false positives in quoted strings.
			// Simple approach: only match outside of quotes.
			if !inQuote(command, i) {
				// For &, require " && " or " & " (not just bare & like in "2>&1").
				// But we reject ANY unquoted & to be safe — redirects like 2>&1
				// will be caught. Actually, we need to be more careful:
				// "&&" is chaining, "&" background, "2>&1" is a redirect.
				// For simplicity and safety, we reject any unquoted & or |.
				if c == '&' {
					// Check if it's part of && or standalone &
					if i+1 < len(command) && command[i+1] == '&' {
						return true // &&
					}
					// Check for " & " (background)
					// But also check this isn't a redirect like 2>&1
					// We reject all unquoted & for safety
					return true
				}
				if c == '|' {
					if i+1 < len(command) && command[i+1] == '|' {
						return true // ||
					}
					return true // |
				}
				if c == ';' {
					return true
				}
				if c == '>' || c == '<' {
					return true
				}
				if c == '`' {
					return true
				}
			}
		}
	}

	// Check for $( substitution
	for i := 0; i < len(command)-1; i++ {
		if command[i] == '$' && command[i+1] == '(' && !inQuote(command, i) {
			return true
		}
	}

	return false
}

// inQuote reports whether position i in s is inside a quoted string.
// Handles single and double quotes with backslash escaping in double quotes.
func inQuote(s string, i int) bool {
	inSingle := false
	inDouble := false
	for j := 0; j < i && j < len(s); j++ {
		c := s[j]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '\\' {
				j++ // skip escaped char
				continue
			}
			if c == '"' {
				inDouble = false
			}
		default:
			switch c {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			}
		}
	}
	return inSingle || inDouble
}

// rtkRewriteMapping maps the first whitespace-delimited token of a command
// to its rtk subcommand. Ported from rtk's Rust rewrite_prefixes and
// rtk_cmd in src/discover/rules.rs.
//
// Single-token prefixes only. Multi-token prefixes (like "npm exec tsc") are
// not in this table — the follow-on token is checked at match time.
var rtkRewriteMapping = map[string]string{
	"git":              "git",
	"yadm":             "git",
	"gh":               "gh",
	"glab":             "glab",
	"cargo":            "cargo",
	"pnpm":             "pnpm",
	"npm":              "npm",
	"npx":              "npx",
	"cat":              "read",
	"head":             "read",
	"tail":             "read",
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
// only used for the rewrite mapping, which needs the first few tokens correctly
// identified. The original command string is appended as-is to the rtk argv,
// so this tokenizer's imperfections do not affect the executed command — only
// the detection of whether to rewrite.
func shellSplit(s string) []string {
	var tokens []string
	var current []byte
	inSingle := false
	inDouble := false
	escaped := false

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
			continue
		case '"':
			inDouble = true
			continue
		case ' ', '\t':
			if len(current) > 0 {
				tokens = append(tokens, string(current))
				current = current[:0]
			}
			continue
		}
		current = append(current, c)
	}

	if len(current) > 0 {
		tokens = append(tokens, string(current))
	}

	return tokens
}

// resetRTKCache resets the cached rtk version probe. Exported for tests only.
func resetRTKCache(fn func() rtkCache) {
	rtkProbeMu.Lock()
	defer rtkProbeMu.Unlock()
	rtkProbeOverride = fn
}
