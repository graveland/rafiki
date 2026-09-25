// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSink is an in-memory EjectionSink that records appends and can be told
// to fail.
type fakeSink struct {
	mu   sync.Mutex
	recs []EjectionRecord
	err  error
}

func (s *fakeSink) Append(_ context.Context, e EjectionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.recs = append(s.recs, e)
	return nil
}

func (s *fakeSink) Active(context.Context, time.Time) ([]EjectionRecord, error) { return nil, nil }

// TestBanAppliesToEveryModelLine proves an operator ban is global: it lands in
// the ignore list of models the guard has never observed.
func TestBanAppliesToEveryModelLine(t *testing.T) {
	g := testGuard()
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "Open Inference", 0, "spinning"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"deepseek/deepseek-v4-pro", "z-ai/glm-5.3-flash", "moonshotai/kimi-k3"} {
		if got := g.IgnoredFor(now, m); len(got) != 1 || got[0] != "open-inference" {
			t.Errorf("IgnoredFor(%s) = %v, want [open-inference]", m, got)
		}
	}
}

// TestBanWithoutTTLNeverExpires proves a zero duration means "until lifted",
// not "already expired" — the zero-value trap this field would otherwise be.
func TestBanWithoutTTLNeverExpires(t *testing.T) {
	g := testGuard()
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "openinference", 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := g.IgnoredFor(now.Add(10*365*24*time.Hour), "any/model"); len(got) != 1 {
		t.Errorf("IgnoredFor ten years on = %v, want [openinference]", got)
	}
}

func TestBanWithTTLExpires(t *testing.T) {
	g := testGuard()
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "openinference", time.Hour, ""); err != nil {
		t.Fatal(err)
	}
	if got := g.IgnoredFor(now.Add(59*time.Minute), "any/model"); len(got) != 1 {
		t.Errorf("at 59m IgnoredFor = %v, want [openinference]", got)
	}
	if got := g.IgnoredFor(now.Add(61*time.Minute), "any/model"); len(got) != 0 {
		t.Errorf("at 61m IgnoredFor = %v, want none", got)
	}
}

// TestBanSurvivesPerLineCap proves operator bans are exempt from the cap: the
// guard ejecting five providers on a line neither evicts the ban nor has the
// ban count against the guard's own three.
func TestBanSurvivesPerLineCap(t *testing.T) {
	g := testGuard()
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "zulu", 0, ""); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} {
		conv := string(rune('a' + i))
		for range 6 {
			g.Observe(now.Add(time.Duration(i)*time.Second), miss(conv, p))
		}
	}
	got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro")
	if len(got) != 4 || got[3] != "zulu" {
		t.Errorf("IgnoredFor = %v, want three guard ejections plus zulu", got)
	}
}

// TestBanDeduplicatesWithGuardEjection proves a provider both ejected by the
// guard and banned appears once in the ignore list.
func TestBanDeduplicatesWithGuardEjection(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	if _, err := g.Ban(context.Background(), now, "coreweave", 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 1 {
		t.Errorf("IgnoredFor = %v, want [coreweave] once", got)
	}
}

// TestLiftRemovesOnlyTheOperatorBan proves Lift leaves the guard's own
// line-scoped ejection of the same provider standing.
func TestLiftRemovesOnlyTheOperatorBan(t *testing.T) {
	g := testGuard()
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	if _, err := g.Ban(context.Background(), now, "coreweave", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := g.Lift(context.Background(), now, "CoreWeave"); err != nil {
		t.Fatal(err)
	}
	if got := g.IgnoredFor(now, "z-ai/glm-5.2"); len(got) != 0 {
		t.Errorf("glm IgnoredFor = %v, want none after lift", got)
	}
	if got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro"); len(got) != 1 {
		t.Errorf("deepseek IgnoredFor = %v, want the guard's [coreweave] to stand", got)
	}
}

func TestLiftWithoutBanIsErrNoBan(t *testing.T) {
	g := testGuard()
	if err := g.Lift(context.Background(), time.Now(), "nobody"); !errors.Is(err, ErrNoBan) {
		t.Errorf("Lift = %v, want ErrNoBan", err)
	}
}

// TestBanAndLiftAreLogged proves both land in the durable log, the lift as a
// superseding row rather than a delete.
func TestBanAndLiftAreLogged(t *testing.T) {
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "openinference", 0, "spinning"); err != nil {
		t.Fatal(err)
	}
	if err := g.Lift(context.Background(), now, "openinference"); err != nil {
		t.Fatal(err)
	}
	if len(sink.recs) != 2 {
		t.Fatalf("sink has %d rows, want 2", len(sink.recs))
	}
	ban, lift := sink.recs[0], sink.recs[1]
	if ban.Reason != ReasonOperator || ban.ModelLine != AllModelLines || !ban.ExpiresAt.IsZero() || ban.Note != "spinning" {
		t.Errorf("ban row = %+v", ban)
	}
	if lift.Reason != ReasonLift || lift.Provider != "openinference" || lift.ModelLine != AllModelLines {
		t.Errorf("lift row = %+v", lift)
	}
}

// TestBanSinkFailureDoesNotApply proves a ban that couldn't be recorded is
// reported and NOT applied, so the operator never believes a ban stuck when
// it would vanish on restart.
func TestBanSinkFailureDoesNotApply(t *testing.T) {
	g := testGuard()
	g.SetSink(&fakeSink{err: errors.New("db down")})
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "openinference", 0, ""); err == nil {
		t.Fatal("Ban succeeded against a failing sink")
	}
	if got := g.IgnoredFor(now, "any/model"); len(got) != 0 {
		t.Errorf("IgnoredFor = %v, want none", got)
	}
}

// TestObserveOffKeepsBans proves RAFIKI_PROVIDER_GUARD=off's meaning: no
// automatic ejection, operator bans still apply.
func TestObserveOffKeepsBans(t *testing.T) {
	g := testGuard()
	g.SetObserve(false)
	now := time.Now()
	for range 20 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	if _, err := g.Ban(context.Background(), now, "openinference", 0, ""); err != nil {
		t.Fatal(err)
	}
	got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro")
	if len(got) != 1 || got[0] != "openinference" {
		t.Errorf("IgnoredFor = %v, want [openinference] only", got)
	}
}

func TestBanRejectsBadSlugs(t *testing.T) {
	g := testGuard()
	for _, p := range []string{"", "   ", "*", "a\tb"} {
		if _, err := g.Ban(context.Background(), time.Now(), p, 0, ""); err == nil {
			t.Errorf("Ban(%q) succeeded, want refusal", p)
		}
	}
	if _, err := g.Ban(context.Background(), time.Now(), "x", -time.Hour, ""); err == nil {
		t.Error("Ban with a negative duration succeeded")
	}
}

func TestBanNilGuard(t *testing.T) {
	var g *ProviderGuard
	if _, err := g.Ban(context.Background(), time.Now(), "x", 0, ""); err == nil {
		t.Error("Ban on a nil guard succeeded")
	}
}
