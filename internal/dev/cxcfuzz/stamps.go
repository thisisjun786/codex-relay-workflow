//go:build dev

package cxcfuzz

import "time"

// writtenStampLayout is the layout of a wall-clock stamp: an ISO-8601 instant with milliseconds, UTC.
const writtenStampLayout = "2006-01-02T15:04:05.000Z"

// maskWrittenStamp masks a published document's top-level updatedAt only when this write made it: an
// instant between from and to, the clock readings taken around the write (CRW-978 c3c). The writer restamps
// updatedAt from its own clock, so its stamp always falls inside that window. A stamp outside the window
// was kept from the file rather than made by the write, and stays as stored. Masking every stamp by shape
// would hide such a kept value. Each side is judged against its own clock, where that side read it.
func maskWrittenStamp(text string, from, to time.Time) string {
	lo := from.Truncate(time.Millisecond)
	hi := to.Add(time.Millisecond)
	return maskTopLevelTimestamp(text, func(stamp string) bool {
		at, err := time.Parse(writtenStampLayout, stamp)
		return err == nil && !at.Before(lo) && !at.After(hi)
	})
}

// anyStamp accepts every stamp, for the read forms whose stamp the reader defaulted from the clock.
func anyStamp(string) bool { return true }
