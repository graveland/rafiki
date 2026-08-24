package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestExecutorNameSetsAndPrints(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	set := newExecutorNameCmd()
	set.SetArgs([]string{"laptop"})
	var out bytes.Buffer
	set.SetOut(&out)
	if err := set.Execute(); err != nil {
		t.Fatal(err)
	}

	get := newExecutorNameCmd()
	get.SetArgs(nil)
	out.Reset()
	get.SetOut(&out)
	if err := get.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "laptop") {
		t.Fatalf("printing the name should show it, got %q", out.String())
	}
}

// The name gates which machine a child lands on, so the command must refuse a
// value a selector would reparse rather than write it and fail later at enroll.
func TestExecutorNameRejectsASelectorBreakingValue(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cmd := newExecutorNameCmd()
	cmd.SetArgs([]string{"my,laptop"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err == nil {
		t.Fatal("a comma splits a selector; the command must refuse it")
	}
}

func TestExecutorNameUnsetExplainsBothMechanisms(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cmd := newExecutorNameCmd()
	cmd.SetArgs(nil)
	var errBuf bytes.Buffer
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(&errBuf)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("no name set: the command must say so")
	}
	if !strings.Contains(err.Error(), "RAFIKI_EXECUTOR_NAME") {
		t.Fatalf("the error must mention the env var, got: %v", err)
	}
}
