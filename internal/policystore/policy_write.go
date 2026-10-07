package policystore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// The named outcomes of a write. Each is a state a caller must tell apart; none is collapsed into
// another and none is an empty success. The handler maps each to its status.
const (
	WriteStored         = "stored"
	WriteStaleDigest    = "stale_digest"
	WriteInvalidPolicy  = "invalid_policy"
	WriteRegisterFailed = "register_failed"
	WriteRecoveryNeeded = "recovery_needed"
	WriteNotRegistered  = "not_registered"
	WriteUnreadable     = "unreadable"
	WriteSymlinked      = "symlinked"
	WriteBusy           = "busy"
	WriteCancelled      = "cancelled"
	WriteFailed         = "failed"
)

// The shapes of the durable artifacts this file writes.
const (
	// writeLockSuffix names the lock file beside the policy file. The lock is an flock, so the file
	// is created once and never unlinked; a lock file that outlives its holder is the one thing an
	// flock does not need swept.
	writeLockSuffix = ".lock"
	// writeBackupPrefix names the backup of the original bytes: <file>.backup-<UTC>.
	writeBackupPrefix = ".backup-"
	// writeBackupMode is the mode of a backup: it holds a policy, so only its owner reads it.
	writeBackupMode = 0o600
	// writeLockTimeout bounds the wait for the policy lock and writeLockPoll is the interval
	// between tries. A run that cannot take the lock answers busy rather than waiting for ever.
	writeLockTimeout = 10 * time.Second
	writeLockPoll    = 25 * time.Millisecond
	// writeTempPrefix is the temporary file's name prefix in the policy's own directory.
	writeTempPrefix = ".crw-policy-"
)

// errPolicyMoved is the publication's answer when the file no longer holds the bytes the caller
// authorized it to replace: the exchange was undone and nothing was replaced.
var errPolicyMoved = errors.New("the policy file changed since it was read")

// errExchangeHappened reports a publication whose exchange ran and whose outcome could not be read
// back or put back. The path holds the new bytes, so a caller must not report that nothing was
// written; the bytes the exchange displaced are kept at the path the error names.
var errExchangeHappened = errors.New("the policy file was replaced and what it held could not be read back")

// writeDecisionTimeout bounds the post-publication phase: the registration, the (a)/(b)/(c)
// decision and any restore. It is a var so a test can shorten it. The phase runs on a context
// detached from the request, because a client that goes away must not be able to leave the policy
// file and the wiring record naming different digests - the launcher refuses such a policy, so
// every bridge would fail to start until a person repaired it.
var writeDecisionTimeout = 2 * time.Minute

// The outcome spellings of crw install register-mcp --re-register-policy that mean the wiring record
// names the new policy: the registration succeeded.
var registrationSucceeded = map[string]bool{"record_updated": true, "record_unchanged": true}

// The outcome spellings the decided answers name as a registration that did not update the record.
// The policy file is put back only when the record is then read and still names the bytes this run
// replaced; an outcome in this list does not by itself establish that, so each one is confirmed.
var registrationUnchanged = map[string]bool{
	"record_absent":               true,
	"record_differs":              true,
	"record_not_canonical":        true,
	"record_symlinked":            true,
	"execution_policy_unreadable": true,
	"CONFLICT":                    true,
	"record_changed_underneath":   true,
}

// RegisterAnswer is what one registration step answered: the exit status install.Main returned, the
// JSON envelope it wrote to standard output, its standard error, and whether the call itself failed
// (a lost response or a cancelled run). The outcome field of the envelope decides, never the exit
// status alone.
type RegisterAnswer struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Err      error
}

// RegisterFunc performs the registration step for one policy file.
// path is the kernel spelling of the file (the value the command line's --execution-policy
// receives), which the registerer passes to the installer unchanged and never encodes a second
// time: the installer opens what it is given, so a record's surrogate escape must already have
// been decoded to its byte.
type RegisterFunc func(ctx context.Context, path string) RegisterAnswer

// writeLocate and writeSwap are the two steps a test replaces to drive a state it cannot otherwise
// reach: the record as a request saw it before it waited for the lock, and the moment between the
// replacement and the registration. The production values are Locate and swapPolicy.
var (
	writeLocate = Locate
	writeSwap   = swapPolicy
	// writeCandidate is the seam a test replaces to reach the moment between the backup and the
	// publication: a cancellation there must publish nothing.
	writeCandidate = candidateBytes
)

// WriteOptions are the seams a write runs with. A nil field takes the production value: the clock,
// install.Main in this process, and the running relay's worker policy digest.
type WriteOptions struct {
	Now      func() time.Time
	Register RegisterFunc
	Running  func(context.Context, LookupEnv) Running
	// Swap replaces the policy file durably, and only while it still holds the expected bytes. It is
	// the seam a caller in another package (the GUI route) uses to reach the publication and the
	// restore, which the package-level writeSwap seam cannot reach from outside policystore. A nil
	// field takes swapPolicy.
	Swap SwapFunc
}

