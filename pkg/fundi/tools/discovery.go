package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

// rgPath resolves ripgrep once. ripgrep is a required runtime dependency
// (see BuildRuntime, which fails at startup when it is missing); the
// empty string here means "not installed" and is only reachable in tests.
var rgPath = sync.OnceValue(func() string {
	p, err := exec.LookPath("rg")
	if err != nil {
		return ""
	}
	return p
})

// RipgrepAvailable reports whether ripgrep is installed. BuildRuntime
// calls this once at startup so a missing dependency fails loudly there
// rather than once per tool call.
func RipgrepAvailable() bool { return rgPath() != "" }

// FileQuery locates files by path pattern, scoped to Root.
type FileQuery struct {
	Root string
	Glob string
	// Exclude drops matching paths. It is applied here rather than by the
	// caller because Limit counts *kept* files: a caller that filters the
	// returned slice can only ever remove entries, never reach the ones
	// that the limit already excluded. ls hit exactly that — in a
	// directory of 1100 logs plus one .go file, all 1001 discovered paths
	// were logs, so `ignore: ["*.log"]` yielded an empty listing instead
	// of the one file the user was looking for.
	Exclude []string
	Limit   int
}

// ContentQuery searches file contents under Root.
type ContentQuery struct {
	Root    string
	Pattern string
	Glob    string
	Limit   int
}

// Match is one content-search hit.
type Match struct {
	Path string
	Line int
	Text string
}

// rgError turns a ripgrep failure into a useful message. rg exits 1 when
// it simply found nothing, which is not an error.
func rgError(err error, stderr string) error {
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return nil
	}
	if stderr != "" {
		return fmt.Errorf("ripgrep: %s", strings.TrimSpace(stderr))
	}
	return fmt.Errorf("ripgrep: %w", err)
}

// baseArgs are shared by every invocation. --no-require-git makes rg
// honour .gitignore even when Root is not itself inside a git working
// tree (e.g. a plain checkout with the .git directory stripped, or a
// test fixture) — by default ripgrep only applies gitignore rules when
// it detects a .git directory, which is not the property this package
// exists to provide.
//
// --hidden is required, not optional: without it rg skips every dotfile,
// so .env.example, .github/, and .gitignore itself are invisible to
// glob/grep/ls. A tool that silently cannot see a file the user can see
// answers "no matches" for things that exist, which is worse than being
// slow. --hidden alone would then walk .git/objects, so it is paired
// with a *negative* glob.
//
// The "--glob overrides gitignore" hazard (ripgrep docs: "This always
// overrides any other ignore logic") applies to *inclusion* globs, which
// can re-admit an ignored path. A negation glob can only ever subtract,
// so `!.git/` is safe here — verified: with a .gitignore of "ignored/",
// `--hidden -g '!.git/'` still excludes ignored/. Positive glob filtering
// is nonetheless applied in Go (see globMatch), never via --glob.
//
// Never add -L: following symlinks lets rg escape Root into module
// caches or the nix store, chase cycles, and pin every core. Never add
// --no-ignore: it defeats the entire purpose of this helper.
var baseArgs = []string{"--no-require-git", "--hidden", "--glob", "!.git/"}

// globMatch reports whether path (relative to root) matches pattern.
// A pattern without a path separator (e.g. "*.go") matches the basename
// at any depth, mirroring ripgrep's -g behaviour.
func globMatch(pattern, rel string) (bool, error) {
	ok, err := doublestar.Match(pattern, rel)
	if err != nil {
		return false, err
	}
	if !ok && !strings.ContainsRune(pattern, '/') {
		return doublestar.Match(pattern, filepath.Base(rel))
	}
	return ok, nil
}

// excludedByAny reports whether rel matches any of the patterns, using the
// same semantics as an include glob (a pattern without a separator matches
// the basename at any depth). A malformed pattern is treated as
// non-matching rather than failing the whole listing.
func excludedByAny(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		if ok, err := globMatch(pattern, rel); err == nil && ok {
			return true
		}
	}
	return false
}

