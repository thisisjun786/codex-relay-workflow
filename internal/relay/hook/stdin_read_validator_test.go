package hook

import (
	"context"
	"errors"
	"testing"
	"time"
)

func stdinUnreadableRow(detail string, reading Object) Object {
	return Object{{Key: "adapterOutcome", Value: "stdin_unreadable"}, {Key: "detail", Value: detail}, {Key: "stdinRead", Value: reading}}
}

func reading(cause string, bytes int64) Object {
	return Object{{Key: "cause", Value: cause}, {Key: "error", Value: "e"}, {Key: "bytesRead", Value: bytes}, {Key: "waitStartedMs", Value: int64(0)}, {Key: "waitEndedMs", Value: int64(100)}}
}

func TestStdinReadRecorded(t *testing.T) {
	utf8Detail := StdinNotUTF8Prefix + "x"
	for name, c := range map[string]struct {
		row  Object
		want bool
	}{
		"a legacy row without the key": {Object{{Key: "adapterOutcome", Value: "stdin_unreadable"}, {Key: "detail", Value: StdinUnreadableDetail}}, true},
		"input_late":                   {stdinUnreadableRow(StdinUnreadableDetail, reading(StdinInputLate, 0)), true},
		"work_ended":                   {stdinUnreadableRow(StdinUnreadableDetail, reading(StdinWorkEnded, 0)), true},
		"read_error with bytes":        {stdinUnreadableRow(StdinUnreadableDetail, reading(StdinReadError, 7)), true},
		"invalid_utf8":                 {stdinUnreadableRow(utf8Detail, reading(StdinInvalidUTF8, 1)), true},
		"invalid_utf8 with no bytes":   {stdinUnreadableRow(utf8Detail, reading(StdinInvalidUTF8, 0)), false},
		"invalid_utf8, generic detail": {stdinUnreadableRow(StdinUnreadableDetail, reading(StdinInvalidUTF8, 1)), false},
		"input_late, UTF-8 detail":     {stdinUnreadableRow(utf8Detail, reading(StdinInputLate, 0)), false},
		"unknown cause":                {stdinUnreadableRow(StdinUnreadableDetail, reading("timeout", 0)), false},
		"key on another outcome":       {Object{{Key: "adapterOutcome", Value: "stdin_not_json"}, {Key: "detail", Value: StdinUnreadableDetail}, {Key: "stdinRead", Value: reading(StdinReadError, 0)}}, false},
		"six keys":                     {stdinUnreadableRow(StdinUnreadableDetail, append(reading(StdinReadError, 0), Object{{Key: "payload", Value: "x"}}...)), false},
		"a number as the error":        {stdinUnreadableRow(StdinUnreadableDetail, reading(StdinReadError, 0).Set("error", int64(1))), false},
		"negative bytes":               {stdinUnreadableRow(StdinUnreadableDetail, reading(StdinReadError, -1)), false},
		"a null key":                   {Object{{Key: "adapterOutcome", Value: "stdin_unreadable"}, {Key: "detail", Value: StdinUnreadableDetail}, {Key: "stdinRead", Value: nil}}, false},
	} {
		if got := StdinReadRecorded(c.row); got != c.want {
			t.Errorf("%s: StdinReadRecorded = %v, want %v", name, got, c.want)
		}
	}
}

// The cause is the context that ended FIRST, read from the context itself, so a work context that
// ends after the input deadline fired cannot turn input_late into work_ended.
func TestNamedKeepsTheContextThatEndedFirst(t *testing.T) {
	cause := func(err error) string {
		t.Helper()
		var failure *readFailure
		if !errors.As(err, &failure) {
			t.Fatalf("not a read failure: %v", err)
		}
		return failure.cause
	}
	t.Run("input deadline first, work ends later", func(t *testing.T) {
		work, endWork := context.WithCancel(context.Background())
		input, cancel := context.WithDeadlineCause(work, time.Now().Add(-time.Second), errInputLate)
		defer cancel()
		endWork()
		if got := cause(named(input.Err(), input)); got != StdinInputLate {
			t.Fatalf("cause = %s", got)
		}
	})
	t.Run("work ends first", func(t *testing.T) {
		work, endWork := context.WithCancel(context.Background())
		input, cancel := context.WithDeadlineCause(work, time.Now().Add(time.Hour), errInputLate)
		defer cancel()
		endWork()
		if got := cause(named(input.Err(), input)); got != StdinWorkEnded {
			t.Fatalf("cause = %s", got)
		}
	})
	t.Run("the descriptor path watches work alone", func(t *testing.T) {
		work, endWork := context.WithTimeout(context.Background(), time.Nanosecond)
		defer endWork()
		<-work.Done()
		if got := cause(named(work.Err(), work)); got != StdinWorkEnded {
			t.Fatalf("cause = %s", got)
		}
	})
	t.Run("a failure that already has its cause keeps it", func(t *testing.T) {
		work, endWork := context.WithCancel(context.Background())
		endWork()
		tagged := &readFailure{StdinReadError, context.DeadlineExceeded}
		if got := named(tagged, work); got != error(tagged) {
			t.Fatalf("retagged: %v", got)
		}
	})
	t.Run("a recovered panic and any other error are read errors", func(t *testing.T) {
		work := context.Background()
		for _, err := range []error{&recoveredPanic{value: "x"}, errors.New("boom")} {
			if got := cause(named(err, work)); got != StdinReadError {
				t.Fatalf("cause = %s for %v", got, err)
			}
		}
		if named(nil, work) != nil {
			t.Fatal("no error stays none")
		}
	})
}
