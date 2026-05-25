package persist

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Mode controls when log dumps are written for a child.
type Mode string

const (
	// ModeOnExit writes logs on every child exit.
	ModeOnExit Mode = "on_exit"
	// ModeOnFailure writes logs only when a child exits badly — non-zero
	// exit code, signal termination, or last_status == "error".
	ModeOnFailure Mode = "on_failure"
	// ModeNever never writes any logs.
	ModeNever Mode = "never"
)

// ExitInfo carries the outcome of a child process.
type ExitInfo struct {
	ExitCode   int
	Signal     string
	LastStatus string
}

// Meta holds spawn arguments and timing for a child, written to meta.json.
// Field names follow the protocol spec (§11.3).
type Meta struct {
	ChildID     string   `json:"childId"`
	Name        string   `json:"name,omitempty"`
	Cwd         string   `json:"cwd"`
	Model       string   `json:"model,omitempty"`
	SessionFile string   `json:"sessionFile,omitempty"`
	SpawnedAt   int64    `json:"spawnedAt"`
	ExitedAt    int64    `json:"exitedAt"`
	ExitCode    int      `json:"exitCode"`
	ExitSignal  string   `json:"exitSignal,omitempty"`
	Argv        []string `json:"argv,omitempty"`
}

// LogDumper writes gzip-compressed JSONL dumps for child stdin/stdout/stderr
// and a plain meta.json on child exit, gated by mode.
type LogDumper struct {
	dir  string
	mode Mode
}

// NewLogDumper returns a LogDumper that writes under dir using the given mode.
// An empty mode defaults to ModeOnExit.
func NewLogDumper(dir string, mode Mode) *LogDumper {
	if mode == "" {
		mode = ModeOnExit
	}
	return &LogDumper{dir: dir, mode: mode}
}

// Dump writes in.jsonl.gz, out.jsonl.gz, err.log.gz, and meta.json for
// childID under the configured directory, subject to the emission mode.
//
// Layout: <dir>/<childID>/{in.jsonl.gz, out.jsonl.gz, err.log.gz, meta.json}
// All stream files are 0o600; the directory is 0o700.
func (d *LogDumper) Dump(
	childID string,
	in [][]byte,
	out [][]byte,
	errBytes []byte,
	meta Meta,
	exit ExitInfo,
) error {
	if d.mode == ModeNever {
		return nil
	}
	if d.mode == ModeOnFailure &&
		exit.ExitCode == 0 &&
		exit.Signal == "" &&
		exit.LastStatus != "error" {
		return nil
	}

	childDir := filepath.Join(d.dir, childID)
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		return err
	}

	// Write meta.json first so the directory is always identifiable even if
	// a stream write fails partway through.
	if err := writeMeta(filepath.Join(childDir, "meta.json"), meta); err != nil {
		return err
	}
	if err := writeGzLines(filepath.Join(childDir, "in.jsonl.gz"), in); err != nil {
		return err
	}
	if err := writeGzLines(filepath.Join(childDir, "out.jsonl.gz"), out); err != nil {
		return err
	}
	return writeGzBytes(filepath.Join(childDir, "err.log.gz"), errBytes)
}

// writeGzLines writes each line to a gzip-compressed file, appending \n after
// each entry. The file is created at 0o600.
func writeGzLines(path string, lines [][]byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// gzip.NewWriter never returns an error for valid built-in levels.
	gz := gzip.NewWriter(f) // uses DefaultCompression
	for _, line := range lines {
		if _, err := gz.Write(line); err != nil {
			gz.Close()
			return err
		}
		if _, err := gz.Write([]byte{'\n'}); err != nil {
			gz.Close()
			return err
		}
	}
	return gz.Close()
}

// writeGzBytes writes raw bytes to a gzip-compressed file at 0o600.
func writeGzBytes(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// gzip.NewWriter never returns an error for valid built-in levels.
	gz := gzip.NewWriter(f) // uses DefaultCompression
	if len(b) > 0 {
		if _, err := gz.Write(b); err != nil {
			gz.Close()
			return err
		}
	}
	return gz.Close()
}

// writeMeta writes meta as indented JSON to path at 0o600. Plain (not gzip)
// so humans can grep it directly.
func writeMeta(path string, meta Meta) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return errors.Join(enc.Encode(meta), f.Sync())
}
