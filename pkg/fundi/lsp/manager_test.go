package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestManager_For_NoMatch(t *testing.T) {
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Extensions: []string{".go"}},
		},
	}, "/tmp")

	ctx := context.Background()
	client, err := mgr.For(ctx, "/tmp/foo.py")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if client != nil {
		t.Error("expected nil client for unmatched extension")
	}
}

func TestManager_For_Match(t *testing.T) {
	dir := t.TempDir()

	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found: %v", err)
	}

	// Create a minimal Go module.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: goplsPath, Extensions: []string{".go"}},
		},
	}, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mgr.For(ctx, filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client for .go file")
	}

	// Second call for same language should return the same client.
	client2, err := mgr.For(ctx, filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("For (second): %v", err)
	}
	if client != client2 {
		t.Error("expected same client on second For call")
	}

	// Shutdown.
	mgr.Shutdown(ctx)
}

func TestManager_ServerFor(t *testing.T) {
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go":     {Command: "gopls", Extensions: []string{".go"}},
			"python": {Command: "pyright", Extensions: []string{".py"}},
		},
	}, "/tmp")

	tests := []struct {
		path   string
		want   string
		wantOk bool
	}{
		{"/tmp/main.go", "go", true},
		{"/tmp/thing.py", "python", true},
		{"/tmp/readme.md", "", false},
		{"/tmp/nosuffix", "", false},
	}

	for _, tt := range tests {
		name, _, ok := mgr.serverFor(tt.path)
		if ok != tt.wantOk {
			t.Errorf("serverFor(%q): ok=%v, want %v", tt.path, ok, tt.wantOk)
		}
		if name != tt.want {
			t.Errorf("serverFor(%q): name=%q, want %q", tt.path, name, tt.want)
		}
	}
}

func TestManager_NotifyChange(t *testing.T) {
	dir := t.TempDir()

	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: goplsPath, Extensions: []string{".go"}},
		},
	}, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// NotifyChange on a path without first calling For should lazily start.
	if err := mgr.NotifyChange(ctx, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("NotifyChange: %v", err)
	}

	mgr.Shutdown(ctx)
}

func TestManager_Shutdown(t *testing.T) {
	mgr := NewManager(Config{
		Servers: map[string]ServerConfig{
			"go": {Command: "gopls", Extensions: []string{".go"}},
		},
	}, "/tmp")
	ctx := context.Background()
	mgr.Shutdown(ctx)

	// Operations after shutdown should fail.
	_, err := mgr.For(ctx, "/tmp/main.go")
	if err == nil {
		t.Error("expected error after shutdown")
	}
}
