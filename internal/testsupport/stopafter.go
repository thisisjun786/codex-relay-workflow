package testsupport

import (
	"context"
	"sync/atomic"
	"time"
)

// StopAfter is a context that reports itself ended once its Err has been asked n times. A test uses
// it to end a context at every boundary of code that checks it between its steps, one boundary per
// n, without sleeping or racing a timer. It carries a deadline an hour away, so code that reads
// differently under a bounded context (a hook's) takes that path, and its Done channel never
// closes: it serves code that asks Err, which is what the marker and manifest functions do.
func StopAfter(n int) context.Context {
	return &stopAfter{Context: context.Background(), allowed: int64(n), deadline: time.Now().Add(time.Hour)}
}

type stopAfter struct {
	context.Context
	asked, allowed int64
	deadline       time.Time
}

func (s *stopAfter) Deadline() (time.Time, bool) { return s.deadline, true }

func (s *stopAfter) Err() error {
	if atomic.AddInt64(&s.asked, 1) > s.allowed {
		return context.Canceled
	}
	return nil
}
