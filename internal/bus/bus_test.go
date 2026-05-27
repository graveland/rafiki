package bus_test

import (
	"sync"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/bus"
)

func TestBus_SubscribeAndPublish(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 4})
	defer b.Close()

	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(1)
	b.Publish(2)

	got := []int{}
	for i := 0; i < 2; i++ {
		select {
		case v := <-ch:
			got = append(got, v)
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, want [1 2]", got)
	}
}

func TestBus_MultipleSubscribers_IndependentChannels(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 4})
	defer b.Close()

	ch1, c1 := b.Subscribe()
	ch2, c2 := b.Subscribe()
	defer c1()
	defer c2()

	b.Publish(42)
	for _, ch := range []<-chan int{ch1, ch2} {
		select {
		case v := <-ch:
			if v != 42 {
				t.Fatalf("got %d", v)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber missed event")
		}
	}
}

func TestBus_DropsOnFullChannel_DoesNotBlock(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 2})
	defer b.Close()

	_, cancel := b.Subscribe()
	defer cancel()

	// Publish more than buffer can hold. Must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Publish(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}

	// Subscriber's drop counter must show at least 8 drops.
	stats := b.Stats()
	if stats.SubscriberCount != 1 {
		t.Fatalf("expected 1 subscriber, got %d", stats.SubscriberCount)
	}
	if stats.TotalDrops < 8 {
		t.Fatalf("expected at least 8 drops, got %d", stats.TotalDrops)
	}
}

func TestBus_CancelRemovesSubscriber(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 1})
	defer b.Close()

	ch, cancel := b.Subscribe()
	cancel()

	// After cancel, the channel must close eventually.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to close")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after cancel")
	}
	if b.Stats().SubscriberCount != 0 {
		t.Fatal("subscriber not removed")
	}
}

func TestBus_CloseClosesAllSubscriberChannels(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 1})
	ch1, _ := b.Subscribe()
	ch2, _ := b.Subscribe()
	b.Close()
	for i, ch := range []<-chan int{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("ch%d: expected closed", i+1)
			}
		case <-time.After(time.Second):
			t.Fatalf("ch%d: not closed after Close", i+1)
		}
	}
}

// TestBus_ConcurrentPublishAndCancel stresses the race between Publish and
// cancel. Without the per-subscriber mutex this panics with send on closed channel
// within a handful of iterations.
func TestBus_ConcurrentPublishAndCancel(t *testing.T) {
	for i := 0; i < 200; i++ {
		b := bus.New[int](bus.Options{PerSubBuffer: 1})
		_, cancel := b.Subscribe()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.Publish(j)
			}
		}()
		go func() {
			defer wg.Done()
			cancel()
		}()
		wg.Wait()
		b.Close()
	}
	// Reaching here without panicking confirms the race is fixed.
}

// TestBus_ConcurrentPublishAndClose stresses the race between Publish and
// Close. Without the per-subscriber mutex this panics with send on closed channel
// within a handful of iterations.
func TestBus_ConcurrentPublishAndClose(t *testing.T) {
	for i := 0; i < 200; i++ {
		b := bus.New[int](bus.Options{PerSubBuffer: 1})
		_, _ = b.Subscribe()
		_, _ = b.Subscribe()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.Publish(j)
			}
		}()
		go func() {
			defer wg.Done()
			b.Close()
		}()
		wg.Wait()
	}
}