// SwapFunc is the shape of the publication step: the file is replaced only while it still holds
// expected, by an atomic exchange with a temporary file holding next. displaced is what the exchange
// moved out of the path and is set only when the replacement stands. kept names a file holding bytes
// this call did not create and therefore never deletes: the caller must report it. err is nil on a
// standing replacement, errPolicyMoved when the file had already changed (nothing was replaced),
// errExchangeHappened when the exchange ran and its outcome could not be established, and any other
// error when nothing was replaced. The context is honoured up to the exchange, which is the durable
// effect. It is exported so a caller outside policystore can name the type of the WriteOptions.Swap
// seam.
type SwapFunc func(ctx context.Context, path string, expected, next []byte, mode os.FileMode) (displaced []byte, kept string, err error)

// WriteRequest is one proposed write: the digest the caller read and the change it proposes.
type WriteRequest struct {
	ExpectedDigest string
	Change         Change
}

// WriteResult is what a write answers. Kind names the outcome; the other fields carry the evidence
// of that outcome and are empty where the outcome does not have them.
type WriteResult struct {
	Kind             string
	Errors           []string
	Warnings         []string
	CurrentDigest    string
	StoredDigest     string
	RegisteredDigest string
	FileDigest       string
	Backup           string
	// RecordBackup is the wiring record's own backup, taken by the installer beside the record. It is
	// a different artifact from Backup, which holds the policy file's previous bytes.
	RecordBackup string
	// RestartRequired is the installer's advice about a bridge or relay service already running: it
	// keeps the policy it started under until it is restarted.
	RestartRequired string
	// Kept names a file holding bytes this write did not create and therefore never deletes (a
	// document that raced the exchange). It is empty when no such bytes exist.
	Kept     string
	Recovery string
	Restored bool
	Applied  string
	Actions  []string
	Step     string
}

