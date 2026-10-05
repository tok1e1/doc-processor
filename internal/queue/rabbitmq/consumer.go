package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/tok1e1/doc-processor/internal/queue"
)

// Consumer reads from the generate queue and reconnects with back-off when the connection drops.
type Consumer struct {
	url      string
	prefetch int
	retry    RetryPolicy
	log      *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
}

func NewConsumer(url string, prefetch int, retry RetryPolicy, log *slog.Logger) *Consumer {
	return &Consumer{url: url, prefetch: prefetch, retry: retry, log: log}
}

// Run forwards deliveries to out until ctx is cancelled, then closes out.
//
// On shutdown the consumer is cancelled first, so the broker stops sending new messages,
// while already prefetched deliveries are still handed over and can be acknowledged.
// Close must be called after the handlers finish.
func (c *Consumer) Run(ctx context.Context, out chan<- queue.Message) {
	defer close(out)

	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		err := c.consume(ctx, out)
		if ctx.Err() != nil {
			return
		}
		c.log.Error("consumer disconnected, reconnecting", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Consumer) consume(ctx context.Context, out chan<- queue.Message) (err error) {
	conn, err := amqp.DialConfig(c.url, amqp.Config{Properties: amqp.Table{"connection_name": "doc-processor-worker"}})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		// On a clean shutdown the connection stays open until Close, so in-flight messages can be acked.
		if err != nil {
			_ = conn.Close()
		}
	}()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	if err := Declare(ch, c.retry); err != nil {
		return err
	}
	if err := ch.Qos(c.prefetch, 0, false); err != nil {
		return fmt.Errorf("set qos: %w", err)
	}

	const tag = "doc-processor-worker"
	deliveries, err := ch.Consume(GenerateQueue, tag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume: %w", err)
	}
	c.log.Info("consuming", "queue", GenerateQueue, "prefetch", c.prefetch)

	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	stop := ctx.Done()
	for {
		select {
		case <-stop:
			// Stop new deliveries; keep draining the channel until the library closes it.
			_ = ch.Cancel(tag, false)
			stop = nil
		case amqpErr := <-closed:
			if amqpErr == nil {
				return errors.New("connection closed")
			}
			return fmt.Errorf("connection closed: %w", amqpErr)
		case d, ok := <-deliveries:
			if !ok {
				if ctx.Err() == nil {
					return errors.New("delivery channel closed by broker")
				}
				return nil
			}
			out <- delivery{d}
		}
	}
}

// Close closes the current connection. Call it after all in-flight messages are acknowledged.
func (c *Consumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

type delivery struct {
	d amqp.Delivery
}

func (m delivery) Body() []byte   { return m.d.Body }
func (m delivery) Ack() error     { return m.d.Ack(false) }
func (m delivery) Reject() error  { return m.d.Nack(false, false) }
func (m delivery) Requeue() error { return m.d.Nack(false, true) }
