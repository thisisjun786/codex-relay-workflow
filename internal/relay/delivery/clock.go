package delivery

import (
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Clock is the Python clock: seconds as a float, and an isoformat stamp at microseconds.
type Clock interface {
	Now() float64
	ISO() string
}

// ISOOf is datetime.fromtimestamp(now, utc).isoformat(timespec="microseconds").
func ISOOf(now float64) string { return store.SendStamp(now) }

// SystemClock reads the wall clock.
type SystemClock struct{}

var cliClock Clock = SystemClock{}

func (SystemClock) Now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }
func (SystemClock) ISO() string  { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

// FakeClock is a clock tests move by hand, starting at Python FakeClock's 1_700_000_000.
type FakeClock struct {
	T float64
	// OnISO, when set, runs before every ISO read (tests that make each read differ).
	OnISO func()
}

func NewFakeClock() *FakeClock { return &FakeClock{T: 1_700_000_000} }

func (c *FakeClock) Now() float64 { return c.T }
func (c *FakeClock) Advance(seconds float64) float64 {
	c.T += seconds
	return c.T
}
func (c *FakeClock) ISO() string {
	if c.OnISO != nil {
		c.OnISO()
	}
	return ISOOf(c.T)
}
