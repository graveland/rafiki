// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/providers"
)

// providersHeader opens every providers.toml setup writes. It points at the
// provider guide for everything setup deliberately does not configure.
const providersHeader = `# providers.toml — written by ` + "`rafiki setup`" + `.
# Setup rewrites this file on a re-run only while it is unchanged since setup
# wrote it; once you edit it, setup leaves it alone (and offers to back it up).
# Aliases, ` + "`only`" + ` pins, via_executor, fallback chains and session headers are
# hand-edited: see docs/guide/proxy.md.

`

// providersFileState classifies providers.toml against the hash setup recorded.
type providersFileState int

const (
	providersFileMissing providersFileState = iota // no file at path
	providersFileOwned                             // bytes hash to the recorded SHA-256
	providersFileForeign                           // exists, and no hash recorded or a different one
)

// sha256Hex is the hex SHA-256 of b, the form install.toml records.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// classifyProvidersFile reads path and classifies it against recordedSHA.
// current is the file's bytes (nil when missing). A read error other than
// not-exist is returned.
func classifyProvidersFile(path, recordedSHA string) (state providersFileState, current []byte, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return providersFileMissing, nil, nil
		}
		return providersFileMissing, nil, err
	}
	if recordedSHA != "" && sha256Hex(b) == recordedSHA {
		return providersFileOwned, b, nil
	}
	return providersFileForeign, b, nil
}

// renderProvider, renderSet, renderEmbeddings and renderSummaries are
// render-only mirrors of the providers types: every field carries omitempty so
// an unset value is omitted rather than written as `= ""`. Setting up does not
// write model declarations, extras, via_executor, session headers or any
// fallback beyond what Default() carries.
type renderProvider struct {
	Kind      providers.Kind `toml:"kind"`
	BaseURL   string         `toml:"base_url,omitempty"`
	APIKeyEnv string         `toml:"api_key_env,omitempty"`
	Fallback  []string       `toml:"fallback,omitempty"`
}

type renderEmbeddings struct {
	URL        string `toml:"url"`
	APIKeyEnv  string `toml:"api_key_env,omitempty"`
	Model      string `toml:"model"`
	Dimensions int    `toml:"dimensions"`
}

type renderSummaries struct {
	Model            string `toml:"model"`
	MaxSegmentTokens int    `toml:"max_segment_tokens,omitempty"`
}

type renderSet struct {
	DefaultProvider string                    `toml:"default_provider"`
	Providers       map[string]renderProvider `toml:"providers"`
	Embeddings      *renderEmbeddings         `toml:"embeddings,omitempty"`
	Summaries       *renderSummaries          `toml:"summaries,omitempty"`
}

// renderProviders renders set deterministically: providersHeader, then TOML
// with map keys sorted. It calls set.Validate() first and returns its error.
// It refuses (error naming the provider) any field setup does not write —
// Models, Extras, ViaExecutor, SessionHeader non-empty — so an owned file can
// never silently lose hand-written content.
func renderProviders(set *providers.Set) ([]byte, error) {
	if set == nil {
		return nil, errors.New("providers: no set to render")
	}
	if err := set.Validate(); err != nil {
		return nil, err
	}
	out := &renderSet{
		DefaultProvider: set.DefaultProvider,
		Providers:       make(map[string]renderProvider, len(set.Providers)),
	}
	for _, name := range set.Names() {
		p := set.Providers[name]
		if len(p.Models) > 0 {
			return nil, fmt.Errorf("providers: %s: setup does not write model declarations (hand-edit providers.toml)", name)
		}
		if len(p.Extras) > 0 {
			return nil, fmt.Errorf("providers: %s: setup does not write extras (hand-edit providers.toml)", name)
		}
		if p.ViaExecutor != nil {
			return nil, fmt.Errorf("providers: %s: setup does not write via_executor (hand-edit providers.toml)", name)
		}
		if p.SessionHeader != "" {
			return nil, fmt.Errorf("providers: %s: setup does not write session_header (hand-edit providers.toml)", name)
		}
		out.Providers[name] = renderProvider{
			Kind:      p.Kind,
			BaseURL:   p.BaseURL,
			APIKeyEnv: p.APIKeyEnv,
			Fallback:  p.Fallback,
		}
	}
	if set.Embeddings != nil {
		out.Embeddings = &renderEmbeddings{
			URL:        set.Embeddings.URL,
			APIKeyEnv:  set.Embeddings.APIKeyEnv,
			Model:      set.Embeddings.Model,
			Dimensions: set.Embeddings.Dimensions,
		}
	}
	if set.Summaries != nil {
		out.Summaries = &renderSummaries{
			Model:            set.Summaries.Model,
			MaxSegmentTokens: set.Summaries.MaxSegmentTokens,
		}
	}
	var buf bytes.Buffer
	buf.WriteString(providersHeader)
	if err := toml.NewEncoder(&buf).Encode(out); err != nil {
		return nil, fmt.Errorf("providers: render: %w", err)
	}
	return buf.Bytes(), nil
}

// backupName is path plus a UTC timestamp, optionally disambiguated with a
// "-<n>" counter before ".bak".
func backupName(path string, now time.Time, n int) string {
	base := path + "." + now.UTC().Format("20060102T150405Z")
	if n == 0 {
		return base + ".bak"
	}
	return fmt.Sprintf("%s-%d.bak", base, n)
}

// backupProvidersFile copies path to a timestamped ".bak" name, creating it
// with O_CREATE|O_EXCL and the original's mode. If that name exists, it tries
// "-1", "-2", … appended before ".bak" (up to 100). It never overwrites, and
// returns the backup path.
func backupProvidersFile(path string, now time.Time) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	for n := 0; n < 100; n++ {
		name := backupName(path, now, n)
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		if _, err := f.Write(b); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("providers: %s: 100 backups already exist for %s", path, now.UTC().Format("20060102T150405Z"))
}

// writeProvidersFile writes b to path atomically (os.CreateTemp in the same
// dir + Chmod 0o644 + os.Rename), creating the dir 0o700 if needed. If the
// file already holds exactly b, it writes nothing and returns changed=false.
func writeProvidersFile(path string, b []byte) (changed bool, err error) {
	if cur, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(cur, b) {
		return false, nil
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".providers.toml-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return false, err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	renamed = true
	return true, nil
}
