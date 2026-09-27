package main

import (
	"bytes"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestExecutorNameSetsAndPrints(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := assert.NewAborting(t)

	set := newExecutorNameCmd()
	set.SetArgs([]string{"laptop"})
	var out bytes.Buffer
	set.SetOut(&out)
	c.NoError(set.Execute())

	get := newExecutorNameCmd()
	get.SetArgs(nil)
	out.Reset()
	get.SetOut(&out)
	c.NoError(get.Execute())
	c.StrContains(out.String(), "laptop", "printing the name should show it, got")
}

// The name gates which machine a child lands on, so the command must refuse a
// value a selector would reparse rather than write it and fail later at enroll.
func TestExecutorNameRejectsASelectorBreakingValue(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cmd := newExecutorNameCmd()
	cmd.SetArgs([]string{"my,laptop"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	assert.NewAborting(t).Error(cmd.Execute(), "a comma splits a selector; the command must refuse it")
}

func TestExecutorNameUnsetExplainsBothMechanisms(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := assert.NewAborting(t)
	cmd := newExecutorNameCmd()
	cmd.SetArgs(nil)
	var errBuf bytes.Buffer
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(&errBuf)
	err := cmd.Execute()
	c.Error(err, "no name set: the command must say so")
	c.StrContains(err.Error(), "RAFIKI_EXECUTOR_NAME", "the error must mention the env var, got: %v", err)
}
