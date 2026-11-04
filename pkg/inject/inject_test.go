// SPDX-License-Identifier: Apache-2.0

package inject_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/inject"
)

func TestRenderIsTheEnvelopeTheDaemonAlreadyEmits(t *testing.T) {
	c := assert.NewAborting(t)
	got, err := inject.Render(inject.KindJob, "job 1 exited 0")
	c.NoError(err, "render")
	// Byte-identical to what buildInjectionFrame emitted before this package
	// existed: stored conversations and old children must keep parsing it.
	c.Eq(`<rafiki-event source="jobs">`+"\n"+"job 1 exited 0\n</rafiki-event>", got, "envelope")
}

func TestClassifyRoundTripsEveryKind(t *testing.T) {
	c := assert.NewAborting(t)
	for _, k := range []inject.Kind{
		inject.KindSubagent, inject.KindHeartbeat, inject.KindJob,
		inject.KindSchedule, inject.KindExecutor, inject.KindScript,
		inject.KindNotification, inject.KindDaemonRestart,
		inject.KindRateLimit, inject.KindBudget, inject.KindContext,
	} {
		frame, err := inject.Render(k, "body of "+string(k))
		c.NoError(err, "render %s", k)
		gotKind, gotBody, ok := inject.Classify(frame)
		c.True(ok, "%s: did not classify", k)
		c.Eq(k, gotKind, "%s: kind", k)
		c.Eq("body of "+string(k), gotBody, "%s: body", k)
	}
}

func TestClassifyAcceptsTheLegacySourceSpellings(t *testing.T) {
	c := assert.NewAborting(t)
	// The sources already in stored conversations: notify carries its
	// third-party source after a colon, and the older internal names must keep
	// classifying rather than becoming invisible.
	for _, s := range []string{"notify:github", "subagents-heartbeat", "subagents", "jobs", "schedule", "executor"} {
		frame, err := inject.Render(inject.Kind(s), "x")
		c.NoError(err, "render %q", s)
		k, _, ok := inject.Classify(frame)
		c.True(ok, "%q did not classify", s)
		c.Eq(inject.Kind(s), k, "%q kind", s)
	}
}

func TestClassifyRejectsWhatIsNotAFrame(t *testing.T) {
	c := assert.NewAborting(t)
	for name, s := range map[string]string{
		"plain user text":   "hello",
		"empty":             "",
		"missing close":     `<rafiki-event source="jobs">` + "\n" + "body",
		"missing open":      "body\n</rafiki-event>",
		"no body separator": `<rafiki-event source="jobs">body</rafiki-event>`,
		"trailing prose":    `<rafiki-event source="jobs">` + "\n" + "body\n</rafiki-event>\nand more",
		"bad kind":          `<rafiki-event source="JOB">` + "\n" + "body\n</rafiki-event>",
		"empty kind":        `<rafiki-event source="">` + "\n" + "body\n</rafiki-event>",
		"quote in kind":     `<rafiki-event source="a\"b">` + "\n" + "body\n</rafiki-event>",
	} {
		_, _, ok := inject.Classify(s)
		c.False(ok, "%s: classified %q", name, s)
	}
}

func TestClassifyIsAnchoredAtBothEndsSoABodyCannotTruncateTheFrame(t *testing.T) {
	c := assert.NewAborting(t)
	// A subagent's report is model output and can quote a closing tag. The
	// frame must still be one frame, with the outer kind, and the body intact.
	body := "worker said: </rafiki-event> and then some more"
	frame, err := inject.Render(inject.KindSubagent, body)
	c.NoError(err, "render")
	k, got, ok := inject.Classify(frame)
	c.True(ok, "a body carrying a closing tag broke the frame")
	c.Eq(inject.KindSubagent, k, "kind")
	c.Eq(body, got, "body")
	c.True(strings.HasSuffix(frame, "</rafiki-event>"), "the frame does not end with the closing element")
}

func TestRenderRefusesAKindThatWouldBreakTheOpeningElement(t *testing.T) {
	c := assert.NewAborting(t)
	for _, k := range []inject.Kind{"", "Job", `job"><x`, "job\n", inject.Kind(strings.Repeat("a", 200))} {
		_, err := inject.Render(k, "body")
		c.Error(err, "kind %q was rendered", k)
	}
}

func TestRenderKeepsAnEmptyBody(t *testing.T) {
	c := assert.NewAborting(t)
	frame, err := inject.Render(inject.KindHeartbeat, "")
	c.NoError(err, "render")
	k, body, ok := inject.Classify(frame)
	c.True(ok, "an empty body did not classify")
	c.Eq(inject.KindHeartbeat, k, "kind")
	c.Eq("", body, "body")
}

