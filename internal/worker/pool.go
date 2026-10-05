package worker

import (
	"context"
	"sync"

	"github.com/tok1e1/doc-processor/internal/queue"
)

type HandlerFunc func(ctx context.Context, msg queue.Message)

// Pool runs a fixed number of goroutines that handle messages from src.
type Pool struct {
	size    int
	handler HandlerFunc

	// OnBusyChange is called with +1/-1 when a worker starts/finishes a message.
	OnBusyChange func(delta float64)
	// OnPanic is called when a handler panics. The message is rejected to the dead-letter queue
	// so a poison message cannot crash-loop the worker.
	OnPanic func(v any)
}

func NewPool(size int, handler HandlerFunc) *Pool {
	return &Pool{size: size, handler: handler}
}

// Run blocks until src is closed and every in-flight message is handled.
//
// Handlers get ctx, which should outlive the shutdown signal: the consumer closes src
// on shutdown, and the remaining messages are still processed and acknowledged.
func (p *Pool) Run(ctx context.Context, src <-chan queue.Message) {
	var wg sync.WaitGroup
	wg.Add(p.size)
	for range p.size {
		go func() {
			defer wg.Done()
			for msg := range src {
				p.handle(ctx, msg)
			}
		}()
	}
	wg.Wait()
}

func (p *Pool) handle(ctx context.Context, msg queue.Message) {
	p.busy(1)
	defer p.busy(-1)
	defer func() {
		if v := recover(); v != nil {
			_ = msg.Reject()
			if p.OnPanic != nil {
				p.OnPanic(v)
			}
		}
	}()
	p.handler(ctx, msg)
}

func (p *Pool) busy(delta float64) {
	if p.OnBusyChange != nil {
		p.OnBusyChange(delta)
	}
}