// DiscoverFiles lists files under q.Root, honouring .gitignore. It never
// follows symlinks: -L lets rg escape the root into module caches or the
// nix store, chase cycles, and pin every core. Glob filtering is applied
// in Go rather than via ripgrep's --glob, because --glob overrides
// .gitignore (documented ripgrep behaviour).
func DiscoverFiles(ctx context.Context, q FileQuery) ([]string, bool, error) {
	if rgPath() == "" {
		return nil, false, fmt.Errorf("ripgrep (rg) not found on PATH")
	}
	args := append([]string{}, baseArgs...)
	args = append(args, "--files", "--null")
	// --glob is deliberately not used: it overrides .gitignore.
	args = append(args, "--", q.Root)

	cmd := exec.CommandContext(ctx, rgPath(), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	// Stream and stop at Limit rather than buffering the whole tree and
	// slicing (design §0.1 forbids the latter): cmd.Output() on a 400k-file
	// monorepo materialises every path and a 400k-element slice to return
	// the 101 that glob asked for, and blocks until the full walk finishes.
	var paths []string
	truncated := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxPathScanBytes)
	scanner.Split(scanNullTerminated)
	for scanner.Scan() {
		p := scanner.Text()
		if p == "" {
			continue
		}
		if q.Glob != "" || len(q.Exclude) > 0 {
			rel, relErr := filepath.Rel(q.Root, p)
			if relErr != nil {
				continue
			}
			if q.Glob != "" {
				ok, mErr := globMatch(q.Glob, rel)
				if mErr != nil || !ok {
					continue
				}
			}
			if excludedByAny(q.Exclude, rel) {
				continue
			}
		}
		if q.Limit > 0 && len(paths) == q.Limit {
			truncated = true
			break
		}
		paths = append(paths, p)
	}
	scanErr := scanner.Err()

	// See SearchContent for why the early-close errors are swallowed when
	// we are the reason the stream stopped.
	closeErr := stdout.Close()
	waitErr := cmd.Wait()
	if truncated {
		return paths, truncated, nil
	}
	if scanErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, fmt.Errorf("ripgrep: reading file list: %w", scanErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("ripgrep: closing stdout: %w", closeErr)
	}
	if waitErr != nil {
		if rerr := rgError(waitErr, stderr.String()); rerr != nil {
			return nil, false, rerr
		}
	}
	return paths, truncated, nil
}

// maxPathScanBytes bounds a single NUL-delimited path. PATH_MAX is 4096 on
// Linux and 1024 on Darwin; 64 KiB is slack enough that hitting it means
// the stream is corrupt, not that someone has a deep directory.
const maxPathScanBytes = 64 * 1024

// scanNullTerminated is a bufio.SplitFunc for rg --null output.
func scanNullTerminated(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// clampMatchText bounds one search hit's line text. rg embeds the entire
// matched line in the event, so an unclamped match against a minified
// bundle drops megabytes into the model's context for one grep call.
func clampMatchText(s string) string { return truncateLine(s, maxLineChars) }

// rgText is ripgrep's "arbitrary data" JSON encoding: valid UTF-8 is sent
// as {"text": "..."}, anything else as {"bytes": "<base64>"}. Decoding
// only Text silently yields "" for a Latin-1 source file or a path with a
// stray byte — an empty match that still consumes a slot against Limit.
type rgText struct {
	Text  string `json:"text"`
	Bytes []byte `json:"bytes"`
}

// String prefers the UTF-8 form and falls back to the raw bytes.
func (t rgText) String() string {
	if t.Text != "" {
		return t.Text
	}
	return string(t.Bytes)
}

// rgEvent is the subset of ripgrep's --json event stream we consume.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
	} `json:"data"`
}

