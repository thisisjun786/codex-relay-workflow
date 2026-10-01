//go:build dev

package trialledger

import "time"

// parseMoment reads text as an RFC 3339 date and time. aware is false for a date, or a date and
// time, that names no UTC offset: such a text is a time, but not a moment the ledger can place.
func parseMoment(text string) (at time.Time, aware bool, err error) {
	at, err = time.Parse(time.RFC3339Nano, text)
	if err == nil {
		return at, true, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"} {
		if _, naive := time.Parse(layout, text); naive == nil {
			return time.Time{}, false, nil
		}
	}
	return time.Time{}, false, err
}