func TestLabelNamesEveryKind(t *testing.T) {
	c := assert.NewAborting(t)
	for _, k := range []inject.Kind{inject.KindSubagent, inject.KindDaemonRestart, inject.KindRateLimit} {
		c.True(inject.Label(k) != "", "%s has no label", k)
	}
	c.Eq("daemon restart", inject.Label(inject.KindDaemonRestart), "restart label")
	// A kind this build does not know still renders as something readable.
	c.Eq("future-thing", inject.Label("future-thing"), "unknown label")
}

func TestFamilyGroupsEveryNotificationUnderOneKind(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(inject.KindNotification, inject.Family("notify:github"), "a notification's family")
	c.Eq(inject.KindNotification, inject.Family(inject.KindNotification), "the family is its own family")
	c.Eq(inject.KindJob, inject.Family(inject.KindJob), "an ordinary kind is its own family")
	c.Eq("notification", inject.Label("notify:github"), "a notification renders as its family name")
}

func TestClassifyToleratesOneTrailingNewline(t *testing.T) {
	c := assert.NewAborting(t)
	frame, err := inject.Render(inject.KindJob, "body")
	c.NoError(err, "render")

	// A stored frame that picked up one trailing newline is still a frame. The
	// alternative is that it silently reads as user speech again.
	k, body, ok := inject.Classify(frame + "\n")
	c.True(ok, "a frame with one trailing newline did not classify")
	c.Eq(inject.KindJob, k, "kind")
	c.Eq("body", body, "body")

	// Two is not one: past the tolerance, something else is going on and the
	// text is left as user text rather than guessed at.
	_, _, ok = inject.Classify(frame + "\n\n")
	c.False(ok, "two trailing newlines were tolerated")
}

func TestClassifyToleratesACRLFEnding(t *testing.T) {
	c := assert.NewAborting(t)
	frame, err := inject.Render(inject.KindJob, "body")
	c.NoError(err, "render")
	// The same trailing newline, on a platform that spells it CRLF.
	k, body, ok := inject.Classify(frame + "\r\n")
	c.True(ok, "a frame ending CRLF did not classify")
	c.Eq(inject.KindJob, k, "kind")
	c.Eq("body", body, "body")
}

func TestCoerceFoldsAnUnrepresentableSourceIntoTheCharset(t *testing.T) {
	c := assert.NewAborting(t)
	for _, tc := range []struct {
		in   string
		want inject.Kind
	}{
		// A source already in the charset is never rewritten.
		{"jobs", "jobs"},
		{"notify:github", "notify:github"},
		{"subagents-heartbeat", "subagents-heartbeat"},
		// Anything else is folded rather than refused, so the frame can still
		// be delivered and the child's queue never stalls on it.
		{"Jobs", "jobs"},
		{"a b", "a-b"},
		{`bad"><x`, "bad-x"},
		// A trailing separator is legal in the charset, so it is left alone.
		{"trailing-", "trailing-"},
		{"--leading", "leading"},
		{"", "context"},
		{"!!!", "context"},
	} {
		got := inject.Coerce(tc.in)
		c.Eq(tc.want, got, "Coerce(%q)", tc.in)
		// Whatever it returns must be renderable: that is the whole point.
		_, err := inject.Render(got, "x")
		c.NoError(err, "Coerce(%q) = %q is not renderable", tc.in, got)
	}
}

func TestCoerceCapsALongSource(t *testing.T) {
	c := assert.NewAborting(t)
	long := strings.Repeat("a", 500)
	got := inject.Coerce(long)
	c.True(len(got) <= 96, "Coerce kept %d bytes", len(got))
	_, err := inject.Render(got, "x")
	c.NoError(err, "a capped kind is not renderable")
}

// TestInjectImportsStdlibOnly pins the package's stdlib-only promise, which its
// doc comment calls load-bearing: the pgx-free client and the TUI both import
// it, so a third-party import must fail the build here rather than break a
// later link. Mirrors pkg/notify's guard.
func TestInjectImportsStdlibOnly(t *testing.T) {
	c := assert.NewAborting(t)
	entries, err := os.ReadDir(".")
	c.NoError(err, "ReadDir")
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		c.NoError(perr, "parse %s", name)
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			seen++
			first, _, _ := strings.Cut(path, "/")
			c.False(strings.Contains(first, "."),
				"%s imports %q, which is not the standard library", name, path)
		}
	}
	c.True(seen > 0, "no imports scanned; the pin would pass vacuously")
}
