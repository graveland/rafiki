package integration_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// The binaries these tests exec are built in a subprocess, so the go command
// cannot see their sources: the test binary's own import graph is little more
// than pkg/protocol, and a cached PASS would survive any change to the daemon.
// go test does, however, fold every file a test opens (mtime and size) and
// every directory it opens (its listing) into the cache key. Opening the
// subprocess's inputs from inside a test therefore makes the cached result
// exactly as fresh as the binaries — which is what lets `make test` drop
// -count=1.
//
// The opens must happen while tests run: the test log that records them is
// installed by m.Run, so anything opened in TestMain beforehand is invisible.
// That is why the binaries are reached through daemonBinary/cliBinary rather
// than the bare path variables.

// buildInputs is every file and directory the built binaries and their
// subprocess fixtures depend on, computed once in TestMain.
var (
	buildInputs     []string
	trackInputsOnce sync.Once
)

// daemonBinary returns the rafikid binary built by TestMain.
func daemonBinary() string {
	trackInputsOnce.Do(openBuildInputs)
	return binaryPath
}

// cliBinary returns the rafiki binary built by TestMain.
func cliBinary() string {
	trackInputsOnce.Do(openBuildInputs)
	return cliPath
}

func openBuildInputs() {
	for _, p := range buildInputs {
		if f, err := os.Open(p); err == nil {
			f.Close()
		}
	}
}

// listBuildInputs names the main-module sources behind pkgs (Go and embedded
// files plus each package directory, so an added or removed file counts too),
// go.mod/go.sum, and the non-Go fixtures the tests hand to subprocesses: the
// fake child scripts beside this file and the Python SDK tree.
func listBuildInputs(root string, pkgs ...string) ([]string, error) {
	const tmpl = `{{if and .Module .Module.Main}}{{$d := .Dir}}{{$d}}
{{range .GoFiles}}{{$d}}/{{.}}
{{end}}{{range .CgoFiles}}{{$d}}/{{.}}
{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}
{{end}}{{end}}`
	list := exec.Command("go", append([]string{"list", "-deps", "-f", tmpl}, pkgs...)...)
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	inputs := strings.Fields(string(out))
	inputs = append(inputs, filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum"))

	scripts, err := filepath.Glob(filepath.Join(root, "test", "integration", "*.sh"))
	if err != nil {
		return nil, err
	}
	inputs = append(inputs, scripts...)

	sdk := filepath.Join(root, "sdk", "python")
	err = filepath.WalkDir(sdk, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || d.Name() == "__pycache__") && p != sdk {
			return filepath.SkipDir
		}
		inputs = append(inputs, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", sdk, err)
	}
	return inputs, nil
}
