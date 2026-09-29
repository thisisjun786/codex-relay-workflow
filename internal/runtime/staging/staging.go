// Package staging is scripts/crw_runtime/staging.py: the claim a run leaves in the directory
// it is building, and who may remove it.
//
// Two files answer two questions. The LOCK file (.crw-staging-lock) answers "is anybody still
// building this": an flock(2) on a file created once and never replaced, because a lock
// belongs to an inode and a rename over the file would leave it on an unlinked one. The CLAIM
// file (.crw-staging-claim.json) answers "what did that run say it was doing"; it is rewritten
// when the staging settles, which is why it cannot also be the lock. Liveness is the lock and
// never a recorded pid. Removing anything requires a claim this command wrote.
//
// A Go install stages into a temporary sibling (.<name>.tmp-<random>) and renames it to its
// final name once it is complete, so a final-named directory is complete by construction; the
// claim and lock travel with the directory. The RECORDED decision of staging.py (a directory
// made before claims existed) is not ported: every Python env-* directory on the one host that
// predates claims now carries one.
package staging

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// ClaimName and LockName are the two files a staging run leaves.
const (
	ClaimName = ".crw-staging-claim.json"
	LockName  = ".crw-staging-lock"
)

// ClaimVersion is the only claimVersion a claim may declare.
const ClaimVersion = 1

// The markers that make a claim THIS command's: the Python installer's, and the Go
// installer's. Any other writtenBy is somebody else's file at that path.
const (
	WrittenByPython = "runtime_install.py"
	WrittenByGo     = "crw install"
)

// What a claim says about the run that wrote it.
const (
	Staging  = "STAGING"
	Complete = "COMPLETE"
)

// Whether anybody still holds the lock; UNKNOWN is never read as DEAD.
const (
	Live    = "LIVE"
	Dead    = "DEAD"
	Unknown = "UNKNOWN"
)

// What a reading of an existing directory decides.
const (
	Reclaim  = "RECLAIM"
	Adopt    = "ADOPT"
	Resume   = "RESUME"
	Occupied = "OCCUPIED"
	Foreign  = "FOREIGN"
	Keep     = "KEEP"
	Settled  = "SETTLED"
)

// Decisions is every decision Decide can make.
var Decisions = []string{Reclaim, Adopt, Resume, Occupied, Foreign, Keep, Settled}

// Removes is the only decision that deletes anything.
func Removes(decision string) bool { return decision == Reclaim }

// ClaimPath and LockPath name the two files in a directory.
func ClaimPath(directory string) string { return filepath.Join(directory, ClaimName) }
func LockPath(directory string) string  { return filepath.Join(directory, LockName) }

// Payload is staging.claim_payload with every provenance value supplied, so its bytes are
// reproducible. Only "state" decides anything.
func Payload(state, writtenBy string, issue, run any, pid int, host, writtenAt string) record.Object {
	return record.Object{
		{Key: "claimVersion", Value: int64(ClaimVersion)},
		{Key: "state", Value: state},
		{Key: "writtenBy", Value: writtenBy},
		{Key: "issue", Value: issue},
		{Key: "run", Value: run},
		{Key: "pid", Value: int64(pid)},
		{Key: "host", Value: host},
		{Key: "writtenAt", Value: writtenAt},
		{Key: "livenessNote", Value: "the pid and host are provenance for a report, not a liveness test. Whether a run is still building is decided by the advisory lock on " + LockName + " alone."},
	}
}

// Shape is staging.shape: reject anything that is not a claim this command wrote.
func Shape(v any) error {
	claim, ok := v.(record.Object)
	if !ok {
		return reading.Fail("TypeError", "a staging claim is an object, found "+typeName(v))
	}
	writer := record.Get(claim, "writtenBy")
	if writer != WrittenByPython && writer != WrittenByGo {
		return reading.Fail("ValueError", "this claim was not written by "+WrittenByPython+" or "+WrittenByGo+", it names "+repr(writer))
	}
	version := record.Get(claim, "claimVersion")
	if n, ok := version.(int64); !ok || n != ClaimVersion {
		return reading.Fail("ValueError", "a claim declares claimVersion 1, found "+repr(version))
	}
	if state := record.Get(claim, "state"); state != Staging && state != Complete {
		return reading.Fail("ValueError", "a staging claim's state is one of STAGING, COMPLETE, found "+repr(state))
	}
	return nil
}

