// Package rabbitmq contains the broker adapter: topology, publisher with confirms and a
// reconnecting consumer.
package rabbitmq

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	Exchange       = "documents"
	DeadExchange   = "documents.dlx"
	GenerateQueue  = "documents.generate"
	DeadQueue      = "documents.dead"
	RoutingKeyJobs = "generate"
)

// RetryPolicy describes delayed retries. Each delay gets its own queue with a fixed TTL:
// a single queue with per-message TTL would suffer from head-of-line blocking, because
// RabbitMQ only expires messages at the head of the queue.
type RetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

// Delay returns the back-off before the given attempt (attempt >= 1 is the one that failed).
func (p RetryPolicy) Delay(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if p.MaxDelay > 0 && d >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return d
}

func RetryQueue(delay time.Duration) string {
	return fmt.Sprintf("documents.retry.%dms", delay.Milliseconds())
}

func (p RetryPolicy) delays() []time.Duration {
	seen := map[time.Duration]bool{}
	var out []time.Duration
	for a := 1; a < p.MaxAttempts; a++ {
		d := p.Delay(a)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// Declare creates exchanges and queues. It is idempotent and called on every (re)connect.
func Declare(ch *amqp.Channel, retry RetryPolicy) error {
	if err := ch.ExchangeDeclare(Exchange, amqp.ExchangeDirect, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange: %w", err)
	}
	if err := ch.ExchangeDeclare(DeadExchange, amqp.ExchangeFanout, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dead-letter exchange: %w", err)
	}

	if _, err := ch.QueueDeclare(GenerateQueue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange": DeadExchange,
	}); err != nil {
		return fmt.Errorf("declare queue: %w", err)
	}
	if err := ch.QueueBind(GenerateQueue, RoutingKeyJobs, Exchange, false, nil); err != nil {
		return fmt.Errorf("bind queue: %w", err)
	}

	if _, err := ch.QueueDeclare(DeadQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dead queue: %w", err)
	}
	if err := ch.QueueBind(DeadQueue, "", DeadExchange, false, nil); err != nil {
		return fmt.Errorf("bind dead queue: %w", err)
	}

	for _, d := range retry.delays() {
		if _, err := ch.QueueDeclare(RetryQueue(d), true, false, false, false, amqp.Table{
			"x-message-ttl":             d.Milliseconds(),
			"x-dead-letter-exchange":    Exchange,
			"x-dead-letter-routing-key": RoutingKeyJobs,
		}); err != nil {
			return fmt.Errorf("declare retry queue %s: %w", RetryQueue(d), err)
		}
	}
	return nil
}