// Write applies one change to the execution policy the wiring record names, and brings the record's
// digest up to date with the bytes it wrote.
//
// The order is the decided one and it is not rearranged: the lock beside the policy file is taken
// first, the file is read again under it, a digest that is not the caller's is refused with the
// digest on disk, the candidate is judged with the same check the read path answers with, the
// original bytes are backed up, the candidate is published atomically, and the registration step
// runs in this process while the lock is still held. A registration the write cannot trust is
// decided by reading the file and the record again rather than by believing the answer.
//
// Cancellation is honoured only before the candidate is published: once the file holds the new
// bytes, the registration and the decision must finish, because stopping there would leave the file
// and the wiring record naming different digests and no bridge would start until a person repaired
// it. That final sequence therefore runs on a context detached from the caller's, bounded by this
// write's own timeout; a bound that runs out is decided like any other untrusted answer.
func Write(ctx context.Context, env LookupEnv, opts WriteOptions, request WriteRequest) WriteResult {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	register := opts.Register
	if register == nil {
		register = registerWithInstaller
	}
	running := opts.Running
	if running == nil {
		running = RunningDigest
	}
	swap := opts.Swap
	if swap == nil {
		swap = writeSwap
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{Kind: WriteCancelled, Step: "start", Errors: []string{err.Error()}}
	}
	located := writeLocate(env)
	if located.State != Registered {
		kind := WriteUnreadable
		if located.State == NotRegistered {
			kind = WriteNotRegistered
		}
		return WriteResult{Kind: kind, Errors: []string{located.Reason}}
	}
	path := located.Path
	// The record names the policy the way the installer recorded it, which may be a surrogate escape
	// standing for a byte that is not UTF-8. The kernel opens the byte, so every filesystem call
	// below uses the encoded spelling while the record's own spelling is kept for the installer, the
	// messages and the answer.
	encoded, err := encodedPath(path)
	if err != nil {
		return WriteResult{Kind: WriteUnreadable, Errors: []string{err.Error()}}
	}
	lock, err := lockPolicy(ctx, encoded, writeLockTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return WriteResult{Kind: WriteCancelled, Step: "lock", Errors: []string{ctx.Err().Error()}}
		}
		return WriteResult{Kind: WriteBusy, Errors: []string{err.Error()}}
	}
	defer lock.release()

	// The bytes judged are the bytes read here, inside the lock: a snapshot a caller passed in could
	// have moved between its read and this write.
	raw, err := ReadRaw(path)
	if err != nil {
		return WriteResult{Kind: WriteUnreadable, Errors: []string{err.Error()}}
	}
	original := digestOfBytes(raw)
	if request.ExpectedDigest != original {
		return WriteResult{Kind: WriteStaleDigest, CurrentDigest: original}
	}
	// The record is read again under the lock. A request that waited for another writer must decide
	// against the record as it now stands, never against the one it read before the wait, or a
	// queued write would refuse a policy the earlier writer already registered.
	located = writeLocate(env)
	if located.State != Registered {
		kind := WriteUnreadable
		if located.State == NotRegistered {
			kind = WriteNotRegistered
		}
		return WriteResult{Kind: kind, Errors: []string{located.Reason}}
	}
	if located.Path != path {
		// Another writer registered a different policy while this run waited. Replacing the file this
		// run locked would leave that record naming bytes it does not describe.
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: original, RegisteredDigest: located.RegisteredDigest,
			Recovery: "the wiring record now names " + located.Path + ", not the policy this write locked, so nothing was written; re-register the policy you meant to change"}
	}
	// The write maintains one invariant: a policy file and the wiring record that names it hold the
	// same digest. A violation under the lock is a write that could not reconcile its two durable
	// effects, so this one refuses before touching anything and the next one does the same.
	if located.RegisteredDigest != original {
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: original, RegisteredDigest: located.RegisteredDigest,
			Recovery: recoveryAdvice(path, "")}
	}
	checked := Check(raw, request.ExpectedDigest, request.Change)
	if !checked.Valid {
		return WriteResult{Kind: WriteInvalidPolicy, Errors: checked.Errors}
	}
	if info, err := os.Lstat(encoded); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return WriteResult{Kind: WriteSymlinked, Errors: []string{"the execution policy at " + path + " is a symbolic link, and replacing it would turn the link into a regular file rather than update the file it names"}}
	}
	info, err := os.Stat(encoded)
	if err != nil {
		return WriteResult{Kind: WriteUnreadable, Errors: []string{err.Error()}}
	}
	// The backup is the first durable effect of this write: a request that went away while the
	// candidate was judged must not leave one behind.
	if err := ctx.Err(); err != nil {
		return WriteResult{Kind: WriteCancelled, Step: "check", Errors: []string{err.Error()}}
	}
	backup, err := backupPolicy(encoded, raw, now())
	if err != nil {
		return WriteResult{Kind: WriteFailed, Errors: []string{"the execution policy could not be backed up: " + err.Error()}}
	}
	// The backup is named in the record's own spelling, as every other path in an answer is.
	reported := pyvalue.FSDecode(backup)
	if err := ctx.Err(); err != nil {
		return WriteResult{Kind: WriteCancelled, Step: "backup", Backup: reported, Errors: []string{err.Error()}}
	}
	updated, err := writeCandidate(raw, request.Change)
	if err != nil {
		return WriteResult{Kind: WriteInvalidPolicy, Backup: reported, Errors: []string{err.Error()}}
	}
	// The last boundary before the replacement: a request that went away while the candidate was
	// rendered must not publish. After this point cancellation is no longer honoured, because stopping
	// there would leave the file and the wiring record naming different digests.
	if err := ctx.Err(); err != nil {
		return WriteResult{Kind: WriteCancelled, Step: "publish", Backup: reported, Errors: []string{err.Error()}}
	}
	// The replacement happens only while the file still holds the bytes read under the lock, and it is
	// an atomic exchange, so a writer that saved in between is neither replaced nor lost.
	displaced, kept, err := swap(ctx, encoded, raw, updated, info.Mode())
	var warnings []string
	switch {
	case kept != "":
		// Bytes this call did not create are kept: the file and the record do not describe one
		// document, so the answer is a recovery that names where those bytes are.
		observed, readErr := digestAt(path)
		if readErr != nil {
			observed = ""
		}
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: observed, RegisteredDigest: original, Backup: reported,
			Kept: pyvalue.FSDecode(kept), Recovery: recoveryAdviceKept(path, reported, pyvalue.FSDecode(kept)),
			Warnings: warnings, Errors: []string{err.Error()}}
	case err == nil:
		// The file held the bytes this run read and now holds the candidate.
	case errors.Is(err, errPolicyMoved):
		// The file no longer holds the bytes this run read, so nothing was replaced. The record still
		// names those bytes, so the two disagree and the answer says what is on disk.
		observed, readErr := digestAt(path)
		detail := "the execution policy changed while this write held its lock, so it was not replaced"
		if readErr != nil {
			observed = ""
			detail += "; the policy file could not be read back: " + readErr.Error()
		}
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: observed, RegisteredDigest: original, Backup: reported,
			Recovery: recoveryAdvice(path, reported), Errors: []string{detail}}
	case displaced == nil && ctx.Err() != nil:
		// The publication refused the replacement because the request ended: nothing was replaced.
		return WriteResult{Kind: WriteCancelled, Step: "publish", Backup: reported, Errors: []string{err.Error()}}
	case displaced == nil:
		return WriteResult{Kind: WriteFailed, Backup: reported, Errors: []string{"the execution policy could not be written: " + err.Error()}}
	default:
		// The exchange happened and its durability was not established: the candidate is on disk, so
		// this is a replacement whose survival through a power loss is not known.
		warnings = append(warnings, "the execution policy was replaced and its directory could not be synced ("+err.Error()+"); a host that loses power now may find the previous file")
	}
	stored := digestOfBytes(updated)
	// The replacement has happened, so the sequence must run to completion: cancellation is honoured
	// only before it. A request context that ends here (a closed browser tab, an aborted fetch) would
	// otherwise leave the file and the wiring record naming different digests, which is a policy no
	// bridge will start under. The registration, the decision and any restore therefore run under a
	// context detached from the request and bounded by this write's own budget; a budget that runs out
	// is decided as an untrusted answer by re-reading the file and the record, never left half-done.
	decision, stop := context.WithTimeout(context.WithoutCancel(ctx), writeDecisionTimeout)
	defer stop()
	// The restore repairs a file this run already replaced, so it must run to completion: neither the
	// request's cancellation nor the decision bound may stop it, or the file and the record would be
	// left naming different digests with no repair. Its own atomic write is a bounded local operation.
	restore := context.WithoutCancel(decision)
	// The registration receives the kernel spelling the command line's --execution-policy receives,
	// not the record's surrogate-escaped spelling: the installer opens what it is given.
	answer := register(decision, encoded)
	envelope, parsed := registrationEnvelopeOf(answer)
	outcome := envelope.Outcome
	switch {
	case answer.Err == nil && parsed && registrationSucceeded[outcome]:
		return storedResult(decision, env, running, path, stored, reported, envelope, warnings, nil)
	case answer.Err == nil && parsed && registrationUnchanged[outcome]:
		// The outcome says the registration did not update the record. Whether the record still names
		// the bytes this run replaced is a separate fact, and only that fact makes a restore correct:
		// record_absent and record_changed_underneath in particular can leave a record that names
		// something else, or nothing at all.
		if after := Locate(env); after.State == Registered && after.RegisteredDigest == original {
			return restoreResult(restore, swap, env, encoded, path, raw, info.Mode(), original, stored, reported, warnings, "the registration did not take the new policy: "+outcome)
		}
		// The record names something else, so nothing is restored. What the file holds is read rather
		// than assumed: reporting the digest this run tried to write would present an intention as an
		// observation.
		fileNow, fileErr := digestAt(path)
		detail := "the registration did not take the new policy: " + outcome
		if fileErr != nil {
			detail += "; the policy file could not be read back: " + fileErr.Error()
		}
		return recoveryFrom(env, path, fileNow, reported, warnings, detail)
	}
	// The answer is not trusted: read the file and the record again and decide from what they say.
	fileNow, fileErr := digestAt(path)
	recordNow := Locate(env)
	detail := "the registration answered " + describeAnswer(answer, outcome, parsed)
	switch {
	case fileErr == nil && fileNow == stored && recordNow.State == Registered && recordNow.RegisteredDigest == stored:
		// (a) both durable effects happened; only the answer was lost.
		return storedResult(decision, env, running, path, stored, reported, registrationEnvelope{}, warnings, []string{detail})
	case recordNow.State == Registered && recordNow.RegisteredDigest == original:
		// (b) the record still names the old bytes: put them back.
		return restoreResult(restore, swap, env, encoded, path, raw, info.Mode(), original, stored, reported, warnings, detail)
	default:
		// (c) the two no longer describe one document, or the restore cannot be made.
		if fileErr != nil {
			// The file could not be read back, so what it holds was not established. Reporting the
			// digest this run tried to write would present an intention as an observation.
			detail += "; the policy file could not be read back: " + fileErr.Error()
		}
		return recoveryFrom(env, path, fileNow, reported, warnings, detail)
	}
}