// repr is Python's repr() of a decoded JSON scalar, for refusal text.
func repr(v any) string {
	switch value := v.(type) {
	case nil:
		return "None"
	case string:
		return store.PythonRepr(value)
	case bool:
		if value {
			return "True"
		}
		return "False"
	case float64:
		return evidence.Float(value)
	case int64:
		return strconv.FormatInt(value, 10)
	}
	return evidence.Dumps(v, false, false, false)
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case float64:
		return "float"
	}
	return "int"
}

// ReadClaim is staging.read_claim: absent, present, unreadable and unreachable stay four
// answers, and an absent claim carries nil.
func ReadClaim(directory string) reading.Reading {
	return reading.ReadJSON(ClaimPath(directory), "the staging claim", nil, Shape)
}

// WriteClaim is staging.write_claim: the claim's bytes under the claim's .crw-lock, replaced
// by rename (which is why it is not the lock file).
func WriteClaim(directory string, payload record.Object) error {
	target := ClaimPath(directory)
	lock, err := record.Lock(target, 0)
	if err != nil {
		return err
	}
	defer lock.Release()
	return record.AtomicWrite(target, record.Encode(payload))
}

// NewPayload is a claim written now by the Go installer.
func NewPayload(state string, issue, run any) record.Object {
	host, _ := os.Hostname()
	return Payload(state, WrittenByGo, issue, run, os.Getpid(), host, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
}

// Held is staging.Held: the advisory lock a run holds while building, on a file nothing
// rewrites. Taking it never waits.
type Held struct {
	Path   string
	handle *os.File
}

// Take acquires the lock and keeps it until Release or process exit.
func Take(directory string) (*Held, error) {
	path := LockPath(directory)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return nil, err
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(handle.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return &Held{Path: path, handle: handle}, nil
}

// Release unlocks and closes; the lock file stays.
func (h *Held) Release() {
	if h == nil || h.handle == nil {
		return
	}
	_ = unix.Flock(int(h.handle.Fd()), unix.LOCK_UN)
	_ = h.handle.Close()
	h.handle = nil
}

// OwnerLiveness is staging.owner_liveness: take the lock and release it at once. DEAD only
// when the lock was actually free (or no run ever took it); UNKNOWN when it could not be put.
func OwnerLiveness(directory string) (string, string) {
	switch state, detail := record.Probe(LockPath(directory)); state {
	case record.NoFile:
		return Dead, "no run has taken the lock on this staging; there is nothing holding it"
	case record.Held:
		return Live, "another run holds the advisory lock on this staging"
	case record.Free:
		return Dead, "nothing holds the advisory lock on this staging"
	default:
		return Unknown, detail
	}
}

// DirectoryOccupied is staging.directory_occupied: whether the directory holds anything
// besides this command's two files. nil when it could not be listed.
func DirectoryOccupied(directory string) (*bool, string) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, "the directory could not be listed: " + store.PythonOSError(err)
	}
	other := 0
	for _, entry := range entries {
		if entry.Name() != ClaimName && entry.Name() != LockName {
			other++
		}
	}
	occupied := other > 0
	if occupied {
		return &occupied, "it holds " + strconv.Itoa(other) + " entries besides this command's own"
	}
	return &occupied, "it holds nothing besides this command's own files"
}

