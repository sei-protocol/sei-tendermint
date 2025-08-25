package p2p

import (
	"context"
)

// queue does QoS scheduling for Envelopes, enqueueing and dequeueing according
// to some policy. Queues are used at contention points, i.e.:
//
// - Receiving inbound messages to a single channel from all peers.
// - Sending outbound messages to a single peer from all channels.
type queue interface {
	enqueue() chan<- Envelope // enqueue returns a channel for submitting envelopes.
	dequeue() <-chan Envelope // dequeue returns a channel ordered according to some queueing policy.
	run(ctx context.Context) error
}

// fifoQueue is a simple unbuffered lossless queue that passes messages through
// in the order they were received, and blocks until message is received.
type fifoQueue chan Envelope

func newFIFOQueue(size int) queue {
	return fifoQueue(make(chan Envelope, size))
}

func (q fifoQueue) enqueue() chan<- Envelope { return q }
func (q fifoQueue) dequeue() <-chan Envelope { return q }
func (q fifoQueue) run(ctx context.Context) error { return nil }
