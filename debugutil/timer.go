package debugutil

import (
	"fmt"
	"time"
)

type Timer struct {
	name      string
	startTime time.Time
}

func NewTimer(name string) *Timer {
	return &Timer{
		name:      name,
		startTime: time.Now(),
	}
}

func (t *Timer) Stop() {
	fmt.Printf("[Debug] %s took %s\n", t.name, time.Since(t.startTime))
}

func PrintStats(msg string, durations []time.Duration) {
	if len(durations) == 0 {
		fmt.Printf("[Debug] %s count=0 (no stats)\n", msg)
		return
	}
	var sum time.Duration
	var max time.Duration
	var min time.Duration
	for i, d := range durations {
		sum += d
		if d > max {
			max = d
		}
		if i == 0 || d < min {
			min = d
		}
	}
	avg := sum / time.Duration(len(durations))
	// only show the slow ones
	if sum > 500*time.Millisecond {
		fmt.Printf("[Debug] %s count=%d sum=%s avg=%s max=%s min=%s\n", msg, len(durations), sum, avg, max, min)
	}
}
