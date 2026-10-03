package state

import "time"

func ReadState(cwd, sessionID string) State { return State{} }

func ReadStateStrict(cwd, sessionID string) (State, bool) { return State{}, true }

func restore(sessionID string, raw []byte, now time.Time) (State, bool) { return State{}, true }

func ReconstructUnverified(raw any) (entries []UnverifiedSubagent, corrupt bool) { return nil, false }
