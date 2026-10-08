//go:build dev

package cxcfuzz

import "time"

// testWrittenStamp masks a published document the way a write made in 2026 does: its stamp lies in the
// clock window the writer read around that write (CRW-978 c3c). The answer helpers use it for "written".
func testWrittenStamp(text string) string {
	return maskWrittenStamp(text, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
}
