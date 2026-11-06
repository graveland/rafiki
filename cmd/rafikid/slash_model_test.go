package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// argvLoggingClaude is a fake claude that appends its argv to a log on every
// launch, so a test can read back exactly what a restart ran with.
func argvLoggingClaude(t *testing.T) (script, log string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "fakeclaude.sh")
	log = filepath.Join(dir, "argv.log")
	body := "#!/bin/bash\n" +
		"echo \"$*\" >> " + log + "\n" +
		"SID=fresh\n" +
		"for a in \"$@\"; do if [ \"$prev\" = \"--resume\" ]; then SID=\"got-$a\"; fi; prev=\"$a\"; done\n" +
		"printf '%s\\n' \"{\\\"type\\\":\\\"system\\\",\\\"subtype\\\":\\\"init\\\",\\\"session_id\\\":\\\"$SID\\\",\\\"model\\\":\\\"claude-opus-4-8\\\"}\"\n" +
		"while IFS= read -r line; do :; done\n" +
		"while true; do sleep 0.05; done\n"
	assert.NewAborting(t).NoError(os.WriteFile(script, []byte(body), 0o755), "write fake")
	return script, log
}

func modelCommand(childID, text string) inbox.Inbound {
	return inbox.Inbound{ChildID: childID, Mode: inbox.ModePrompt, Text: text}
}

// /model on a claude child rewrites the stored model and relaunches the process
// under the same child id, resuming the same session with the new --model.
func TestSlashModelRestartsClaudeOnTheNewModel(t *testing.T) {
	ck := assert.NewAborting(t)
	script, argvLog := argvLoggingClaude(t)
	c, id := newClaudeTestChild(t, script)
	before, _ := c.cm.Get(id)
	pidBefore := before.PID()

	handled, err := c.handleSlashCommand(t.Context(), modelCommand(id, "/model anthropic/claude-sonnet-5"))
	ck.Require().NoError(err, "/model")
	ck.True(handled, "/model is executed here and queues nothing")

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ch, ok := c.cm.Get(id); ok && ch.PID() != pidBefore && ch.Status() == protocol.StatusIdle {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap, _ := c.st.Get(id)
	ck.Eq("claude-sonnet-5", snap.Model, "the stored model carries the bare id")

	// The fake logs its argv as its first act, which can trail the pid change
	// the loop above waits on.
	var lines []string
	for logDeadline := time.Now().Add(5 * time.Second); time.Now().Before(logDeadline); time.Sleep(20 * time.Millisecond) {
		raw, err := os.ReadFile(argvLog)
		ck.Require().NoError(err, "read argv log")
		lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) >= 2 {
			break
		}
	}
	ck.Require().Len(lines, 2, "launches: spawn + restart")
	ck.StrContains(lines[1], "--model claude-sonnet-5", "the restart runs on the new model")
	ck.StrContains(lines[1], "--resume fresh", "the restart resumes the same session")
}

// Everything a restart cannot carry is refused, naming the fork, and leaves the
// model untouched.
func TestSlashModelRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		line     string
		oldModel string
		ctxID    *server.Identity
		wantCode string
	}{
		"another provider":       {"/model openrouter/z-ai/glm-5.3-flash", "claude-opus-5", nil, protocol.ErrInvalidArgs},
		"tier alias":             {"/model anthropic/opus-latest", "claude-opus-5", nil, protocol.ErrInvalidArgs},
		"no argument":            {"/model", "claude-opus-5", nil, protocol.ErrInvalidArgs},
		"launched non-anthropic": {"/model claude-sonnet-5", "z-ai/glm-5.3-flash", nil, protocol.ErrInvalidArgs},
		"child credential": {"/model claude-sonnet-5", "claude-opus-5",
			&server.Identity{Via: server.ProvenanceChildToken, ChildID: "c_other"}, protocol.ErrPermissionDenied},
	} {
		t.Run(name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			ctrl, rec, _ := slashFixture(t)
			id := spawnTestChild(t, ctrl, nil)
			ck.NoError(ctrl.st.Update(id, func(s *childstore.Session) { s.Model = tc.oldModel }), "seed model")

			ctx := t.Context()
			if tc.ctxID != nil {
				ctx = server.WithIdentity(ctx, tc.ctxID)
			}
			_, err := ctrl.connectInbox().Accept(ctx, modelCommand(id, tc.line))
			var ce *connectapi.ControllerError
			ck.Require().True(errors.As(err, &ce), "Accept(%q) = %v; want a coded error", tc.line, err)
			ck.Eq(tc.wantCode, ce.Code, "error code")
			snap, _ := ctrl.st.Get(id)
			ck.Eq(tc.oldModel, snap.Model, "a refused /model leaves the model alone")
			ck.Empty(rec.accepted(), "a refused /model queues nothing")
		})
	}
}

// A restart that cannot start puts the old model back: the stored model must
// stay the one the process last ran on.
func TestSlashModelRestoresTheModelWhenTheRestartFails(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, _, _ := slashFixture(t)
	ctrl.st.Insert(&childstore.Session{ChildID: "c_gone", Kind: protocol.KindClaude, Status: protocol.StatusIdle, Model: "claude-opus-5"})

	_, err := ctrl.connectInbox().Accept(t.Context(), modelCommand("c_gone", "/model claude-sonnet-5"))
	ck.Error(err, "no live process to restart")
	snap, _ := ctrl.st.Get("c_gone")
	ck.Eq("claude-opus-5", snap.Model, "model restored after a failed restart")
}

// A fundi child has no in-place switch: the cockpit forks it instead.
func TestSlashModelIsRefusedForFundi(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl, rec, _ := slashFixture(t)
	id := spawnFundiChild(t, ctrl)

	_, err := ctrl.connectInbox().Accept(t.Context(), modelCommand(id, "/model claude-sonnet-5"))
	var ce *connectapi.ControllerError
	ck.Require().True(errors.As(err, &ce), "want a coded error, got %v", err)
	ck.Eq(protocol.ErrInvalidArgs, ce.Code, "code")
	ck.Empty(rec.accepted(), "nothing queued")
}
