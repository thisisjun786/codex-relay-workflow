//go:build dev

package cxcfuzz

import (
	"strings"
	"testing"
	"time"
)

// A written document's updatedAt is masked only when the write made it: an instant inside the clock window
// taken around the write. A stamp the write kept from the file is outside that window and is compared as
// stored, so a port that keeps a stale updatedAt is not masked into a Same as an oracle that restamps
// (CRW-978 c3c). The window is the write's own clock, so the two sides' clocks are each judged by their own.
func TestWrittenStampIsMaskedOnlyInsideTheWriteWindow(t *testing.T) {
	from := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Second)
	fresh := `{"objective": "o", "updatedAt": "2026-10-08T12:00:00.500Z"}`
	if got := maskWrittenStamp(fresh, from, to); !strings.Contains(got, timestampPlaceholder) {
		t.Fatalf("a stamp inside the write window was not masked: %s", got)
	}
	stale := `{"objective": "o", "updatedAt": "2020-01-01T00:00:00.000Z"}`
	if got := maskWrittenStamp(stale, from, to); got != stale {
		t.Fatalf("a stamp outside the write window was masked: %s", got)
	}
}

// The window is inclusive at the millisecond the write began and ended.
func TestWrittenStampWindowIncludesItsEdges(t *testing.T) {
	from := time.Date(2026, 10, 8, 12, 0, 0, 250_000_000, time.UTC)
	to := from.Add(10 * time.Millisecond)
	for _, stamp := range []string{"2026-10-08T12:00:00.250Z", "2026-10-08T12:00:00.260Z"} {
		text := `{"updatedAt": "` + stamp + `"}`
		if got := maskWrittenStamp(text, from, to); !strings.Contains(got, timestampPlaceholder) {
			t.Fatalf("the edge stamp %s was not masked: %s", stamp, got)
		}
	}
}
