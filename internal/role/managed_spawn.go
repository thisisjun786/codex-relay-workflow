package role

type ManagedSpawnSelection struct {
	Candidate DispatchCandidate
	Role      RoleName
}

func ManagedSpawn(cwd, session, message string) (*ManagedSpawnSelection, error) { return nil, nil }
func IssueManagedSpawn(cwd, session, message string, toolUseID *string) (*ManagedSpawnSelection, error) {
	return nil, nil
}
