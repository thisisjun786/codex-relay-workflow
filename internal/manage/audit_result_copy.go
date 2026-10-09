package manage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"strings"
	"syscall"
	"time"
)

// auditResultsDir is where the copies of graded results live, below the audit state directory:
// <state_dir>/audit/results/<row id>.json.
const auditResultsDir = "results"

// auditRowIDChars is how many leading hex characters of the digest a row id keeps.
const auditRowIDChars = 16

// auditRowID is the id of an ok ledger row: the first sixteen hex characters of
// sha256(target "\n" head "\n" gradedAt), the target being the row's subject. Two grades of one
// target differ in their gradedAt, so a regrade never shares a row's id: a grade that starts in
// the same second as another takes the next free second (auditResultCopyPublish) (CRW-838).
func auditRowID(subject, head, gradedAt string) string {
	sum := sha256.Sum256([]byte(subject + "\n" + head + "\n" + gradedAt))
	return hex.EncodeToString(sum[:])[:auditRowIDChars]
}

// auditResultCopyPath is the copy of the result a row with this id carries.
func auditResultCopyPath(e *Env, cfg *Config, id string) string {
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditResultsDir, id+".json")
}

// auditResultCopyName accepts an id that is a plain lower-case hex name of the length a row id
// has, so a ledger line can never make a reader open a file outside the results directory.
func auditResultCopyName(id string) error {
	if len(id) != auditRowIDChars {
		return fmt.Errorf("the row id %q is not %d characters", id, auditRowIDChars)
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("the row id %q is not lower-case hexadecimal", id)
		}
	}
	return nil
}

// auditResultCopyBumps is how many following seconds a grade may move on to before its copy
// finds a free name.
const auditResultCopyBumps = 100000

// auditResultCopyPublish makes the bytes of a graded result visible whole under the first free
// copy name at or after gradedAt, and returns that name's id, its path and the graded_at it
// stands for. A temporary file beside the copies is written and fsynced once, then linked to the
// name, so a reader never sees a half-written copy and a copy that exists is never replaced.
//
// The id is a function of the target, the head and graded_at only, and graded_at has whole
// seconds, so two grades of one target that start in the same second (a regrade, or bundles that
// name the same subject and head graded together) would name one copy. Neither is refused for
// that: a name that is taken, whatever it holds, moves this grade's graded_at on by a second
// until the name is free, so every ok row has an id and a copy of its own and the id is still the
// digest of the graded_at the row records.
func auditResultCopyPublish(e *Env, cfg *Config, subject, head, gradedAt string, data []byte) (id, path, at string, err error) {
	first := auditResultCopyPath(e, cfg, "x")
	dir := rootDir(first)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}
	tmp, err := os.CreateTemp(dir, "result.tmp-")
	if err != nil {
		return "", "", "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", "", "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", "", err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", "", "", err
	}
	at = gradedAt
	for bump := 0; ; bump++ {
		id = auditRowID(subject, head, at)
		path = auditResultCopyPath(e, cfg, id)
		err := os.Link(tmpName, path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", "", "", err
		}
		when, perr := time.Parse(auditTimeFormat, at)
		if perr != nil || bump >= auditResultCopyBumps {
			return "", "", "", fmt.Errorf("result_copy_conflict: %s already holds another grade of %s at %s", path, subject, gradedAt)
		}
		at = when.UTC().Add(time.Second).Format(auditTimeFormat)
	}
	// The name is durable once its directory is; a directory that cannot be synced leaves the
	// copy in place, and the caller's row is appended after this returns.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return id, path, at, nil
}

// auditResultCopyFor makes the copy a row carries and fills the row's id, copy path and digest.
// The copy is made from the bytes the grade read and validated (AuditResult.graded), not from a
// grade.json read again later: a batch grades several bundles before any row is recorded, and a
// file another process changed or removed in between must neither be copied as this result nor
// cost the row its copy. A result built by hand has no such bytes and is copied from the
// bundle's grade.json as it stands. A new ok row that has nothing to copy is refused by name and
// is not appended, so no ok row exists without its copy. The statuses that carry no result
// (invalid, timeout) have neither an id nor a copy.
func auditResultCopyFor(e *Env, cfg *Config, result AuditResult, row *auditLedgerRow) error {
	if result.Status != auditStatusOK {
		return nil
	}
	data := result.graded
	if data == nil {
		if result.Bundle == "" {
			return fmt.Errorf("result_copy_unavailable: the ok result of %s names no bundle to copy a result from", result.Subject)
		}
		read, err := os.ReadFile(crwconfig.JoinRoot(result.Bundle, auditGradeFile))
		if err != nil {
			return fmt.Errorf("result_copy_unavailable: the ok result of %s has no readable %s: %w", result.Subject, auditGradeFile, err)
		}
		data = read
	}
	id, path, gradedAt, err := auditResultCopyPublish(e, cfg, result.Subject, result.Head, result.GradedAt, data)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	row.ID, row.Result, row.ResultSHA256, row.GradedAt = id, path, hex.EncodeToString(sum[:]), gradedAt
	return nil
}

// auditResultCopyRead reads the copy a ledger row names and checks it against the row's digest,
// so a copy that was replaced or damaged is named rather than drafted from. A missing copy is a
// distinct, named reason.
func auditResultCopyRead(e *Env, cfg *Config, row auditLedgerRow) ([]byte, error) {
	if err := auditResultCopyName(row.ID); err != nil {
		return nil, err
	}
	path := auditResultCopyPath(e, cfg, row.ID)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("the result copy %s is missing", path)
		}
		return nil, err
	}
	sum := sha256.Sum256(data)
	if row.ResultSHA256 != "" && !strings.EqualFold(hex.EncodeToString(sum[:]), row.ResultSHA256) {
		return nil, fmt.Errorf("the result copy %s does not match the sha256 the row records", path)
	}
	return data, nil
}