// Decide is staging.decide without RECORDED: what may be done with a directory that already
// exists. Each argument is one reading's answer; occupied is nil when the listing failed.
// protected is conservative (true when either the record or the pointer names the directory,
// or when either reading failed); selected is true only when the record was read and names it.
func Decide(claim reading.Reading, liveness string, occupied *bool, protected, selected bool) (string, string) {
	if claim.State == reading.Absent {
		switch {
		case liveness == Live:
			return Occupied, "another run holds the staging lock here and has not written its claim yet, so this directory is being taken, not free"
		case liveness == Unknown:
			return Keep, "there is no claim here and whether a run holds the staging lock could not be established"
		case occupied == nil:
			return Keep, "there is no claim here and the directory could not be listed, so whether it holds anything could not be established"
		case *occupied && selected:
			// staging.py's RECORDED branch adopted this as a pre-claim installation; it is retired,
			// so a directory without a claim is never taken as this command's own.
			return Foreign, "this directory holds files and carries no claim from this command. The host record selects something inside it, but a directory without a claim is not adopted as this command's own, so it is left alone"
		case *occupied:
			return Foreign, "this directory holds files and carries no claim from this command, and the host record does not select anything inside it, so it belongs to somebody else and is left alone"
		case protected:
			return Keep, "this environment is in use and carries no claim, so it is left exactly as it is"
		}
		return Adopt, "this directory is empty and carries no claim, so it is taken over as it stands; nothing is removed"
	}
	if !claim.Usable() {
		return Keep, "no claim of this command's could be read here, so who owns this directory could not be established: " + claim.Detail
	}
	state := ""
	if value, ok := claim.Value.(record.Object); ok {
		state, _ = record.Get(value, "state").(string)
	}
	if liveness == Live {
		return Occupied, "another run holds this staging and is still building it"
	}
	if liveness == Unknown {
		return Keep, "whether a run still holds this staging could not be established, and an owner nobody could establish is not an owner that is gone"
	}
	if state == Complete {
		if selected {
			return Settled, "this combination is already installed here and selected, so there is nothing to build"
		}
		if protected {
			return Keep, "this environment finished and something may be using it, but the host record could not be read to confirm it selects this one, so reporting it as already installed would be a success claim from a reading that failed"
		}
		return Keep, "this environment was promoted once and finished. Nothing selects it now, but a process started from it may still be running out of it, so it is reported rather than removed"
	}
	if selected {
		return Resume, "a previous run committed this environment as selected and did not finish. It is built and in use, so the pointer is brought into agreement with the selection rather than anything being rebuilt"
	}
	if protected {
		return Keep, "something may be using this environment, but the host record could not be read to confirm it selects this one, so neither removing it nor writing a pointer to it is established as safe"
	}
	return Reclaim, "this staging was abandoned by a run that no longer holds it and nothing selects it or points at it, so it is removed and created again"
}

// ClearOwn is staging.clear_own: remove this command's own two files and nothing else.
func ClearOwn(directory string) []string {
	var removed []string
	for _, name := range []string{ClaimName, LockName} {
		if err := os.Remove(filepath.Join(directory, name)); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}

// IsSettled is staging.settled: whether a readable claim says its run finished.
func IsSettled(claim reading.Reading) bool {
	value, ok := claim.Value.(record.Object)
	return claim.OK() && ok && record.Get(value, "state") == Complete
}

// Stage is a Go staging in progress: a temporary sibling of the final directory, locked and
// claimed STAGING, that Commit renames into place once it is complete.
type Stage struct {
	Directory string // the temporary directory being built
	Final     string
	held      *Held
}

// Begin creates <parent>/.<basename of final>.tmp-<random> with an exclusive mkdir, takes its
// lock and writes its STAGING claim. A final directory that already exists is the caller's to
// decide on (Decide) before staging again.
func Begin(final string, issue, run any) (*Stage, error) {
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(parent, "."+filepath.Base(final)+".tmp-")
	if err != nil {
		return nil, err
	}
	held, err := Take(directory)
	if err != nil {
		return nil, err
	}
	if err := WriteClaim(directory, NewPayload(Staging, issue, run)); err != nil {
		held.Release()
		return nil, err
	}
	return &Stage{Directory: directory, Final: final, held: held}, nil
}

// ErrFinalExists is Commit's answer when something already sits at the final name.
var ErrFinalExists = errors.New("the final directory already exists")

// Commit writes the COMPLETE claim and renames the staging into its final name. The lock stays
// held (it moved with the directory) until Release.
func (s *Stage) Commit(issue, run any) error {
	if _, err := os.Lstat(s.Final); err == nil {
		return ErrFinalExists
	}
	if err := WriteClaim(s.Directory, NewPayload(Complete, issue, run)); err != nil {
		return err
	}
	if err := os.Rename(s.Directory, s.Final); err != nil {
		return err
	}
	s.Directory = s.Final
	return nil
}

// Release lets go of the staging lock.
func (s *Stage) Release() { s.held.Release() }

// Claim is the claim document as a report shows it.
func Claim(claim reading.Reading) any {
	if value, ok := claim.Value.(contract.OrderedObject); ok {
		return value
	}
	return nil
}
