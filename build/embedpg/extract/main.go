// Command extract unpacks an embedded PostgreSQL artifact with embedpg.ExtractTar,
// the extractor rafikid itself uses, so a tarball that the build scripts produce
// is proven to satisfy it: no hardlinks, no escaping symlinks, nothing but files,
// directories and links.
//
//	go run ./build/embedpg/extract postgres-darwin-universal.txz <dir>
package main

import (
	"fmt"
	"os"

	"github.com/ulikunitz/xz"

	"go.graveland.dev/rafiki/pkg/embedpg"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: extract <artifact.txz> <dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "extract:", err)
		os.Exit(1)
	}
}

func run(artifact, dir string) error {
	f, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", artifact, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return embedpg.ExtractTar(xr, dir)
}
