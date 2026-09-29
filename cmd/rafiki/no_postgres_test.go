// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// The two-binary split is the whole point of this architecture and the easiest
// invariant for a future change to violate silently: Go imports whole packages,
// so one import of a package that happens to contain a pgxpool field is enough.
//
// These are linker-level assertions, which are the only kind that cannot be
// satisfied by a convention nobody re-reads. They shell out to `go list` rather
// than importing anything, because a test that imported the offending package
// would itself be the violation.
func TestClientDoesNotLinkPostgres(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/cmd/rafiki").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
			t.Errorf("cmd/rafiki links %s. rafiki is a socket client and must never open a database; "+
				"find the path with: go mod why -m github.com/jackc/pgx/v5", dep)
		}
	}
}

// TestClientDoesNotLinkOIDC pins the other half of the split: the Login flow
// is driven by the daemon, so the client links no OIDC library and never
// imports the daemon's login engine (pkg/oidclogin) — a client that linked
// them would start talking to IdPs itself, with the client's credentials.
//
// golang.org/x/oauth2 cannot be banned from the dependency closure flat out:
// the MCP SDK (modelcontextprotocol/go-sdk) legitimately links it for its own
// OAuth support, so it rides into every client build transitively — it was an
// indirect dependency long before OIDC login existed. The enforceable shape of
// the invariant is that no FIRST-PARTY rafiki package in the client's tree
// imports it (or go-oidc): oauth2 may reach the client only through
// third-party code.
func TestClientDoesNotLinkOIDC(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-json", "go.graveland.dev/rafiki/cmd/rafiki").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath string
			Imports    []string
		}
		if err := dec.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("parse go list -json output: %v", err)
		}
		if strings.Contains(pkg.ImportPath, "coreos/go-oidc") {
			t.Errorf("cmd/rafiki links %s. The client drives OIDC login over the Connect "+
				"Login service and links no OIDC library; find the path with: go mod why -m github.com/coreos/go-oidc/v3",
				pkg.ImportPath)
		}
		if strings.Contains(pkg.ImportPath, "/pkg/oidclogin") {
			t.Errorf("cmd/rafiki links %s. pkg/oidclogin is the daemon's login engine and is "+
				"daemon-only; the client talks to it over the Connect Login service",
				pkg.ImportPath)
		}
		if !strings.HasPrefix(pkg.ImportPath, "go.graveland.dev/rafiki/") {
			continue
		}
		for _, imp := range pkg.Imports {
			if strings.Contains(imp, "golang.org/x/oauth2") || strings.Contains(imp, "coreos/go-oidc") {
				t.Errorf("first-party %s imports %s in the client's tree. OAuth/OIDC libraries are "+
					"daemon-only (pkg/oidclogin); the client links oauth2 only transitively via third-party deps",
					pkg.ImportPath, imp)
			}
		}
	}
}