// SearchContent runs a content search under q.Root, honouring .gitignore.
// It uses --json because plain output is ambiguous on paths containing a
// colon. Glob filtering is applied in Go rather than via ripgrep's --glob,
// because --glob overrides .gitignore (documented ripgrep behaviour).
func SearchContent(ctx context.Context, q ContentQuery) ([]Match, bool, error) {
	if rgPath() == "" {
		return nil, false, fmt.Errorf("ripgrep (rg) not found on PATH")
	}
	args := append([]string{}, baseArgs...)
	args = append(args, "--json", "-H", "-n")
	// --glob is deliberately not used: it overrides .gitignore.
	// -e and -- are load-bearing: without them a pattern that starts with
	// a dash ("--force", "-race", "-v") is parsed by rg as a flag and the
	// search dies with "unrecognized flag" instead of matching.
	args = append(args, "-e", q.Pattern, "--", q.Root)

	cmd := exec.CommandContext(ctx, rgPath(), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	// Stat once, outside the loop: whether Root is a file decides how the
	// glob filter behaves for every match.
	rootIsFile := false
	if info, statErr := os.Stat(q.Root); statErr == nil && !info.IsDir() {
		rootIsFile = true
	}

	var matches []Match
	truncated := false
	// A json.Decoder, not a bufio.Scanner: rg embeds the whole matched line
	// in each match event, so a single >10MB line (a minified bundle, a
	// one-line JSON fixture) overflows Scanner's token limit. Scanner then
	// stops with ErrTooLong, and because Err() is cheap to forget, the loop
	// used to exit as if the stream had ended cleanly — returning zero
	// matches, truncated=false, and a nil error for the entire search.
	// Decoder has no per-token cap, so the cliff is gone; the per-match
	// text clamp below keeps a huge line out of the model's context.
	dec := json.NewDecoder(stdout)
	var decErr error
	for {
		var ev rgEvent
		if err := dec.Decode(&ev); err != nil {
			if !errors.Is(err, io.EOF) {
				decErr = err
			}
			break
		}
		if ev.Type != "match" {
			continue
		}
		path := ev.Data.Path.String()
		// rootIsFile: when Root is a single file rather than a directory,
		// filepath.Rel returns "." and no glob can match it, so every hit
		// was discarded and grep answered "no matches" for a file that
		// plainly contained the pattern. Naming one file is already
		// narrower than any glob, so the filter has nothing left to do.
		if q.Glob != "" && !rootIsFile {
			rel, relErr := filepath.Rel(q.Root, path)
			if relErr != nil {
				continue
			}
			ok, mErr := globMatch(q.Glob, rel)
			if mErr != nil || !ok {
				continue
			}
		}
		if q.Limit > 0 && len(matches) == q.Limit {
			truncated = true
			break
		}
		matches = append(matches, Match{
			Path: path,
			Line: ev.Data.LineNumber,
			Text: clampMatchText(strings.TrimRight(ev.Data.Lines.String(), "\n")),
		})
	}
	// Stop rg as soon as the limit is hit rather than draining the stream.
	// Closing our end of the pipe makes the next write in the still-running
	// rg process fail (EPIPE); ripgrep exits on that rather than hanging,
	// and the exit/wait error it produces is expected here, so it is
	// swallowed below whenever we are the reason the stream stopped early.
	closeErr := stdout.Close()
	waitErr := cmd.Wait()
	if truncated {
		return matches, truncated, nil
	}
	// A decode failure means the event stream was cut short, so `matches`
	// is a silent partial. Report it rather than passing off a truncated
	// search as a complete one.
	if decErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, fmt.Errorf("ripgrep: decoding --json stream: %w", decErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("ripgrep: closing stdout: %w", closeErr)
	}
	if waitErr != nil {
		if rerr := rgError(waitErr, stderr.String()); rerr != nil {
			return nil, false, rerr
		}
	}
	return matches, truncated, nil
}
