package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu        sync.Mutex
	pending   []Message
	published []int64
}

func (s *memStore) ProcessBatch(ctx context.Context, limit int, publish PublishFunc) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.pending[:min(limit, len(s.pending))]
	if len(batch) == 0 {
		return 0, nil
	}
	ids, err := publish(ctx, batch)
	s.published = append(s.published, ids...)
	s.pending = s.pending[len(ids):]
	return len(ids), err
}

func (s *memStore) Cleanup(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *memStore) state() (pending int, published []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), append([]int64(nil), s.published...)
}

type flakyPublisher struct {
	mu     sync.Mutex
	failOn map[string]int // routing key -> remaining failures
	sent   []string
}

func (p *flakyPublisher) Publish(_ context.Context, key string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failOn[key] > 0 {
		p.failOn[key]--
		return errors.New("broker unavailable")
	}
	p.sent = append(p.sent, key)
	return nil
}

func TestRelayPublishesInOrderAndRetries(t *testing.T) {
	store := &memStore{}
	for i, key := range []string{"a", "b", "c", "d", "e"} {
		store.pending = append(store.pending, Message{ID: int64(i + 1), RoutingKey: key})
	}
	pub := &flakyPublisher{failOn: map[string]int{"c": 2}}

	relay := NewRelay(store, pub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay.Interval = 5 * time.Millisecond
	relay.BatchSize = 2

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go relay.Run(ctx)

	for {
		if pending, _ := store.state(); pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("relay did not drain the outbox")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()

	_, published := store.state()
	for i, id := range published {
		if id != int64(i+1) {
			t.Fatalf("messages published out of order: %v", published)
		}
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.sent) != 5 {
		t.Fatalf("sent %v, want 5 messages exactly once", pub.sent)
	}
}