// recoveryFrom is the (c) answer: the policy file and the wiring record no longer describe one
// document, so the write stops with both digests, the backup and the command that settles them.
func recoveryFrom(env LookupEnv, path, fileDigest, backup string, warnings []string, detail string) WriteResult {
	registered := ""
	if located := Locate(env); located.State == Registered {
		registered = located.RegisteredDigest
	}
	return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: fileDigest, RegisteredDigest: registered,
		Backup: backup, Recovery: recoveryAdvice(path, backup), Warnings: warnings, Errors: []string{detail}}
}

// storedResult is the answer for a write whose two durable effects both happened: the file holds the
// new bytes and the record names them. applied is the read path's own rule, so the two answers
// cannot disagree about whether the running relay has loaded the file.
// envelope carries the facts a trusted registration answered with: the wiring record's backup and the
// restart advice. A success established by re-reading (the lost-answer case) passes the zero envelope,
// because nothing in an answer that was not trusted may be reported.
func storedResult(ctx context.Context, env LookupEnv, running func(context.Context, LookupEnv) Running, path, stored, backup string, envelope registrationEnvelope, warnings, extra []string) WriteResult {
	// The registration answers about the file it read, which is the file at the path now: the
	// installer opens the path itself, so an editor that saved after this run published leaves a
	// record naming bytes this run did not write. What is stored is therefore read back rather than
	// assumed to be the candidate.
	current, readErr := ReadRaw(path)
	if readErr != nil {
		// The file cannot be read back, so what is stored is not established: the digest this run tried
		// to write is not reported as the file's. The record's digest is read rather than assumed, and
		// is left empty when the record itself cannot be read.
		located := Locate(env)
		registered := ""
		if located.State == Registered && located.RegisteredDigest != "" {
			registered = located.RegisteredDigest
		}
		warnings = append(warnings, "the execution policy could not be read back after the registration: "+readErr.Error())
		return WriteResult{Kind: WriteRecoveryNeeded, RegisteredDigest: registered, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{"the execution policy could not be read back after the registration: " + readErr.Error()}}
	}
	stored = digestOfBytes(current)
	registered := stored
	located := Locate(env)
	established := located.State == Registered && located.RegisteredDigest != ""
	if established {
		registered = located.RegisteredDigest
	} else {
		// The record could not be read back after the registration reported success. The file is the
		// bytes this run wrote, but what the record now names was not established, so the answer leaves
		// registered empty rather than reporting the file's digest as the record's.
		registered = ""
		warnings = append(warnings, "the execution policy was written and the wiring record could not be read back afterwards, so the digest it names was not established: "+located.Reason)
	}
	file := Reading{State: Registered, Path: path, Digest: stored, RegisteredDigest: registered}
	observed := running(ctx, env)
	applied := Applied(file, observed)
	if !established {
		// Whether the host enforces these bytes was not established either, so the applied rule's
		// unverifiable answer is the honest one rather than one taken from the file alone.
		applied = AppliedUnverifiable
	}
	return WriteResult{
		Kind:             WriteStored,
		StoredDigest:     stored,
		RegisteredDigest: registered,
		FileDigest:       stored,
		Backup:           backup,
		RecordBackup:     envelope.Backup,
		RestartRequired:  envelope.RestartRequired,
		Applied:          applied,
		Actions:          AppliedActions(file, applied),
		Warnings:         append(warnings, extra...),
		Step:             "registered",
	}
}

