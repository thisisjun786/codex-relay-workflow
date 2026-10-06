package role

import (
	"errors"
	"path/filepath"
	"regexp"
)

// ManagedSpawnSelection ports managedSpawn's snapshot, without invoking a model.
type ManagedSpawnSelection struct {
	Candidate DispatchCandidate
	Role      RoleName
}

// The oracle's multiline JS regexp admits CR, LF, LS and PS line boundaries.
func managedSpawnMarker(message string) []string {
	return regexp.MustCompile(`(?:^|[\r\n\x{2028}\x{2029}])\[CRW-DISPATCH:([a-zA-Z0-9_-]+):([a-zA-Z0-9_-]+)\](?:\r?\n|$|[\r\x{2028}\x{2029}])`).FindStringSubmatch(message)
}

// ManagedSpawn ports fallback-dispatch.ts:237-247 after name substitution.
func ManagedSpawn(cwd, session, message string) (*ManagedSpawnSelection, error) {
	root, err := dispatchRoot(cwd)
	if err != nil {
		return nil, err
	}
	match := managedSpawnMarker(message)
	if match == nil {
		return nil, nil
	}
	for _, field := range []struct{ value, name string }{{session, "sessionId"}, {match[1], "dispatchId"}, {match[2], "attemptId"}} {
		if _, err := dispatchID(field.value, field.name); err != nil {
			return nil, err
		}
	}
	d, err := dispatchRead(filepath.Join(root, ".crw", "dispatches", session, match[1]+".json"), session, match[1])
	if err != nil {
		return nil, err
	}
	a := d.Attempts[len(d.Attempts)-1]
	if a.ID != match[2] || !a.Claimed || !dispatchIs(a.Status, "claimed") || !dispatchIs(d.Status, "active") {
		return nil, errors.New("managed spawn attempt is not claimed or no longer current")
	}
	return &ManagedSpawnSelection{Candidate: a.Candidate, Role: d.Role}, nil
}

// IssueManagedSpawn consumes issuance under the ledger lock (oracle:250-267).
// Repeated hook delivery may reuse only the same nonempty host tool-use ID.
func IssueManagedSpawn(cwd, session, message string, toolUseID *string) (*ManagedSpawnSelection, error) {
	root, err := dispatchRoot(cwd)
	if err != nil {
		return nil, err
	}
	match := managedSpawnMarker(message)
	if match == nil {
		return nil, nil
	}
	resolved, err := ManagedSpawn(root, session, message)
	if err != nil {
		return nil, err
	}
	dir, err := dispatchDirectory(root, session, nil)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name := match[1] + ".json"
	release, err := dir.lock(name)
	if err != nil {
		return nil, err
	}
	defer release()
	d, err := dispatchPinnedRead(dir, name, session, match[1])
	if err != nil {
		return nil, err
	}
	a := &d.Attempts[len(d.Attempts)-1]
	if a.ID != match[2] || !a.Claimed || !dispatchIs(a.Status, "claimed") || !dispatchIs(d.Status, "active") {
		return nil, errors.New("managed attempt changed before issuance")
	}
	if a.SpawnIssued && (toolUseID == nil || *toolUseID == "" || a.ToolUseID == nil || *a.ToolUseID != *toolUseID) {
		return nil, errors.New("attempt already issued to another native call; reconcile before retry")
	}
	a.SpawnIssued, a.ToolUseID = true, toolUseID
	// The ledger marshaler overlays only the fields its own operations mutate.
	a.raw.set("spawnIssued", true)
	a.raw.set("toolUseId", toolUseID)
	if err := dispatchSave(dir, name, &d, nil); err != nil {
		return nil, err
	}
	return resolved, nil
}
