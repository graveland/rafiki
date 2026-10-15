package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fakeSchedules records what the schedule tools asked the daemon-side
// ScheduleManager to do. It enforces nothing — validation, clamping, storage
// and the child binding all live on the daemon side of the seam.
type fakeSchedules struct {
	createCalls []struct {
		Cron      string
		Prompt    string
		Recurring bool
	}
	deleteCalls []string
	listCalls   int
	wakeupCalls []struct {
		Delay  time.Duration
		Prompt string
	}
	cancelCalls int

	createInfo ScheduleInfo
	createErr  error
	deleteErr  error
	listInfo   []ScheduleInfo
	listErr    error
	wakeupInfo ScheduleInfo
	wakeupErr  error
	// cancelResult is what CancelWakeup reports: true when there was a
	// pending wakeup.
	cancelResult bool
	cancelErr    error
}

func (f *fakeSchedules) CronCreate(_ context.Context, cron, prompt string, recurring bool) (ScheduleInfo, error) {
	if f.createErr != nil {
		return ScheduleInfo{}, f.createErr
	}
	f.createCalls = append(f.createCalls, struct {
		Cron      string
		Prompt    string
		Recurring bool
	}{cron, prompt, recurring})
	return f.createInfo, nil
}

func (f *fakeSchedules) CronDelete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleteCalls = append(f.deleteCalls, id)
	return nil
}

func (f *fakeSchedules) CronList(context.Context) ([]ScheduleInfo, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listInfo, nil
}

func (f *fakeSchedules) ScheduleWakeup(_ context.Context, delay time.Duration, prompt string) (ScheduleInfo, error) {
	if f.wakeupErr != nil {
		return ScheduleInfo{}, f.wakeupErr
	}
	f.wakeupCalls = append(f.wakeupCalls, struct {
		Delay  time.Duration
		Prompt string
	}{delay, prompt})
	return f.wakeupInfo, nil
}

func (f *fakeSchedules) CancelWakeup(context.Context) (bool, error) {
	f.cancelCalls++
	if f.cancelErr != nil {
		return false, f.cancelErr
	}
	return f.cancelResult, nil
}

// schRun materializes just the four schedule tools with mgr bound — the way
// the daemon binds one — and executes the named one. It returns the
// registry's raw (text, error): the exact strings the model sees.
func schRun(t *testing.T, mgr ScheduleManager, name, input string) (string, error) {
	t.Helper()
	reg := DefaultBlueprint.MaterializeOnly(ToolOpts{Schedules: mgr},
		[]string{"CronCreate", "CronDelete", "CronList", "ScheduleWakeup"})
	res, err := reg.Execute(context.Background(), name, json.RawMessage(input))
	return res.Text, err
}

// TestCronCreateDefaultsRecurringTrue: a nil recurring must reach the manager
// as true — the default lives ONLY here at the tool, which is the only place
// that can tell "the model said nothing" from an explicit false.
func TestCronCreateDefaultsRecurringTrue(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{createInfo: ScheduleInfo{
		ID: "job-1", Kind: "cron", Cron: "0 9 * * *", TZ: "UTC",
		NextFire: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
	}}
	out, err := schRun(t, mgr, "CronCreate", `{"cron":"0 9 * * *","prompt":"stand up"}`)
	c.NoError(err, "CronCreate with no recurring")
	c.Len(mgr.createCalls, 1, "CronCreate calls")
	c.True(mgr.createCalls[0].Recurring, "recurring defaulted to true")
	c.Eq("0 9 * * *", mgr.createCalls[0].Cron, "cron passed through")
	c.Eq("stand up", mgr.createCalls[0].Prompt, "prompt passed through")
	c.StrContains(out, "recurring", "result names the schedule as recurring: %q", out)
	c.StrContains(out, "job-1", "result names the job id: %q", out)
}

