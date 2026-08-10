package tools_test

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// Without an executor these tools must not exist at all. A bash_start that
// can only answer "no executor configured" is a turn the model wastes.
func TestJobToolsDeclineWithoutAnExecutor(t *testing.T) {
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd:   t.TempDir(),
		Tasks: nil,
	})
	for _, name := range []string{"bash_start", "bash_output", "bash_kill"} {
		if _, err := reg.Execute(context.Background(), name, []byte(`{}`)); err == nil {
			t.Fatalf("%s is registered without an executor; it must decline", name)
		}
	}
}

func TestBashStartReturnsAHandle(t *testing.T) {
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	out, err := reg.Execute(context.Background(), "bash_start", []byte(`{"command":"npm run dev"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "job-1") {
		t.Fatalf("result = %q; want it to name the handle", out)
	}
	if got := fake.JobCommand("job-1"); got != "npm run dev" {
		t.Fatalf("command = %q, want %q", got, "npm run dev")
	}
}

func TestBashOutputReportsRunningAndFinishedJobs(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "building...") || !strings.Contains(out, "running") {
		t.Fatalf("result = %q; want the output and a running marker", out)
	}

	fake.SetJobOutput("job-1", "building...\ndone\n", true, 0)
	out, err = reg.Execute(ctx, "bash_output", []byte(`{"handle":"job-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit code 0") {
		t.Fatalf("result = %q; want the exit code once the job finished", out)
	}
}

func TestBashOutputOnAnUnknownHandleIsNotAnError(t *testing.T) {
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	out, err := reg.Execute(context.Background(), "bash_output", []byte(`{"handle":"nope"}`))
	if err != nil {
		t.Fatalf("an unknown handle must be a readable result, not a tool error: %v", err)
	}
	if !strings.Contains(out, "no such job") {
		t.Fatalf("result = %q; want it to say the handle is unknown", out)
	}
}

func TestBashKillKillsTheJob(t *testing.T) {
	fake := executorclient.NewFake()
	reg := tools.DefaultBlueprint.MaterializeAll(tools.ToolOpts{
		Cwd: t.TempDir(), Executor: fake,
	})
	ctx := context.Background()
	if _, err := reg.Execute(ctx, "bash_start", []byte(`{"command":"sleep 99"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Execute(ctx, "bash_kill", []byte(`{"handle":"job-1"}`)); err != nil {
		t.Fatal(err)
	}
	if !fake.Killed("job-1") {
		t.Fatal("bash_kill did not reach the executor")
	}
}
