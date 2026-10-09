package manage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// auditResultsDir is where the copies of graded results live, below the audit state directory:
// <state_dir>/audit/results/<row id>.json.
const auditResultsDir = "results"

// auditRowIDChars is how many leading hex characters of the digest a row id keeps.
const auditRowIDChars = 16

// auditRowID is the id of an ok ledger row: the first sixteen hex characters of
// sha256(target "\n" head "\n" gradedAt), the target being the row's subject. Two grades of one
// bundle differ in their gradedAt, so a regrade never shares a row's id (CRW-838).
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

// auditResultCopyWrite writes the bytes of a graded result to path and makes them visible only
// whole: a temporary file beside it is written and fsynced, then linked to the final name, so a
// reader never sees a half-written copy and an existing copy is never replaced. A copy that
// already exists is accepted only when its bytes are the same; any other content under the id is
// an error and the copy stays as it was.
func auditResultCopyWrite(path string, data []byte) error {
	dir := rootDir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		have, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(have, data) {
			return fmt.Errorf("result_copy_conflict: %s already holds another result", path)
		}
		return nil
	}
	// The name is durable once its directory is; a directory that cannot be synced leaves the
	// copy in place, and the caller's row is appended after this returns.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
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
	id := auditRowID(result.Subject, result.Head, result.GradedAt)
	path := auditResultCopyPath(e, cfg, id)
	if err := auditResultCopyWrite(path, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	row.ID, row.Result, row.ResultSHA256 = id, path, hex.EncodeToString(sum[:])
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
