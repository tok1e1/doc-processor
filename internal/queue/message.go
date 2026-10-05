// Package queue holds broker-agnostic messaging types.
package queue

// Message is a single delivery from the broker.
type Message interface {
	Body() []byte
	Ack() error
	// Reject drops the message to the dead-letter queue.
	Reject() error
	// Requeue returns the message to the queue for immediate redelivery.
	Requeue() error
}
