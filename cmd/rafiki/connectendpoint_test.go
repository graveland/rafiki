// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
)

// newTestRoot builds a root command carrying the same persistent flags the
// real one does, so tests exercise flag plumbing rather than reimplementing it.
func newTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("profile", "P", "", "")
	return root
}

func TestConnectSocketIsASiblingOfTheProfilesControlSocket(t *testing.T) {
	p := profile.Resolved{Profile: profile.Profile{Name: "scratch", Socket: "/tmp/scratch/controller.sock"}}
	if got, want := connectSocketFor(p), filepath.Join("/tmp/scratch", "connect.sock"); got != want {
		t.Fatalf("connectSocketFor = %q, want %q", got, want)
	}
}

func TestConnectEndpointFollowsTheProfileToARemote(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.WriteToken("personal", "sk-personal"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	ep, err := newConnectEndpoint(newTestRoot())
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != "https://rafiki.example.net" {
		t.Fatalf("baseURL = %q", ep.baseURL)
	}
	if ep.identity != "https://rafiki.example.net" {
		t.Fatalf("identity = %q; the completion cache keys on it", ep.identity)
	}
}

func TestConnectEndpointFollowsTheProfileToASocket(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: "/tmp/work/controller.sock"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	ep, err := newConnectEndpoint(newTestRoot())
	if err != nil {
		t.Fatalf("newConnectEndpoint: %v", err)
	}
	if ep.baseURL != connectUDSBaseURL {
		t.Fatalf("baseURL = %q, want the UDS sentinel", ep.baseURL)
	}
	if !strings.HasSuffix(ep.describe, "/tmp/work/connect.sock") {
		t.Fatalf("describe = %q, want the profile's connect socket", ep.describe)
	}
	if ep.identity != "unix:/tmp/work/connect.sock" {
		t.Fatalf("identity = %q", ep.identity)
	}
}

func TestConnectEndpointRefusesARemoteWithNoToken(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	if _, err := newConnectEndpoint(newTestRoot()); err == nil {
		t.Fatal("newConnectEndpoint with a tokenless remote = nil error")
	}
}

func TestSocketFlagIsGone(t *testing.T) {
	root := newRootCmd()
	if f := root.PersistentFlags().Lookup("socket"); f != nil {
		t.Fatal("--socket is still registered; everything it expressed is a profile field now")
	}
}
