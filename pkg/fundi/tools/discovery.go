package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
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
	Root  string
	Glob  string
	Limit int
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
// exists to provide. Never add -L (follow symlinks lets rg escape Root
// into module caches or the nix store, chase cycles, and pin every
// core) or --no-ignore (defeats the entire purpose of this helper).
var baseArgs = []string{"--no-require-git"}

// DiscoverFiles lists files under q.Root, honouring .gitignore. It never
// follows symlinks: -L lets rg escape the root into module caches or the
// nix store, chase cycles, and pin every core.
func DiscoverFiles(ctx context.Context, q FileQuery) ([]string, bool, error) {
	if rgPath() == "" {
		return nil, false, fmt.Errorf("ripgrep (rg) not found on PATH")
	}
	args := append([]string{}, baseArgs...)
	args = append(args, "--files", "--null")
	if q.Glob != "" {
		args = append(args, "--glob", q.Glob)
	}
	args = append(args, q.Root)

	cmd := exec.CommandContext(ctx, rgPath(), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if rerr := rgError(err, stderr.String()); rerr != nil {
			return nil, false, rerr
		}
	}

	var paths []string
	truncated := false
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if q.Limit > 0 && len(paths) == q.Limit {
			truncated = true
			break
		}
		paths = append(paths, p)
	}
	return paths, truncated, nil
}

// rgEvent is the subset of ripgrep's --json event stream we consume.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       struct{ Text string } `json:"path"`
		Lines      struct{ Text string } `json:"lines"`
		LineNumber int                   `json:"line_number"`
	} `json:"data"`
}

// SearchContent runs a content search under q.Root, honouring .gitignore.
// It uses --json because plain output is ambiguous on paths containing a
// colon.
func SearchContent(ctx context.Context, q ContentQuery) ([]Match, bool, error) {
	if rgPath() == "" {
		return nil, false, fmt.Errorf("ripgrep (rg) not found on PATH")
	}
	args := append([]string{}, baseArgs...)
	args = append(args, "--json", "-H", "-n")
	if q.Glob != "" {
		args = append(args, "--glob", q.Glob)
	}
	args = append(args, q.Pattern, q.Root)

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

	var matches []Match
	truncated := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var ev rgEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil || ev.Type != "match" {
			continue
		}
		if q.Limit > 0 && len(matches) == q.Limit {
			truncated = true
			break
		}
		matches = append(matches, Match{
			Path: ev.Data.Path.Text,
			Line: ev.Data.LineNumber,
			Text: strings.TrimRight(ev.Data.Lines.Text, "\n"),
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
