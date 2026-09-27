package tools_test

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

// Without an executor these tools must not exist at all. A bash_start that
// can only answer "no executor configured" is a turn the model wastes.
func TestJobToolsDeclineWithoutAnExecutor(t *testing.T) {
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd:   t.TempDir(),
		Tasks: nil,
	})
	for _, name := range []string{"bash_start", "bash_output", "bash_kill"} {
		_, err := reg.Execute(context.Background(), name, []byte(`{}`))
		assert.NewAborting(t).Error(err, "%s is registered without an executor; it must decline", name)
	}
}

func TestBashStartReturnsAHandle(t *testing.T) {
	c := assert.NewAborting(t)
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	out, err := reg.Execute(context.Background(), "bash_start", []byte(`{"command":"npm run dev"}`))
	c.NoError(err)
	c.StrContains(out, "job-1", "result")
	c.Eq("npm run dev", fake.JobCommand("job-1"), "command")
}

func TestBashOutputReportsRunningAndFinishedJobs(t *testing.T) {
	c := assert.NewAborting(t)
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	ctx := context.Background()
	if _, err := reg.Execute(ctx, "bash_start", []byte(`{"command":"sleep 1"}`)); err != nil {
		t.Fatal(err)
	}

	fake.SetJobOutput("job-1", "building...\n", false, 0)
	out, err := reg.Execute(ctx, "bash_output", []byte(`{"handle":"job-1"}`))
	c.NoError(err)
	c.False(!strings.Contains(out, "building...") || !strings.Contains(out, "running"), "result = %q; want the output and a running marker", out)

	fake.SetJobOutput("job-1", "building...\ndone\n", true, 0)
	out, err = reg.Execute(ctx, "bash_output", []byte(`{"handle":"job-1"}`))
	c.NoError(err)
	c.StrContains(out, "exit code 0", "result")
}

func TestBashOutputOnAnUnknownHandleIsNotAnError(t *testing.T) {
	c := assert.NewAborting(t)
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	out, err := reg.Execute(context.Background(), "bash_output", []byte(`{"handle":"nope"}`))
	c.NoError(err, "an unknown handle must be a readable result, not a tool error")
	c.StrContains(out, "no such job", "result")
}

func TestBashKillKillsTheJob(t *testing.T) {
	c := assert.NewAborting(t)
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	ctx := context.Background()
	if _, err := reg.Execute(ctx, "bash_start", []byte(`{"command":"sleep 99"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := reg.Execute(ctx, "bash_kill", []byte(`{"handle":"job-1"}`))
	c.NoError(err)
	c.True(fake.Killed("job-1"), "bash_kill did not reach the executor")
}
