package reservoir

import (
	"cmp"
	crand "crypto/rand"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// lastPercentileCacheTTL is the duration for which a cached percentile value is
// considered valid if no new percentile p is asked for.
const lastPercentileCacheTTL = 5 * time.Second

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

	// Caching of the last calculated percentile and its value.
	// See Percentile() for details.
	lastP         float64   // last requested percentile
	lastVal       T         // last sample at percentile lastP
	lastCalc      time.Time // zero if never calculated
	dirtySinceAdd bool      // true if Add() happened after the last calculation
}

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

	s.dirtySinceAdd = true
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

// Percentile returns the nearest-rank percentile value.
// Recalculation rules:
//   - If p changed since last call: recompute immediately.
//   - If new data arrived since last calc: recompute only if >= 5s have passed since last calc.
//   - Otherwise, return cached value.
func (s *Sampler[T]) Percentile(p float64) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var zero T
	n := len(s.samples)
	if n == 0 {
		return zero, false
	}

	// If we have a cached value and p is unchanged:
	cachePresent := !s.lastCalc.IsZero()
	if cachePresent && p == s.lastP {
		if time.Since(s.lastCalc) < lastPercentileCacheTTL {
			return s.lastVal, true
		}
		if !s.dirtySinceAdd {
			// No new data, cached value is valid.
			return s.lastVal, true
		}
	}

	// Compute nearest-rank percentile.
	clampedP := min(max(p, 0.0), 1.0) // Clamp p to [0.0, 1.0] while keeping the original p for caching.
	tmp := make([]T, n)
	copy(tmp, s.samples)
	slices.Sort(tmp)

	var index int
	if clampedP == 0 {
		index = 0
	} else {
		index = int(math.Ceil(clampedP*float64(n))) - 1
		index = min(max(index, 0), n-1) // Clamp index to [0, n-1].
	}
	val := tmp[index]

	s.lastP = p
	s.lastVal = val
	s.lastCalc = time.Now()
	s.dirtySinceAdd = false
	return val, true
}

func nonDeterministicSeed() (uint64, uint64) {
	var buf [16]byte
	_, _ = crand.Read(buf[:])
	return binary.LittleEndian.Uint64(buf[:8]), binary.LittleEndian.Uint64(buf[8:])
}
