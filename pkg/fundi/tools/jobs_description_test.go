package tools

import (
	"strings"
	"testing"
)

// These are prose assertions, not behavior — but the rule is checkable
// (does the text say what we mean it to), so it is checked rather than left
// to a human proofreading a multi-line Go string constant. Mirrors
// TestAgentSpawnDescriptionDoesNotInviteWatchingForCompletion: the daemon
// watches background jobs and injects the exit (cmd/rafikid/jobwatch.go), so
// the description must promise the notification and refuse the poll loop the
// old text invited — a model that polls burns a full 20k-char tail per call.
func TestBashStartDescriptionPromisesNotificationNotPolling(t *testing.T) {
	d := bashStartDescription
	if strings.Contains(d, "Poll with bash_output") || strings.Contains(d, "Poll it with bash_output") {
		t.Error("bash_start description still instructs the model to poll for completion")
	}
	if !strings.Contains(d, "notified") {
		t.Error("bash_start description does not promise the finish notification")
	}
	if !strings.Contains(d, "do not") && !strings.Contains(d, "Do not") {
		t.Error("bash_start description does not explicitly tell the model not to poll")
	}
}

func TestBashOutputDescriptionIsForReadingNotWaiting(t *testing.T) {
	d := bashOutputDescription
	if !strings.Contains(d, "notified") {
		t.Error("bash_output description does not point at the finish notification")
	}
	if !strings.Contains(d, "loop") {
		t.Error("bash_output description does not warn against calling it in a loop")
	}
}

// The bash_start RESULT text reaches the model on every start; it must carry
// the same promise the description does.
func TestBashStartResultNamesTheNotification(t *testing.T) {
	got := bashStartResultText("job-1")
	if !strings.Contains(got, "notified") {
		t.Errorf("result %q does not promise the notification", got)
	}
	if strings.Contains(got, "Poll it with") {
		t.Errorf("result %q still invites polling", got)
	}
}
