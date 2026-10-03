package evidence

import "time"

// UnrecordableSubdir is the directory, under the state directory, of the markers that say a verdict existed and could not be recorded.
const UnrecordableSubdir = "evidence-unrecordable"

// VerdictStatus is what UnrecordableVerdictStatus found.
type VerdictStatus struct{ Present, Unreadable bool }

// WriteUnrecordableMarker is a stub.
func WriteUnrecordableMarker(cwd, sessionID, agentID string) error { return nil }

func writeUnrecordableMarker(cwd, sessionID, agentID string, now time.Time) error { return nil }

// UnrecordableVerdictStatus is a stub.
func UnrecordableVerdictStatus(cwd, sessionID string) VerdictStatus { return VerdictStatus{} }
