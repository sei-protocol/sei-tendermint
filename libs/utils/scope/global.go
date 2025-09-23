package scope

import (
	"context"
)

type GlobalHandle struct {
	ctx context.Context
	cancel context.CancelFunc
	done chan struct{}
	err error
}

func SpawnGlobal(task func(ctx context.Context) error) *GlobalHandle {
	ctx, cancel := context.WithCancel(context.Background())
	h := &GlobalHandle{
		ctx: ctx,
		cancel: cancel,
		done: make(chan struct{}),
	}
	go func() {
		h.err = task(ctx)
		close(h.done)
	}()
	return h
}

func (h *GlobalHandle) Done() <-chan struct{} {
	return h.done
}

func (h *GlobalHandle) Err() error {
	select {
	case <-h.done:
		return h.err
	default:
		return nil
	}
}

func (h *GlobalHandle) Close() error {
	h.cancel()
	<-h.done
	return h.err
}
