// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
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
	_, err := g.Ban(context.Background(), now, "Open Inference", 0, "spinning")
	assert.NewAborting(t).NoError(err)
	for _, m := range []string{"deepseek/deepseek-v4-pro", "z-ai/glm-5.3-flash", "moonshotai/kimi-k3"} {
		if got := g.IgnoredFor(now, m); len(got) != 1 || got[0] != "open-inference" {
			t.Errorf("IgnoredFor(%s) = %v, want [open-inference]", m, got)
		}
	}
}

// TestBanWithoutTTLNeverExpires proves a zero duration means "until lifted",
// not "already expired" — the zero-value trap this field would otherwise be.
func TestBanWithoutTTLNeverExpires(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "openinference", 0, "")
	c.Require().NoError(err)
	c.Len(g.IgnoredFor(now.Add(10*365*24*time.Hour), "any/model"), 1, "IgnoredFor ten years on")
}

func TestBanWithTTLExpires(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "openinference", time.Hour, "")
	c.Require().NoError(err)
	c.Len(g.IgnoredFor(now.Add(59*time.Minute), "any/model"), 1, "at 59m IgnoredFor")
	c.Empty(g.IgnoredFor(now.Add(61*time.Minute), "any/model"), "at 61m IgnoredFor")
}

// TestBanSurvivesPerLineCap proves operator bans are exempt from the cap: the
// guard recording its own ejections on a line — three land, the rest are
// declined at the cap — neither evicts the ban nor has the ban count against
// the guard's own three.
func TestBanSurvivesPerLineCap(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "zulu", 0, "")
	c.Require().NoError(err)
	for i, p := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} {
		conv := string(rune('a' + i))
		for range 6 {
			g.Observe(now.Add(time.Duration(i)*time.Second), miss(conv, p))
		}
	}
	got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro")
	c.False(len(got) != 4 || got[3] != "zulu", "IgnoredFor = %v, want three guard ejections plus zulu", got)
}

// TestBanDeduplicatesWithGuardEjection proves a provider both ejected by the
// guard and banned appears once in the ignore list.
func TestBanDeduplicatesWithGuardEjection(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	_, err := g.Ban(context.Background(), now, "coreweave", 0, "")
	c.Require().NoError(err)
	c.Len(g.IgnoredFor(now, "deepseek/deepseek-v4-pro"), 1, "IgnoredFor")
}

// TestLiftRemovesOnlyTheOperatorBan proves Lift leaves the guard's own
// line-scoped ejection of the same provider standing.
func TestLiftRemovesOnlyTheOperatorBan(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	now := time.Now()
	for range 6 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	if _, err := g.Ban(context.Background(), now, "coreweave", 0, ""); err != nil {
		t.Fatal(err)
	}
	c.Require().NoError(g.Lift(context.Background(), now, "CoreWeave"))
	c.Empty(g.IgnoredFor(now, "z-ai/glm-5.2"), "glm IgnoredFor")
	c.Len(g.IgnoredFor(now, "deepseek/deepseek-v4-pro"), 1, "deepseek IgnoredFor")
}

func TestLiftWithoutBanIsErrNoBan(t *testing.T) {
	g := testGuard()
	assert.NewCollecting(t).ErrorIs(g.Lift(context.Background(), time.Now(), "nobody"), ErrNoBan, "Lift")
}

// TestBanAndLiftAreLogged proves both land in the durable log, the lift as a
// superseding row rather than a delete.
func TestBanAndLiftAreLogged(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	sink := &fakeSink{}
	g.SetSink(sink)
	now := time.Now()
	if _, err := g.Ban(context.Background(), now, "openinference", 0, "spinning"); err != nil {
		t.Fatal(err)
	}
	c.Require().NoError(g.Lift(context.Background(), now, "openinference"))
	c.Require().Len(sink.recs, 2, "sink has %d rows, want 2", len(sink.recs))
	ban, lift := sink.recs[0], sink.recs[1]
	c.False(ban.Reason != ReasonOperator || ban.ModelLine != AllModelLines || !ban.ExpiresAt.IsZero() || ban.Note != "spinning", "ban row = %+v", ban)
	c.False(lift.Reason != ReasonLift || lift.Provider != "openinference" || lift.ModelLine != AllModelLines, "lift row = %+v", lift)
}

// TestBanSinkFailureDoesNotApply proves a ban that couldn't be recorded is
// reported and NOT applied, so the operator never believes a ban stuck when
// it would vanish on restart.
func TestBanSinkFailureDoesNotApply(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	g.SetSink(&fakeSink{err: errors.New("db down")})
	now := time.Now()
	_, err := g.Ban(context.Background(), now, "openinference", 0, "")
	c.Require().Error(err, "Ban succeeded against a failing sink")
	c.Empty(g.IgnoredFor(now, "any/model"), "IgnoredFor")
}

// TestObserveOffKeepsBans proves RAFIKI_PROVIDER_GUARD=off's meaning: no
// automatic ejection, operator bans still apply.
func TestObserveOffKeepsBans(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	g.SetObserve(false)
	now := time.Now()
	for range 20 {
		g.Observe(now, miss("c1", "CoreWeave"))
	}
	_, err := g.Ban(context.Background(), now, "openinference", 0, "")
	c.Require().NoError(err)
	got := g.IgnoredFor(now, "deepseek/deepseek-v4-pro")
	c.False(len(got) != 1 || got[0] != "openinference", "IgnoredFor = %v, want [openinference] only", got)
}

func TestBanRejectsBadSlugs(t *testing.T) {
	c := assert.NewCollecting(t)
	g := testGuard()
	for _, p := range []string{"", "   ", "*", "a\tb"} {
		_, err := g.Ban(context.Background(), time.Now(), p, 0, "")
		c.Error(err, "Ban(%q) succeeded, want refusal", p)
	}
	_, err := g.Ban(context.Background(), time.Now(), "x", -time.Hour, "")
	c.Error(err, "Ban with a negative duration succeeded")
}

func TestBanNilGuard(t *testing.T) {
	var g *ProviderGuard
	_, err := g.Ban(context.Background(), time.Now(), "x", 0, "")
	assert.NewCollecting(t).Error(err, "Ban on a nil guard succeeded")
}
