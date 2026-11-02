// SPDX-License-Identifier: Apache-2.0

// Package rafikidconfig loads rafikid.toml, the daemon's single config file.
package rafikidconfig

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/telemetry"
)

// File is rafikid.toml.
type File struct {
	Telemetry telemetry.Config `toml:"telemetry"`
	MCPOAuth  mcpoauth.Config  `toml:"mcp_oauth"`
}

// Validate compiles the sections that can be checked without another file. A
// malformed entry — a wildcard in allow_hosts, a malformed CIDR, a URL where a
// host belongs — is refused here, which is what makes it a startup error rather
// than a permission that silently matches nothing.
func (f File) Validate() error {
	if _, err := f.MCPOAuth.Policy(); err != nil {
		return err
	}
	return nil
}

// Load reads path. A missing file is not an error: it yields the zero File.
// Any key the File does not declare is an error, so a typo fails startup, and
// the [mcp_oauth] section is validated through Validate.
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
	if err := f.Validate(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