// restoreResult puts the original bytes back with the same atomic write and answers register_failed
// only after the bytes on disk have been read back and confirmed and the restore's own durability
// was established. A restore that cannot be made, or whose durability was not established, is a
// recovery rather than a confirmed restore.
// A restore whose rename happened and whose directory could not be synced is still a restore: the
// answer is register_failed only when the bytes read back are the ones the restore put there; when
// another writer moved them the file and the record disagree and the answer is a recovery. A restore
// whose rename happened and whose directory could not be synced is still a restore - the original
// bytes are on disk and the record names them - and its answer warns that a power loss may bring the
// new bytes back, in which case the next write refuses on the digest disagreement.
// The restore replaces only the bytes this run published: a file another writer moved is not this
// run's to overwrite, and those bytes are in no backup, so the answer is a recovery that names what
// was observed instead.
func restoreResult(ctx context.Context, swap SwapFunc, env LookupEnv, encoded, path string, raw []byte, mode os.FileMode, original, published, backup string, warnings []string, detail string) WriteResult {
	// What the file holds decides. The bytes this run published are put back with the same atomic
	// exchange, so a writer that saved in between is neither replaced nor lost; the original bytes
	// already on disk are a state the two durable artifacts agree on; anything else is a third
	// document this run must not overwrite.
	current, readErr := ReadRaw(path)
	if readErr != nil {
		return WriteResult{Kind: WriteRecoveryNeeded, RegisteredDigest: original, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{detail + "; the policy file could not be read back: " + readErr.Error()}}
	}
	observed := digestOfBytes(current)
	if observed != original && observed != published {
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: observed, RegisteredDigest: original, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{detail + "; the file holds neither the bytes this write published nor the ones the record names, so it was left as it stands"}}
	}
	var syncErr error
	// keptPath names a file holding bytes this call did not delete. A publication whose exchange ran
	// and whose read-back failed leaves the bytes it displaced at that path, so the file is named in
	// the answer rather than silently abandoned.
	var keptPath string
	if observed == published {
		displaced, kept, err := swap(ctx, encoded, current, raw, mode)
		switch {
		case err == nil:
		case errors.Is(err, errPolicyMoved):
			now, _ := digestAt(path)
			return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: now, RegisteredDigest: original, Backup: backup,
				Kept: pyvalue.FSDecode(kept), Recovery: recoveryAdviceKept(path, backup, pyvalue.FSDecode(kept)), Warnings: warnings,
				Errors: []string{detail + "; the file changed while the restore was running, so it was left as it stands"}}
		case errors.Is(err, errExchangeHappened):
			// The exchange ran, so the original bytes are at the path; what it displaced could not be
			// read back, or the exchange could not be undone. What the path holds is read below rather
			// than assumed, the durability of the replacement was not established, and the file the
			// exchange left its bytes at is named rather than deleted.
			keptPath = pyvalue.FSDecode(kept)
			syncErr = err
		case displaced == nil:
			now, _ := digestAt(path)
			return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: now, RegisteredDigest: original, Backup: backup,
				Recovery: recoveryAdvice(path, backup), Warnings: warnings,
				Errors: []string{detail + "; the original bytes could not be put back: " + err.Error()}}
		default:
			syncErr = err
		}
	}
	confirmed, confirmErr := digestAt(path)
	if confirmErr != nil {
		return WriteResult{Kind: WriteRecoveryNeeded, RegisteredDigest: original, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{detail + "; the bytes could not be read back after the restore: " + confirmErr.Error()}}
	}
	if confirmed != original {
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: confirmed, RegisteredDigest: original, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{detail + "; the bytes read back after the restore are not the ones that were backed up"}}
	}
	// The file holds the original bytes again. The record is read once more: a registration that ran
	// elsewhere while this restore was deciding may now name another digest, and then the two still
	// disagree even though the file is back.
	// The file holds the original bytes again, so the record must be read once more and must still
	// name them: a registration that ran elsewhere while this restore was deciding may name another
	// digest, and a record that is now absent or unreadable establishes nothing. Either way the two
	// no longer describe one document, which is a recovery rather than a confirmed restore.
	after := Locate(env)
	switch {
	case after.State != Registered:
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: confirmed, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{detail + "; the wiring record could not be read back after the restore: " + after.Reason}}
	case after.RegisteredDigest != original:
		return WriteResult{Kind: WriteRecoveryNeeded, FileDigest: confirmed, RegisteredDigest: after.RegisteredDigest, Backup: backup,
			Recovery: recoveryAdvice(path, backup), Warnings: warnings,
			Errors: []string{detail + "; the wiring record names " + after.RegisteredDigest + ", not the bytes that were put back"}}
	}
	if syncErr != nil {
		return WriteResult{Kind: WriteRegisterFailed, Restored: true, FileDigest: confirmed, RegisteredDigest: original,
			Backup: backup, Kept: keptPath, Warnings: append(warnings, "the original bytes are back and the restore's durability was not established ("+syncErr.Error()+"), so a host that loses power now may find the new bytes; the next write is then refused because the file and the wiring record name different digests"),
			Errors: []string{detail}, Step: "restored"}
	}
	return WriteResult{Kind: WriteRegisterFailed, Restored: true, FileDigest: confirmed, RegisteredDigest: original,
		Backup: backup, Kept: keptPath, Warnings: warnings, Errors: []string{detail}, Step: "restored"}
}

