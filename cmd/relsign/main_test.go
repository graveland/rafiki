// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/relsign"
)

func mapGetenv(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestRunKeygen(t *testing.T) {
	c := assert.NewCollecting(t)
	var stdout, stderr bytes.Buffer
	c.Require().NoError(run([]string{"keygen"}, mapGetenv(nil), &stdout, &stderr), "run keygen")

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	c.Len(lines, 3, "keygen stdout line count")
	c.True(strings.HasPrefix(lines[0], "seed:   "), "first line = %q", lines[0])
	c.True(strings.HasPrefix(lines[1], "public: "), "second line = %q", lines[1])
	c.True(strings.HasPrefix(lines[2], "id:     "), "third line = %q", lines[2])

	seed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lines[0], "seed:   "))
	c.Require().NoError(err, "decode seed")
	c.Len(seed, ed25519.SeedSize, "seed length")
	c.NotEmpty(stderr.String(), "keygen stderr")
}

func TestRunSignWritesVerifiableSig(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, _, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	priv, err := relsign.PrivateFromSeed(seed)
	c.Require().NoError(err, "PrivateFromSeed()")

	file := filepath.Join(t.TempDir(), "checksums.txt")
	c.Require().NoError(os.WriteFile(file, []byte("contents\n"), 0o644), "write file")

	env := mapGetenv(map[string]string{"RELSIGN_KEY": base64.StdEncoding.EncodeToString(seed)})
	var stdout, stderr bytes.Buffer
	c.Require().NoError(run([]string{"sign", file}, env, &stdout, &stderr), "run sign")
	c.Empty(stdout.String(), "sign stdout")

	sigFile, err := os.ReadFile(file + ".sig")
	c.Require().NoError(err, "read sig")

	pub := priv.Public().(ed25519.PublicKey)
	keys := []relsign.Key{{ID: relsign.KeyID(pub), PublicKey: pub}}
	c.NoError(relsign.Verify(keys, []byte("contents\n"), sigFile, "v0.2.0"), "Verify()")
}

func TestRunSignWithoutKey(t *testing.T) {
	c := assert.NewCollecting(t)
	file := filepath.Join(t.TempDir(), "checksums.txt")
	c.Require().NoError(os.WriteFile(file, []byte("contents\n"), 0o644), "write file")

	var stdout, stderr bytes.Buffer
	err := run([]string{"sign", file}, mapGetenv(nil), &stdout, &stderr)
	c.EqualError(err, "RELSIGN_KEY is not set", "run sign")
}

func TestRunSignMissingFile(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, _, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")

	file := filepath.Join(t.TempDir(), "absent.txt")
	env := mapGetenv(map[string]string{"RELSIGN_KEY": base64.StdEncoding.EncodeToString(seed)})
	var stdout, stderr bytes.Buffer
	c.Error(run([]string{"sign", file}, env, &stdout, &stderr), "run sign")

	_, statErr := os.Stat(file + ".sig")
	c.True(os.IsNotExist(statErr), "sig file exists, want absent: %v", statErr)
}

func TestRunVerifyUnknownKey(t *testing.T) {
	c := assert.NewCollecting(t)
	seed, _, err := relsign.GenerateKey()
	c.Require().NoError(err, "GenerateKey()")
	priv, err := relsign.PrivateFromSeed(seed)
	c.Require().NoError(err, "PrivateFromSeed()")

	dir := t.TempDir()
	file := filepath.Join(dir, "checksums.txt")
	c.Require().NoError(os.WriteFile(file, []byte("contents\n"), 0o644), "write file")
	sigFile := filepath.Join(dir, "checksums.txt.sig")
	c.Require().NoError(os.WriteFile(sigFile, relsign.Sign(priv, []byte("contents\n")), 0o644), "write sig")

	var stdout, stderr bytes.Buffer
	err = run([]string{"verify", file, sigFile, "v0.2.0"}, mapGetenv(nil), &stdout, &stderr)
	c.ErrorIs(err, relsign.ErrUnknownKey, "run verify")
}

func TestRunUnknownVerb(t *testing.T) {
	c := assert.NewCollecting(t)
	var stdout, stderr bytes.Buffer
	err := run([]string{"frobnicate"}, mapGetenv(nil), &stdout, &stderr)
	c.EqualError(err, "usage: relsign keygen | sign <file> | verify <file> <sigfile> <tag>", "run unknown verb")
}
