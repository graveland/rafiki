package eventbuf

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeClockAfterFuncFiresOnAdvance(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	var fired int32
	c.AfterFunc(5*time.Second, func() { atomic.AddInt32(&fired, 1) })

	c.Advance(4 * time.Second)
	if atomic.LoadInt32(&fired) != 0 {
		t.Fatal("timer fired early")
	}
	c.Advance(1 * time.Second)
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("timer did not fire at its deadline")
	}
	c.Advance(10 * time.Second)
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("timer fired more than once")
	}
}

func TestFakeClockStopPreventsFiring(t *testing.T) {
	c := NewFakeClock(time.Unix(0, 0))
	var fired int32
	tm := c.AfterFunc(5*time.Second, func() { atomic.AddInt32(&fired, 1) })
	if !tm.Stop() {
		t.Fatal("Stop on a pending timer should report true")
	}
	c.Advance(10 * time.Second)
	if atomic.LoadInt32(&fired) != 0 {
		t.Fatal("a stopped timer must not fire")
	}
}