// TestCronCreateHonoursRecurringFalse: an explicit false survives — the tool
// must not coerce it back to the default.
func TestCronCreateHonoursRecurringFalse(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{createInfo: ScheduleInfo{
		ID: "job-2", Kind: "cron", Cron: "30 14 1 4 *", TZ: "UTC",
		NextFire: time.Date(2026, 4, 1, 14, 30, 0, 0, time.UTC),
	}}
	out, err := schRun(t, mgr, "CronCreate", `{"cron":"30 14 1 4 *","prompt":"one off","recurring":false}`)
	c.NoError(err, "CronCreate with recurring=false")
	c.Len(mgr.createCalls, 1, "CronCreate calls")
	c.False(mgr.createCalls[0].Recurring, "recurring=false honoured")
	c.StrContains(out, "one-shot", "result names the schedule as one-shot: %q", out)
}

// TestCronCreateRequiresCronAndPrompt: a job with no schedule or no prompt is
// a wasted row — refused before it ever reaches the manager.
func TestCronCreateRequiresCronAndPrompt(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, in := range []string{`{"prompt":"x"}`, `{"cron":"0 9 * * *"}`} {
		mgr := &fakeSchedules{}
		_, err := schRun(t, mgr, "CronCreate", in)
		c.EqualError(err, "CronCreate: cron and prompt are required", "input %s", in)
		c.Empty(mgr.createCalls, "a refused create must not reach the manager (input %s)", in)
	}
}

// TestCronCreateIgnoresDurable: durable is Claude Code's field, accepted so a
// Claude-shaped call validates, and deliberately ignored.
func TestCronCreateIgnoresDurable(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{createInfo: ScheduleInfo{
		ID: "job-3", Kind: "cron", Cron: "0 9 * * *", TZ: "UTC",
		NextFire: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
	}}
	_, err := schRun(t, mgr, "CronCreate", `{"cron":"0 9 * * *","prompt":"x","durable":true}`)
	c.NoError(err, "CronCreate with durable=true")
	c.Len(mgr.createCalls, 1, "CronCreate calls")
	c.True(mgr.createCalls[0].Recurring, "durable must not disturb recurring's default")
}

// TestCronListRendersHeldAndTruncatesPrompt: a held count is shown only when
// non-zero, and a long prompt is cut to 80 runes with an ellipsis.
func TestCronListRendersHeldAndTruncatesPrompt(t *testing.T) {
	c := assert.NewCollecting(t)
	long := strings.Repeat("x", 100)
	mgr := &fakeSchedules{listInfo: []ScheduleInfo{
		{
			ID: "job-1", Kind: "cron", Cron: "0 9 * * *", TZ: "UTC",
			Recurring: true, HeldCount: 3, Prompt: "short",
			NextFire: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		},
		{
			ID: "job-2", Kind: "wakeup", TZ: "UTC",
			Prompt:   long,
			NextFire: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC),
		},
	}}
	out, err := schRun(t, mgr, "CronList", `{}`)
	c.NoError(err, "CronList")
	c.Eq(1, mgr.listCalls, "CronList calls")
	c.StrContains(out, "held 3", "held count rendered: %q", out)
	c.StrContains(out, "wakeup", "a wakeup job renders as wakeup: %q", out)
	c.StrContains(out, strings.Repeat("x", 80)+"…", "prompt truncated to 80 runes with an ellipsis: %q", out)
	c.False(strings.Contains(out, long), "the untruncated prompt must not appear: %q", out)
}

// TestCronListEmpty: no jobs is a plain sentence, not an empty string.
func TestCronListEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{}
	out, err := schRun(t, mgr, "CronList", `{}`)
	c.NoError(err, "CronList")
	c.Eq("no scheduled jobs", out, "empty-list text")
}

// TestScheduleWakeupStopCancels: stop:true cancels the pending wakeup and
// never schedules one.
func TestScheduleWakeupStopCancels(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{cancelResult: true}
	out, err := schRun(t, mgr, "ScheduleWakeup", `{"stop":true}`)
	c.NoError(err, "ScheduleWakeup stop")
	c.Eq(1, mgr.cancelCalls, "CancelWakeup calls")
	c.Len(mgr.wakeupCalls, 0, "stop must not call ScheduleWakeup")
	c.Eq("wakeup cancelled", out, "cancelled text")

	// And a cancel with nothing pending says so rather than claiming success.
	mgr = &fakeSchedules{cancelResult: false}
	out, err = schRun(t, mgr, "ScheduleWakeup", `{"stop":true}`)
	c.NoError(err, "ScheduleWakeup stop with nothing pending")
	c.Eq("no pending wakeup", out, "nothing-pending text")
}

