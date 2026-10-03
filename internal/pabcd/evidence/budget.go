package evidence

// HasSpentBudget is a stub.
func HasSpentBudget(cwd, sessionID string) bool { return false }

// ResolveTombstone is a stub.
func ResolveTombstone(cwd, sessionID string, p Payload) bool {
	return resolveTombstone(cwd, sessionID, p, nil)
}

func resolveTombstone(cwd, sessionID string, p Payload, lock lockFunc) bool { return false }
