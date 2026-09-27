package eventbuf

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestFakeClockAfterFuncFiresOnAdvance(t *testing.T) {
	ck := assert.NewAborting(t)
	c := NewFakeClock(time.Unix(0, 0))
	var fired int32
	c.AfterFunc(5*time.Second, func() { atomic.AddInt32(&fired, 1) })

	c.Advance(4 * time.Second)
	ck.Eq(0, atomic.LoadInt32(&fired), "timer fired early")
	c.Advance(1 * time.Second)
	ck.Eq(1, atomic.LoadInt32(&fired), "timer did not fire at its deadline")
	c.Advance(10 * time.Second)
	ck.Eq(1, atomic.LoadInt32(&fired), "timer fired more than once")
}

func TestFakeClockStopPreventsFiring(t *testing.T) {
	ck := assert.NewAborting(t)
	c := NewFakeClock(time.Unix(0, 0))
	var fired int32
	tm := c.AfterFunc(5*time.Second, func() { atomic.AddInt32(&fired, 1) })
	ck.True(tm.Stop(), "Stop on a pending timer should report true")
	c.Advance(10 * time.Second)
	ck.Eq(0, atomic.LoadInt32(&fired), "a stopped timer must not fire")
}