// TestScheduleWakeupPassesDelay: delaySeconds arrives at the manager as a
// time.Duration of exactly that many seconds.
func TestScheduleWakeupPassesDelay(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{wakeupInfo: ScheduleInfo{
		ID: "wake-1", Kind: "wakeup", TZ: "UTC",
		NextFire: time.Date(2026, 3, 2, 10, 10, 0, 0, time.UTC),
	}}
	out, err := schRun(t, mgr, "ScheduleWakeup", `{"delaySeconds":600,"prompt":"check the build"}`)
	c.NoError(err, "ScheduleWakeup")
	c.Len(mgr.wakeupCalls, 1, "ScheduleWakeup calls")
	c.Eq(10*time.Minute, mgr.wakeupCalls[0].Delay, "delay passed as a Duration")
	c.Eq("check the build", mgr.wakeupCalls[0].Prompt, "prompt passed through")
	c.StrContains(out, "wake-1", "result names the wakeup id: %q", out)
}

// TestScheduleWakeupRequiresDelayAndPrompt: without stop, both a positive
// delay and a prompt are required.
func TestScheduleWakeupRequiresDelayAndPrompt(t *testing.T) {
	c := assert.NewCollecting(t)
	mgr := &fakeSchedules{}
	_, err := schRun(t, mgr, "ScheduleWakeup", `{"prompt":"check"}`)
	c.EqualError(err, "ScheduleWakeup: delaySeconds is required unless stop is true", "no delay")
	c.Len(mgr.wakeupCalls, 0, "a refused wakeup must not reach the manager")

	_, err = schRun(t, mgr, "ScheduleWakeup", `{"delaySeconds":600}`)
	c.EqualError(err, "ScheduleWakeup: prompt is required unless stop is true", "no prompt")
	c.Len(mgr.wakeupCalls, 0, "a refused wakeup must not reach the manager")
}

// TestScheduleToolsDeclineWithoutManager: a caller with no schedule
// capability gets none of the four tools registered — the decline rule, not
// registered-and-failing.
func TestScheduleToolsDeclineWithoutManager(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, bp := range []Tool{
		&CronCreateBlueprint{}, &CronDeleteBlueprint{},
		&CronListBlueprint{}, &ScheduleWakeupBlueprint{},
	} {
		tool, err := bp.(Materializer).Materialize(ToolOpts{})
		c.NoError(err, "%s: Materialize", bp.Name())
		c.Nil(tool, "%s must decline (nil, nil) without a Schedules", bp.Name())
	}

	reg := DefaultBlueprint.MaterializeAll(ToolOpts{Cwd: t.TempDir()})
	for _, name := range []string{"CronCreate", "CronDelete", "CronList", "ScheduleWakeup"} {
		_, err := reg.Execute(context.Background(), name, json.RawMessage(`{}`))
		c.Error(err, "%s must not be registered without a Schedules", name)
		if err != nil {
			c.StrContains(err.Error(), "unknown tool", "%s absence message", name)
		}
	}
}

// TestScheduleDescriptionsMakeNoExpiryOrSessionClaims: a schedule persists
// with its conversation and never expires, so a description must not promise
// otherwise. The checks are whole-word: the required ScheduleWakeup wording
// contains "replaces", which a bare substring search for "repl" would
// wrongly flag.
func TestScheduleDescriptionsMakeNoExpiryOrSessionClaims(t *testing.T) {
	c := assert.NewCollecting(t)
	forbidden := []string{"7-day", "7 day", "auto-expire", "session-only", "session only", "only while", "repl"}
	for _, bp := range []Tool{
		&CronCreateBlueprint{}, &CronDeleteBlueprint{},
		&CronListBlueprint{}, &ScheduleWakeupBlueprint{},
	} {
		desc := strings.ToLower(bp.Description())
		for _, f := range forbidden {
			re := regexp.MustCompile(`\b` + regexp.QuoteMeta(f) + `\b`)
			c.False(re.MatchString(desc), "%s description must not claim %q: %q", bp.Name(), f, desc)
		}
	}
}