// registerWithInstaller is the production registration step: the same code crw install register-mcp
// --re-register-policy --execution-policy FILE runs, called in this process so no child is started
// and the installer's own judgement is reused rather than re-implemented here. It takes the
// crw-mcp-ownership lock itself, inside the policy lock this write holds.
func registerWithInstaller(ctx context.Context, path string) RegisterAnswer {
	var stdout, stderr bytes.Buffer
	code := install.Main(ctx, []string{"register-mcp", "--re-register-policy", "--execution-policy", path},
		scope.Env(os.Environ()), &stdout, &stderr)
	return RegisterAnswer{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
}

// registrationEnvelope is the part of the installer's JSON answer this write reads: the outcome that
// decides, the backup of the wiring record the registration took, and the restart advice. The policy
// file's own backup is a different artifact and never travels here.
type registrationEnvelope struct {
	Outcome         string `json:"outcome"`
	Backup          string `json:"backup"`
	RestartRequired string `json:"restartRequired"`
}

// registrationEnvelope reads the installer's JSON envelope. parsed is false when the answer carries
// no readable envelope at all, or names no outcome, which is itself a reason not to trust it.
func registrationEnvelopeOf(answer RegisterAnswer) (registrationEnvelope, bool) {
	if len(answer.Stdout) == 0 {
		return registrationEnvelope{}, false
	}
	var envelope registrationEnvelope
	if err := json.Unmarshal(answer.Stdout, &envelope); err != nil {
		return registrationEnvelope{}, false
	}
	if envelope.Outcome == "" {
		return registrationEnvelope{}, false
	}
	return envelope, true
}

// describeAnswer is the detail a recovery answer carries about the answer that could not be trusted.
// It names the outcome when there was one and the failure otherwise; it never quotes the envelope,
// which could carry a path this answer already names.
func describeAnswer(answer RegisterAnswer, outcome string, parsed bool) string {
	switch {
	case answer.Err != nil:
		return "with an error: " + answer.Err.Error()
	case !parsed:
		return "exit " + strconv.Itoa(answer.ExitCode) + " without a readable result"
	default:
		return outcome + " (exit " + strconv.Itoa(answer.ExitCode) + ")"
	}
}

// recoveryAdvice is what a person does about a policy file and a wiring record that no longer agree.
// The backup is named only when this run knows one.
// A path whose bytes are not UTF-8 is spelled by the installer as a surrogate escape; a JSON writer
// that replaces it with U+FFFD would name a file that does not exist. The advice is therefore built
// from the path's own bytes (the kernel spelling) so that it survives any writer: the command it
// names opens the file the record describes.
func recoveryAdvice(path, backup string) string {
	advice := "the execution policy and the wiring record name different digests, so neither is enforced; re-register the file with crw install register-mcp --re-register-policy --execution-policy " + path
	if backup != "" {
		advice += ", or put the bytes in " + backup + " back and register them"
	}
	return advice
}

// recoveryAdviceKept is recoveryAdvice for a write that kept bytes another writer put there: the kept
// file is named, because it is the only place those bytes exist.
func recoveryAdviceKept(path, backup, kept string) string {
	advice := recoveryAdvice(path, backup)
	if kept != "" {
		advice += "; the bytes this write did not create are kept at " + kept
	}
	return advice
}

// candidateBytes renders the bytes one change produces, through the same decode, apply and encode
// the check judged the candidate with, so the bytes written are exactly the bytes Check approved.
func candidateBytes(raw []byte, change Change) ([]byte, error) {
	document, err := decode(raw)
	if err != nil {
		return nil, err
	}
	before := snapshotSections(document)
	updated, _, err := apply(document, change)
	if err != nil {
		return nil, err
	}
	if err := onlyTheTargetMoved(before, updated, change); err != nil {
		return nil, err
	}
	return []byte(encode(updated)), nil
}

// digestOfBytes is the digest every reader of the policy file uses.
func digestOfBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// digestAt is the digest of the bytes at path, read through the same descriptor-judged reader the
// reading half uses. path is the record's spelling; the reader encodes it itself.
func digestAt(path string) (string, error) {
	raw, err := ReadRaw(path)
	if err != nil {
		return "", err
	}
	return digestOfBytes(raw), nil
}

// readRegularKernel reads the bytes of the regular file at path, which is already a kernel spelling
// (os.CreateTemp's answer, for example). Unlike ReadRaw it does not fs-encode the path again: a
// directory whose own name holds bytes that look like a WTF-8 surrogate would otherwise be read as
// a different file or refused.
func readRegularKernel(path string) ([]byte, error) {
	file, err := reading.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// backupPolicy writes the bytes the decision was made from to <path>.backup-<UTC> and fsyncs them
// before the replacement is published. path is the encoded spelling the kernel opens. A name that is
// already taken takes the next number rather than overwriting a backup that is already there; a name
// that is a symbolic link is refused by O_EXCL like any other existing name, so a backup never
// follows a link to another file.
func backupPolicy(path string, raw []byte, now time.Time) (string, error) {
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(now.UTC().Format("2006-01-02T15:04:05Z"))
	name := ""
	var out *os.File
	var err error
	for attempt := 0; attempt < 1000 && name == ""; attempt++ {
		candidate := path + writeBackupPrefix + stamp
		if attempt > 0 {
			candidate = path + writeBackupPrefix + stamp + "-" + strconv.Itoa(attempt)
		}
		out, err = os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, writeBackupMode)
		switch {
		case err == nil:
			name = candidate
		case errors.Is(err, os.ErrExist):
			err = nil
		default:
			return "", err
		}
	}
	if name == "" {
		return "", errors.New("no free backup name beside " + path)
	}
	_, writeErr := out.Write(raw)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// swapPolicy replaces the policy file durably and only while it still holds expected: a temporary
// file beside it takes the mode the file already has and the new bytes, its contents are fsynced,
// the two are exchanged atomically, and the directory is fsynced so the exchange survives a power
// loss. The exchange is the durable effect and the only step that is not reversible, so the context
// is honoured immediately before it.
//
// A plain rename replaces whatever is at the path; the exchange lets this step read back what it
// displaced and put it back when it is not the bytes the caller authorized, so a writer that saved
// between the caller's decision and this call keeps its content. Bytes this call did not create are
// never deleted: a document that raced the undo is kept at the path the error names.
func swapPolicy(ctx context.Context, path string, expected, next []byte, mode os.FileMode) (displaced []byte, kept string, err error) {
	dir := publishParent(path)
	file, err := os.CreateTemp(dir, writeTempPrefix)
	if err != nil {
		return nil, "", err
	}
	temporary := file.Name()
	fail := func(err error) ([]byte, string, error) {
		_ = file.Close()
		_ = os.Remove(temporary)
		return nil, "", err
	}
	if err := os.Chmod(temporary, mode.Perm()); err != nil {
		return fail(err)
	}
	if _, err := file.Write(next); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(temporary)
		return nil, "", err
	}
	if err := exchangeFiles(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return nil, "", err
	}
	// The exchange happened: the temporary path now holds what the policy held. It is put back
	// unchanged when it is not the content this call was authorized to replace, so no writer's bytes
	// are lost to a decision made before it saved.
	// The displaced content is read with the package's own non-blocking reader, so a path that became a
	// named pipe after it was judged cannot hang the request while the policy lock is held.
	displaced, readErr := readRegularKernel(temporary)
	if readErr != nil {
		// The exchange ran and what it displaced cannot be read. The bytes are kept at the temporary
		// path, which is never removed, and the caller is told the replacement stands.
		return nil, temporary, fmt.Errorf("%w: %s (%s)", errExchangeHappened, readErr.Error(), temporary)
	}
	if bytes.Equal(displaced, expected) {
		_ = os.Remove(temporary)
		return displaced, "", syncDirectory(dir)
	}
	// The file had already moved on, so this call replaces nothing. The exchange is undone so the
	// writer that saved keeps its bytes.
	if undoErr := exchangeFiles(temporary, path); undoErr != nil {
		return nil, temporary, fmt.Errorf("%w: the exchange could not be undone: %s (%s)", errExchangeHappened, undoErr.Error(), temporary)
	}
	// The undo moved this call's own candidate back to the temporary path unless a writer saved again
	// in between; that writer's document is kept and named rather than deleted.
	if back, backErr := readRegularKernel(temporary); backErr == nil && bytes.Equal(back, next) {
		_ = os.Remove(temporary)
		return nil, "", errPolicyMoved
	}
	return nil, temporary, errPolicyMoved
}

// publishParent is the directory the temporary file is created in and synced: the path's own parent
// with its symbolic links resolved, which is the directory the kernel resolves the rename target
// into. The lexical parent is not enough, because filepath.Dir folds a ".." that follows a symbolic
// link, naming a different directory than the kernel does; a temporary file there would make the
// rename cross a filesystem boundary and the directory fsync would land on the wrong directory.
func publishParent(path string) string {
	dir := parentSpelling(path)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// parentSpelling is the path's parent as it is written, without the lexical cleaning filepath.Dir
// applies: a ".." must reach the kernel, which resolves the component before it first.
func parentSpelling(path string) string {
	index := strings.LastIndexByte(path, '/')
	switch {
	case index < 0:
		return "."
	case index == 0:
		return "/"
	default:
		return path[:index]
	}
}

// syncDirectory fsyncs a directory so a rename in it survives a power loss.
func syncDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	return errors.Join(syncErr, closeErr)
}

// policyLock is an exclusive advisory lock on <policy>.lock, held for the whole read-modify-write so
// two writes cannot decide against the same bytes. It is an flock, which excludes another flock
// holder and is released by the kernel when the process ends; the lock file itself is created once
// and never unlinked.
type policyLock struct {
	path   string
	handle *os.File
}

// lockPolicy takes the lock beside path, waiting up to timeout. path is the encoded spelling the
// kernel opens. It stops waiting, taking nothing, once ctx is done.
func lockPolicy(ctx context.Context, path string, timeout time.Duration) (*policyLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockPath := path + writeLockSuffix
	handle, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(handle.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &policyLock{path: lockPath, handle: handle}, nil
		}
		if !lockContended(err) {
			_ = handle.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = handle.Close()
			return nil, errors.New("another run holds the policy lock at " + lockPath)
		}
		timer := time.NewTimer(writeLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = handle.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// release unlocks and closes. The lock file is left where it is.
func (l *policyLock) release() {
	if l == nil || l.handle == nil {
		return
	}
	_ = unix.Flock(int(l.handle.Fd()), unix.LOCK_UN)
	_ = l.handle.Close()
	l.handle = nil
}

// lockContended is whether a failed flock is another holder rather than a real error.
func lockContended(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES)
}
