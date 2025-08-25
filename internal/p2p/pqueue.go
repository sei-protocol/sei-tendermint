package p2p

import (
	"container/heap"
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/gogo/protobuf/proto"

	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/libs/utils"
)

const defaultCapacity uint = 16e6 // ~16MB

// pqEnvelope defines a wrapper around an Envelope with priority to be inserted
// into a priority queue used for Envelope scheduling.
type pqEnvelope struct {
	envelope  Envelope
	priority  uint
	size      uint
	timestamp time.Time

	index int
}

// priorityQueue defines a type alias for a priority queue implementation.
type priorityQueue []*pqEnvelope

func (pq priorityQueue) get(i int) *pqEnvelope { return pq[i] }
func (pq priorityQueue) Len() int              { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	// higher priority
	if a,b := pq[i].priority,pq[j].priority; a!=b {
		return a > b
	}
	// more recent????
	if a,b := pq[i].timestamp,pq[j].timestamp; abs(a.Sub(b)) >= 10*time.Millisecond {
		return a.After(b)
	}
	// Larger.
	return pq[i].size > pq[j].size
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(x any) {
	n := len(*pq)
	pqEnv := x.(*pqEnvelope)
	pqEnv.index = n
	*pq = append(*pq, pqEnv)
}

func (pq *priorityQueue) Pop() any {
	old := *pq
	n := len(old)
	pqEnv := old[n-1]
	old[n-1] = nil
	pqEnv.index = -1
	*pq = old[:n-1]
	return pqEnv
}

// Assert the priority queue scheduler implements the queue interface at
// compile-time.
var _ queue = (*pqScheduler)(nil)

type pqScheduler struct {
	logger       log.Logger
	metrics      *Metrics
	lc           *metricsLabelCache
	size         uint
	sizes        map[uint]uint // cumulative priority sizes
	pq           *priorityQueue
	chDescs      []*ChannelDescriptor
	chPriorities map[ChannelID]uint

	enqueueCh chan Envelope
	dequeueCh chan Envelope
}

func newPQScheduler(
	logger log.Logger,
	m *Metrics,
	lc *metricsLabelCache,
	chDescs []*ChannelDescriptor,
	size uint,
) *pqScheduler {
	if size%2 != 0 { size++ }
	enqueueBuf := size/2
	dequeueBuf := size/2
	// copy each ChannelDescriptor and sort them by ascending channel priority
	chDescs = append([]*ChannelDescriptor(nil), chDescs...)
	sort.Slice(chDescs, func(i, j int) bool { return chDescs[i].Priority < chDescs[j].Priority })

	chPriorities := map[ChannelID]uint{}
	sizes := map[uint]uint{}

	for _, chDesc := range chDescs {
		chID := chDesc.ID
		chPriorities[chID] = uint(chDesc.Priority)
		sizes[uint(chDesc.Priority)] = 0
	}

	var pq priorityQueue
	heap.Init(&pq)

	return &pqScheduler{
		logger:       logger.With("router", "scheduler"),
		metrics:      m,
		lc:           lc,
		chDescs:      chDescs,
		chPriorities: chPriorities,
		pq:           &pq,
		sizes:        sizes,
		enqueueCh:    make(chan Envelope, enqueueBuf),
		dequeueCh:    make(chan Envelope, dequeueBuf),
	}
}

// start starts non-blocking process that starts the priority queue scheduler.
func (s *pqScheduler) enqueue() chan<- Envelope  { return s.enqueueCh }
func (s *pqScheduler) dequeue() <-chan Envelope  { return s.dequeueCh }

// process starts a block process where we listen for Envelopes to enqueue. If
// there is sufficient capacity, it will be enqueued into the priority queue,
// otherwise, we attempt to dequeue enough elements from the priority queue to
// make room for the incoming Envelope by dropping lower priority elements. If
// there isn't sufficient capacity at lower priorities for the incoming Envelope,
// it is dropped.
//
// After we attempt to enqueue the incoming Envelope, if the priority queue is
// non-empty, we pop the top Envelope and send it on the dequeueCh.
func (s *pqScheduler) run(ctx context.Context) error {
	for {
		e,err := utils.Recv(ctx, s.enqueueCh)
		if err != nil { return err }
		chIDStr := strconv.Itoa(int(e.ChannelID))
		pqEnv := &pqEnvelope{
			envelope:  e,
			size:      uint(proto.Size(e.Message)),
			priority:  s.chPriorities[e.ChannelID],
			timestamp: time.Now().UTC(),
		}

		// enqueue

		// Check if we have sufficient capacity to simply enqueue the incoming
		// Envelope.
		if s.size+pqEnv.size <= defaultCapacity {
			s.metrics.PeerPendingSendBytes.With("peer_id", string(pqEnv.envelope.To)).Add(float64(pqEnv.size))
			// enqueue the incoming Envelope
			s.push(pqEnv)
		} else {
			// There is not sufficient capacity to simply enqueue the incoming
			// Envelope. So we have to attempt to make room for it by dropping lower
			// priority Envelopes or drop the incoming Envelope otherwise.

			// The cumulative size of all enqueue envelopes at the incoming envelope's
			// priority or lower.
			total := s.sizes[pqEnv.priority]

			if total >= pqEnv.size {
				// There is room for the incoming Envelope, so we drop as many lower
				// priority Envelopes as we need to.
				canEnqueue := false
				tmpSize := s.size
				i := s.pq.Len() - 1

				// Drop lower priority Envelopes until sufficient capacity exists for
				// the incoming Envelope
				for i >= 0 && !canEnqueue {
					pqEnvTmp := s.pq.get(i)

					if pqEnvTmp.priority < pqEnv.priority {
						if tmpSize+pqEnv.size <= defaultCapacity {
							canEnqueue = true
						} else {
							pqEnvTmpChIDStr := strconv.Itoa(int(pqEnvTmp.envelope.ChannelID))
							s.metrics.PeerQueueDroppedMsgs.With("ch_id", pqEnvTmpChIDStr).Add(1)
							s.logger.Debug(
								"dropped envelope",
								"ch_id", pqEnvTmpChIDStr,
								"priority", pqEnvTmp.priority,
								"msg_size", pqEnvTmp.size,
								"capacity", defaultCapacity,
							)

							s.metrics.PeerPendingSendBytes.With("peer_id", string(pqEnvTmp.envelope.To)).Add(float64(-pqEnvTmp.size))

							// dequeue/drop from the priority queue
							heap.Remove(s.pq, pqEnvTmp.index)

							// update the size tracker
							tmpSize -= pqEnvTmp.size

							// start from the end again
							i = s.pq.Len() - 1
						}
					} else {
						i--
					}
				}

				// enqueue the incoming Envelope
				s.push(pqEnv)
			} else {
				// There is not sufficient capacity to drop lower priority Envelopes,
				// so we drop the incoming Envelope.
				s.metrics.PeerQueueDroppedMsgs.With("ch_id", chIDStr).Add(1)
				s.logger.Debug(
					"dropped envelope",
					"ch_id", chIDStr,
					"priority", pqEnv.priority,
					"msg_size", pqEnv.size,
					"capacity", defaultCapacity,
				)
			}
		}

		// dequeue

		for s.pq.Len() > 0 {
			pqEnv = heap.Pop(s.pq).(*pqEnvelope)
			s.size -= pqEnv.size

			// deduct the Envelope size from all the relevant cumulative sizes
			for i := 0; i < len(s.chDescs) && pqEnv.priority <= uint(s.chDescs[i].Priority); i++ {
				s.sizes[uint(s.chDescs[i].Priority)] -= pqEnv.size
			}

			s.metrics.PeerSendBytesTotal.With(
				"chID", chIDStr,
				"peer_id", string(pqEnv.envelope.To),
				"message_type", s.lc.ValueToMetricLabel(pqEnv.envelope.Message)).Add(float64(pqEnv.size))
			s.metrics.PeerPendingSendBytes.With(
				"peer_id", string(pqEnv.envelope.To)).Add(float64(-pqEnv.size))
			if err:=utils.Send(ctx, s.dequeueCh, pqEnv.envelope); err!=nil {
				return err
			}
		}
	}
}

func (s *pqScheduler) push(pqEnv *pqEnvelope) {
	// enqueue the incoming Envelope
	heap.Push(s.pq, pqEnv)
	s.size += pqEnv.size
	s.metrics.PeerQueueMsgSize.With("ch_id", strconv.Itoa(int(pqEnv.envelope.ChannelID))).Add(float64(pqEnv.size))

	// Update the cumulative sizes by adding the Envelope's size to every
	// priority less than or equal to it.
	for i := 0; i < len(s.chDescs) && pqEnv.priority <= uint(s.chDescs[i].Priority); i++ {
		s.sizes[uint(s.chDescs[i].Priority)] += pqEnv.size
	}
}
