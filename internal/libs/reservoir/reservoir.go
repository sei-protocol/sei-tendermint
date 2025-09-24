package reservoir

import (
	"cmp"
	crand "crypto/rand"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
)

// Sampler maintains a thread-safe reservoir of size k for ordered items of
// type T, allowing random sampling from a stream of unknown length and
// percentile queries on the current samples.
//
// It uses Vitter's Algorithm R for reservoir sampling (see
// https://en.wikipedia.org/wiki/Reservoir_sampling#Algorithm_R).
//
// The zero value is not usable; use New to create a Sampler.
type Sampler[T cmp.Ordered] struct {
	size    int
	samples []T
	seen    int64
	mu      sync.Mutex
	rng     *rand.Rand
}

// New creates a new Sampler with the given reservoir size.
//
// The rng parameter is optional; if nil, a non-deterministic random number
// generator will be used. Note that the size cannot be less than or equal to
// zero. Otherwise, New will panic.
func New[T cmp.Ordered](size int, rng *rand.Rand) *Sampler[T] {
	if size <= 0 {
		panic("reservoir size must be greater than zero")
	}
	if rng == nil {
		rng = rand.New(rand.NewPCG(nonDeterministicSeed()))
	}
	return &Sampler[T]{
		size:    size,
		samples: make([]T, 0, size),
		rng:     rng,
	}
}

// Add inserts an item into the reservoir with correct probability.
func (s *Sampler[T]) Add(item T) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seen++
	if len(s.samples) < s.size {
		s.samples = append(s.samples, item)
		return
	}
	if j := s.rng.Int64N(s.seen); int(j) < s.size {
		s.samples[j] = item
	}
}

// Seen returns the number of items observed so far.
func (s *Sampler[T]) Seen() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

// Percentile returns the value at percentile p from the reservoir's contents,
// using the "nearest-rank" definition on a sorted copy of the current samples.
//
// p is in [0.0, 1.0]. Values outside are clamped. The ordering is provided by
// the caller via less(a, b) -> true if a < b.
//
// It returns (zero, false) if the reservoir is empty.
func (s *Sampler[T]) Percentile(p float64) (T, bool) {

	// We can use various strategies to cache the result here, e.g. caching the
	// sorted copy and invalidating it on Add. However, this adds complexity and
	// memory usage, and Percentile is not expected to be called very often.
	// Therefore, we keep it simple for now and just sort a fresh copy each time.
	//
	// If this becomes a bottleneck, we can revisit this decision.

	s.mu.Lock()
	defer s.mu.Unlock()

	var zero T
	n := len(s.samples)
	if n == 0 {
		return zero, false
	}

	p = min(max(p, 0.0), 1.0) // Clamp p to [0.0, 1.0].
	tmp := make([]T, n)
	copy(tmp, s.samples)
	slices.Sort(tmp)

	var index int
	if p == 0 {
		index = 0
	} else {
		index = int(math.Ceil(p*float64(n))) - 1
		index = min(max(index, 0), n-1) // Clamp index to [0, n-1].
	}

	return tmp[index], true
}

func nonDeterministicSeed() (uint64, uint64) {
	var buf [16]byte
	// Ignore error and length, because crypto/rand.Read never returns an error and
	// always fills the given buffer entirely as stated in its godoc.
	_, _ = crand.Read(buf[:])
	return binary.LittleEndian.Uint64(buf[:8]), binary.LittleEndian.Uint64(buf[8:])
}
