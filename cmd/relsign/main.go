// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"go.graveland.dev/rafiki/pkg/relsign"
)

const usage = "usage: relsign keygen | sign <file> | verify <file> <sigfile> <tag>"

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "relsign: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:], stdout, stderr)
	case "sign":
		return runSign(args[1:], getenv)
	case "verify":
		return runVerify(args[1:], stdout)
	default:
		return errors.New(usage)
	}
}

func runKeygen(args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return errors.New(usage)
	}
	seed, pub, err := relsign.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "seed:   %s\n", base64.StdEncoding.EncodeToString(seed))
	fmt.Fprintf(stdout, "public: %s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Fprintf(stdout, "id:     %s\n", relsign.KeyID(pub))
	fmt.Fprintln(stderr, "store the seed as the RELSIGN_KEY secret; it cannot be recovered")
	return nil
}

func runSign(args []string, getenv func(string) string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	encoded := getenv("RELSIGN_KEY")
	if encoded == "" {
		return errors.New("RELSIGN_KEY is not set")
	}
	seed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("RELSIGN_KEY is not valid base64: %w", err)
	}
	priv, err := relsign.PrivateFromSeed(seed)
	if err != nil {
		return err
	}
	contents, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	return os.WriteFile(args[0]+".sig", relsign.Sign(priv, contents), 0o644)
}

func runVerify(args []string, stdout io.Writer) error {
	if len(args) != 3 {
		return errors.New(usage)
	}
	message, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	sigFile, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	if err := relsign.Verify(relsign.TrustedKeys, message, sigFile, args[2]); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}
