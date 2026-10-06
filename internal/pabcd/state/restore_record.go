package state

// restore_record.go exposes the state reader's own verdict on a session record to a caller that already holds the
// bytes: the state-copy preflight of internal/runtime/install/migrate, which has to refuse a retained session file
// exactly when this package would call it unreadable. Nothing here reads or writes a file.

import "time"

// RecordUnreadable reports whether the bytes of a session record are unreadable, with the verdict ReadStateStrict
// reaches after reading the same file: a record that is not a JSON object, or whose phase is not one of the known
// phases, is unreadable. sessionID is the record's own session (the file name without ".json"); restore only uses it
// to stamp the defaults this call discards, so the answer does not depend on the clock.
func RecordUnreadable(sessionID string, raw []byte) bool {
	_, unreadable := restore(sessionID, raw, time.Now())
	return unreadable
}
