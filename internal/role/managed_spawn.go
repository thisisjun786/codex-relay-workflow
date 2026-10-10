package role

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"path/filepath"
	"regexp"
	"time"
)

// ManagedSpawnSelection ports managedSpawn's snapshot, without invoking a model. It also keeps what the preview resolved (the
// canonical dispatch root and the attempt it read), so the issuance works on the same root and attempt without resolving or
// reading them again outside its lock (CRW-1124).
type ManagedSpawnSelection struct {
	Candidate DispatchCandidate
	Role      RoleName
	root      string
	session   string
	dispatch  string
	attempt   string
}

// The oracle's multiline JS regexp admits CR, LF, LS and PS line boundaries.
func managedSpawnMarker(message string) []string {
	return regexp.MustCompile(`(?:^|[\r\n\x{2028}\x{2029}])\[CRW-DISPATCH:([a-zA-Z0-9_-]+):([a-zA-Z0-9_-]+)\](?:\r?\n|$|[\r\x{2028}\x{2029}])`).FindStringSubmatch(message)
}

// managedSpawnCarries reports whether message carries the marker of this attempt of this dispatch, anywhere in it: a role's
// prompt can be put before the work message, so other markers may come first.
func managedSpawnCarries(message, dispatch, attempt string) bool {
	return regexp.MustCompile(`(?:^|[\r\n\x{2028}\x{2029}])\[CRW-DISPATCH:` + regexp.QuoteMeta(dispatch) + `:` + regexp.QuoteMeta(attempt) + `\](?:\r?\n|$|[\r\x{2028}\x{2029}])`).MatchString(message)
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
	return &ManagedSpawnSelection{Candidate: a.Candidate, Role: d.Role, root: root, session: session, dispatch: match[1], attempt: match[2]}, nil
}

// IssueManagedSpawn consumes issuance under the ledger lock (oracle:250-267).
// Repeated hook delivery may reuse only the same nonempty host tool-use ID.
func IssueManagedSpawn(cwd, session, message string, toolUseID *string) (*ManagedSpawnSelection, error) {
	return IssueManagedSpawnEnv(cwd, session, message, toolUseID, nil)
}

// managedSpawnLookupBudget bounds the issuance's look at the host's marked children. It runs under the record's lock inside the
// installed hook's own limit (10 seconds for the whole process), so it is kept to a third of that: when it runs out the
// issuance is still saved and the lock released, with the children seen so far unrecorded.
var managedSpawnLookupBudget = 3 * time.Second

// IssueManagedSpawnEnv is IssueManagedSpawn that also records which children the host already shows with the attempt's marker
// (see DispatchAttempt.PriorChildren), reading the native thread database through env. A nil env, a host without a thread
// database and one that cannot be read record nothing; that only removes an early refusal, because the created check ties a
// child to the issued call by the host's result of the call alone.
func IssueManagedSpawnEnv(cwd, session, message string, toolUseID *string, env host.LookupEnv) (*ManagedSpawnSelection, error) {
	resolved, err := ManagedSpawn(cwd, session, message)
	if err != nil || resolved == nil {
		return nil, err
	}
	return IssueManagedSpawnSelection(resolved, toolUseID, env)
}

// IssueManagedSpawnSelection issues the attempt a preview (ManagedSpawn) selected: the record is read again under its lock, through
// the directory pinned below the preview's canonical root, and the attempt must still be the record's current, claimed attempt of an
// active dispatch with the role and the candidate the preview saw, not yet issued or issued to this same native call. The root is
// not resolved again and the record is not read again outside the lock (CRW-1124), so a hook event resolves the root once and the
// candidate it answers with is the one the issuance checked.
func IssueManagedSpawnSelection(sel *ManagedSpawnSelection, toolUseID *string, env host.LookupEnv) (*ManagedSpawnSelection, error) {
	if sel == nil || sel.root == "" {
		return nil, errors.New("managed spawn issuance needs a preview of the attempt")
	}
	dir, err := dispatchDirectory(sel.root, sel.session, nil)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name := sel.dispatch + ".json"
	release, err := dir.lock(name)
	if err != nil {
		return nil, err
	}
	defer release()
	d, err := dispatchPinnedRead(dir, name, sel.session, sel.dispatch)
	if err != nil {
		return nil, err
	}
	a := &d.Attempts[len(d.Attempts)-1]
	if a.ID != sel.attempt || !a.Claimed || !dispatchIs(a.Status, "claimed") || !dispatchIs(d.Status, "active") ||
		d.Role != sel.Role || !managedSpawnSameCandidate(a.Candidate, sel.Candidate) {
		return nil, errors.New("managed attempt changed before issuance")
	}
	if a.SpawnIssued && (toolUseID == nil || *toolUseID == "" || a.ToolUseID == nil || *a.ToolUseID != *toolUseID) {
		return nil, errors.New("attempt already issued to another native call; reconcile before retry")
	}
	if !a.SpawnIssued && env != nil {
		ctx, cancel := context.WithTimeout(context.Background(), managedSpawnLookupBudget)
		prior, err := createdCheckMarked(ctx, env, sel.session, d.ID, a.ID)
		cancel()
		if err == nil && len(prior) > 0 {
			for _, child := range prior {
				a.PriorChildren = append(a.PriorChildren, child.ID)
			}
			a.raw.set("priorChildren", a.PriorChildren)
		}
	}
	a.SpawnIssued, a.ToolUseID = true, toolUseID
	// The ledger marshaler overlays only the fields its own operations mutate.
	a.raw.set("spawnIssued", true)
	a.raw.set("toolUseId", toolUseID)
	if err := dispatchSave(dir, name, &d, nil); err != nil {
		return nil, err
	}
	return sel, nil
}

// managedSpawnSameCandidate reports whether two candidates name the same model and effort (null alike).
func managedSpawnSameCandidate(a, b DispatchCandidate) bool {
	same := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	ea, eb := (*string)(nil), (*string)(nil)
	if a.Effort != nil {
		v := string(*a.Effort)
		ea = &v
	}
	if b.Effort != nil {
		v := string(*b.Effort)
		eb = &v
	}
	return same(a.Model, b.Model) && same(ea, eb)
}
