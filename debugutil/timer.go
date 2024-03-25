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
