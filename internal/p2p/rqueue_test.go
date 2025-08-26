package p2p

import (
	"testing"
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
	if _,err:=sq.Recv(ctx); err!=nil {
		t.Fatal(err)
	}
	if sq.Len()!=0 {
		t.Fatalf("queue length is %d, should be 0", sq.Len())
	}
}
