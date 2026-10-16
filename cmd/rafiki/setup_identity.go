// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// runIdentityStep ensures this machine has a local user and a profile carrying
// its credential. It returns the profile name to use downstream.
//
// The token minted by `rafikid user create --token` (or `rafikid user token
// mint`) is parsed out of the subprocess stdout and written straight to the
// profile's 0600 token file — it is never printed, never passed in argv, and
// the database DSN reaches rafikid through RAFIKI_DB in the environment, never
// as a flag.
func runIdentityStep(ctx context.Context, w *wizard, r runner, out io.Writer, st *installstate.State, dsn, rafikidPath string) (string, error) {
	profileName := st.Identity.Profile
	if profileName == "" {
		profileName = profile.DefaultName
	}

	detectedUser := detectUserName()
	user, err := w.askString("Your rafiki user name", "user", st.Identity.User, detectedUser, true)
	if err != nil {
		return "", err
	}

	// Already done? A token on disk that the daemon accepts means there is
	// nothing to mint.
	if profile.ReadToken(profileName) != "" && statusOK(ctx, profileName) {
		// The profile already carries a working credential. upsertSetupProfile is
		// a read-modify-write that rewrites socket/url/proxy/kind, so calling it
		// on an existing profile would silently repoint one the operator may have
		// aimed at a remote daemon. Only upsert when the profile does not exist
		// yet (a first run); otherwise leave it exactly as it is.
		set, lerr := profile.Load()
		exists := false
		switch {
		case lerr == nil:
			_, exists = set.Get(profileName)
		case errors.Is(lerr, profile.ErrNoManifest):
			// No profiles file at all: nothing exists yet, so upsert.
		default:
			return "", lerr
		}
		if !exists {
			if err := upsertSetupProfile(profileName); err != nil {
				return "", err
			}
		}
		if err := profile.SavePointer(profileName); err != nil {
			return "", err
		}
		st.Identity.User = user
		st.Identity.Profile = profileName
		if err := saveStep(*st, stepIdentity); err != nil {
			return "", err
		}
		fmt.Fprintln(out, "    ok (identity already configured)")
		return profileName, nil
	}

	tok, err := mintLocalToken(ctx, r, dsn, rafikidPath, user)
	if err != nil {
		return "", err
	}
	if err := profile.WriteToken(profileName, tok); err != nil {
		return "", err
	}
	if err := upsertSetupProfile(profileName); err != nil {
		return "", err
	}
	if err := profile.SavePointer(profileName); err != nil {
		return "", err
	}
	if !statusOK(ctx, profileName) {
		return "", fmt.Errorf("the daemon at %s did not accept the new token", paths.SocketPath())
	}

	st.Identity.User = user
	st.Identity.Profile = profileName
	if err := saveStep(*st, stepIdentity); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "    ok (user %q, profile %q)\n", user, profileName)
	return profileName, nil
}

// mintLocalToken runs `rafikid user create <user> --admin --token`, falling
// back to `rafikid user token mint <user>` when the user already exists. Both
// print a `token: <value>` line, which is parsed and returned.
func mintLocalToken(ctx context.Context, r runner, dsn, rafikidPath, user string) (string, error) {
	env := []string{"RAFIKI_DB=" + dsn}
	stdout, stderr, err := r.Run(ctx, env, rafikidPath, "user", "create", user, "--admin", "--token")
	if err != nil {
		if strings.Contains(string(stderr), "already exists") {
			mout, merr, mErr := r.Run(ctx, env, rafikidPath, "user", "token", "mint", user)
			if mErr != nil {
				return "", fmt.Errorf("rafikid user token mint %s: %w: %s", user, mErr, strings.TrimSpace(string(merr)))
			}
			tok := parseTokenLine(mout)
			if tok == "" {
				return "", errors.New("rafikid user token mint printed no token line")
			}
			return tok, nil
		}
		return "", fmt.Errorf("rafikid user create %s: %w: %s", user, err, strings.TrimSpace(string(stderr)))
	}
	tok := parseTokenLine(stdout)
	if tok == "" {
		return "", errors.New("rafikid user create printed no token line")
	}
	return tok, nil
}

// parseTokenLine extracts the value from the `token: <value>` line rafikid's
// user commands print. The token itself is never logged.
func parseTokenLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "token: "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// upsertSetupProfile points the named profile at this machine's daemon, keeping
// any other fields already set (model, labels, ...). It clears url: setup
// targets the local daemon, and a profile may not carry both socket and url.
func upsertSetupProfile(name string) error {
	set, err := profile.Load()
	if errors.Is(err, profile.ErrNoManifest) {
		set = profile.Set{}
	} else if err != nil {
		return err
	}
	if set.Profiles == nil {
		set.Profiles = map[string]profile.Profile{}
	}
	p := set.Profiles[name]
	p.Name = name
	p.Socket = paths.SocketPath()
	p.URL = ""
	p.Proxy = "http://127.0.0.1:8035"
	p.Kind = protocol.KindClaude
	set.Profiles[name] = p
	return profile.Save(set)
}

// statusOK reports whether a Status call on the named profile succeeds — the
// bar for "this profile's credential works".
func statusOK(ctx context.Context, profileName string) bool {
	ep, err := setupEndpoint(profileName)
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = ep.control().Status(cctx, connect.NewRequest(&rafikiv1.StatusRequest{}))
	return err == nil
}

// detectUserName proposes the local account name: $USER lowercased, with
// anything outside [a-z0-9_-] removed so the daemon's user-name rules are met
// before the value is ever offered.
func detectUserName() string {
	u := strings.ToLower(os.Getenv("USER"))
	var b strings.Builder
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}
