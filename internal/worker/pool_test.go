package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tok1e1/doc-processor/internal/queue"
)

func TestPoolProcessesAllMessagesConcurrently(t *testing.T) {
	const (
		size     = 4
		messages = 20
	)
	var (
		handled  atomic.Int32
		inFlight atomic.Int32
		peak     atomic.Int32
	)
	pool := NewPool(size, func(_ context.Context, msg queue.Message) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		handled.Add(1)
		_ = msg.Ack()
	})

	src := make(chan queue.Message)
	done := make(chan struct{})
	go func() {
		pool.Run(context.Background(), src)
		close(done)
	}()

	for range messages {
		src <- newMessage(t, uuid.New())
	}
	close(src)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not stop after the source was closed")
	}
	if handled.Load() != messages {
		t.Fatalf("handled %d of %d messages", handled.Load(), messages)
	}
	if peak.Load() > size {
		t.Fatalf("concurrency limit exceeded: peak %d > %d", peak.Load(), size)
	}
	if peak.Load() < 2 {
		t.Fatalf("messages were not processed concurrently (peak %d)", peak.Load())
	}
}

func TestPoolRecoversFromPanics(t *testing.T) {
	var panics atomic.Int32
	pool := NewPool(1, func(context.Context, queue.Message) { panic("boom") })
	pool.OnPanic = func(any) { panics.Add(1) }

	msg := newMessage(t, uuid.New())
	src := make(chan queue.Message, 1)
	src <- msg
	close(src)

	pool.Run(context.Background(), src)

	if panics.Load() != 1 {
		t.Fatalf("OnPanic called %d times", panics.Load())
	}
	if msg.result() != rejected {
		t.Fatalf("panicking message must be rejected, got %q", msg.result())
	}
}
