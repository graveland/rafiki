// SPDX-License-Identifier: Apache-2.0

// Package rafikidconfig loads rafikid.toml, the daemon's single config file.
package rafikidconfig

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/telemetry"
)

// File is rafikid.toml.
type File struct {
	Telemetry telemetry.Config `toml:"telemetry"`
}

// Load reads path. A missing file is not an error: it yields the zero File.
// Any key the File does not declare is an error, so a typo fails startup.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, nil
		}
		return File{}, err
	}
	var f File
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return File{}, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	return f, nil
}
