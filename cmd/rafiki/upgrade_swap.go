// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Seams for tests.
var (
	linkFile   = os.Link
	renameFile = os.Rename
)

// swapPair replaces the file at Live with the file at Staged. Both must be
// absolute paths in the same directory.
type swapPair struct{ Staged, Live string }

// swapBinaries replaces each pair's Live with its Staged, in order. It never
// removes the Staged files; the caller cleans them up.
func swapBinaries(pairs []swapPair) error {
	for _, p := range pairs {
		if !filepath.IsAbs(p.Staged) || !filepath.IsAbs(p.Live) {
			return fmt.Errorf("swap %s: staged %q and live %q must be absolute paths", p.Live, p.Staged, p.Live)
		}
		if filepath.Dir(p.Staged) != filepath.Dir(p.Live) {
			return fmt.Errorf("swap %s: staged %q and live %q must be in the same directory", p.Live, p.Staged, p.Live)
		}
	}

	type swapped struct {
		staged, live string
		backup       string
		hasBackup    bool
	}
	var done []swapped

	// rollback undoes every swap already done, newest first: the new binary
	// goes back to its staged path, then the old one is restored from the
	// backup. A backup whose restore fails is left in place.
	rollback := func() []error {
		var errs []error
		for i := len(done) - 1; i >= 0; i-- {
			st := done[i]
			if err := renameFile(st.live, st.staged); err != nil {
				errs = append(errs, fmt.Errorf("rollback %s: %w", st.live, err))
			}
			if st.hasBackup {
				if err := renameFile(st.backup, st.live); err != nil {
					errs = append(errs, fmt.Errorf("restore %s: %w", st.live, err))
				}
			}
		}
		return errs
	}

	for _, p := range pairs {
		st := swapped{staged: p.Staged, live: p.Live}
		switch _, err := os.Stat(p.Live); {
		case err == nil:
			backup, err := backupFile(p.Live)
			if err != nil {
				return errors.Join(append([]error{fmt.Errorf("swap %s: %w", p.Live, err)}, rollback()...)...)
			}
			st.backup, st.hasBackup = backup, true
		case os.IsNotExist(err):
			// A fresh install: nothing to back up.
		default:
			return errors.Join(append([]error{fmt.Errorf("swap %s: %w", p.Live, err)}, rollback()...)...)
		}

		if err := renameFile(p.Staged, p.Live); err != nil {
			errs := rollback()
			if st.hasBackup {
				if rerr := os.Remove(st.backup); rerr != nil {
					errs = append(errs, fmt.Errorf("remove backup %s: %w", st.backup, rerr))
				}
			}
			return errors.Join(append([]error{fmt.Errorf("swap %s: %w", p.Live, err)}, errs...)...)
		}
		done = append(done, st)
	}

	var errs []error
	for _, st := range done {
		if !st.hasBackup {
			continue
		}
		if err := os.Remove(st.backup); err != nil {
			errs = append(errs, fmt.Errorf("remove backup %s: %w", st.backup, err))
		}
	}
	return errors.Join(errs...)
}

// backupFile hardlinks live to a fresh ".old.<random>" name beside it, falling
// back to a copy when the filesystem has no hardlinks.
func backupFile(live string) (string, error) {
	info, err := os.Stat(live)
	if err != nil {
		return "", err
	}
	name, err := swapBackupName(live)
	if err != nil {
		return "", err
	}
	if err := linkFile(live, name); err == nil {
		return name, nil
	}
	src, err := os.Open(live)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.OpenFile(name, os.O_EXCL|os.O_CREATE|os.O_WRONLY, info.Mode())
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func swapBackupName(live string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return live + ".old." + hex.EncodeToString(b[:]), nil
}
