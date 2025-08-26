package p2p

import (
	"context"
	"testing"
	"time"
)

func TestSimpleQueue(t *testing.T) {
	ctx := t.Context()

	// set up a small queue with very small buffers so we can
	// watch it shed load, then send a bunch of messages to the
	// queue, most of which we'll watch it drop.
	sq := NewQueue(1)
	for range 100 {
		sq.Send(Envelope{From: "merlin"},0)
	}

	seen := 0

	for seen <= 2 {
		ctx,cancel := context.WithTimeout(ctx,10 * time.Millisecond)
		defer cancel()
		if _,err:=sq.Recv(ctx); err!=nil {
			break
		}
	}
	// if we don't see any messages, then it's just broken.
	if seen != 1 {
		t.Errorf("seen %d messages, should have seen %v", seen, 1)
	}
}
