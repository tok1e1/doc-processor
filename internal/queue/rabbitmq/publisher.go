package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

var errNotConfirmed = errors.New("message was not confirmed by the broker")

// Publisher publishes persistent messages with publisher confirms. It reconnects lazily:
// if the channel is gone, the next Publish call dials again.
type Publisher struct {
	url   string
	retry RetryPolicy
	log   *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

func NewPublisher(url string, retry RetryPolicy, log *slog.Logger) *Publisher {
	return &Publisher{url: url, retry: retry, log: log}
}

// Publish sends a job message to the main exchange and waits for the broker confirm.
func (p *Publisher) Publish(ctx context.Context, routingKey string, body []byte) error {
	return p.publish(ctx, Exchange, routingKey, body, nil)
}

// PublishRetry schedules the message for redelivery after the back-off for the given attempt.
func (p *Publisher) PublishRetry(ctx context.Context, body []byte, attempt int) error {
	queue := RetryQueue(p.retry.Delay(attempt))
	return p.publish(ctx, "", queue, body, amqp.Table{"x-attempt": int32(attempt)}) //nolint:gosec // attempts are small
}

func (p *Publisher) publish(ctx context.Context, exchange, key string, body []byte, headers amqp.Table) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureChannel(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	confirm, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, true, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Headers:      headers,
		Body:         body,
	})
	if err != nil {
		p.reset()
		return fmt.Errorf("publish: %w", err)
	}
	ok, err := confirm.WaitContext(ctx)
	if err != nil {
		p.reset()
		return fmt.Errorf("wait confirm: %w", err)
	}
	if !ok {
		return errNotConfirmed
	}
	return nil
}

func (p *Publisher) ensureChannel() error {
	if p.ch != nil && !p.ch.IsClosed() {
		return nil
	}
	p.reset()

	conn, err := amqp.DialConfig(p.url, amqp.Config{Properties: amqp.Table{"connection_name": "doc-processor-publisher"}})
	if err != nil {
		return fmt.Errorf("dial rabbitmq: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("open channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		_ = conn.Close()
		return fmt.Errorf("enable confirms: %w", err)
	}
	if err := Declare(ch, p.retry); err != nil {
		_ = conn.Close()
		return err
	}

	returns := ch.NotifyReturn(make(chan amqp.Return, 16))
	go func() {
		for r := range returns {
			p.log.Error("message returned by broker", "exchange", r.Exchange, "routing_key", r.RoutingKey, "reason", r.ReplyText)
		}
	}()

	p.conn, p.ch = conn, ch
	return nil
}

func (p *Publisher) reset() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn, p.ch = nil, nil
}

func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reset()
	return nil
}

// Ping reports whether the broker is reachable; used by the readiness probe.
func (p *Publisher) Ping(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureChannel()
}
