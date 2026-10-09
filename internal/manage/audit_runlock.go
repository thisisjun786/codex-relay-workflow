package manage

import (
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"syscall"
)

// auditRunLockFile is the lock one audit run holds for its whole run, below the audit state
// directory beside the ledger.
const auditRunLockFile = "audit-run.lock"

// auditRunLockPath is <state_dir>/audit/audit-run.lock.
func auditRunLockPath(e *Env, cfg *Config) string {
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditRunLockFile)
}

// auditRunLock takes the audit run lock: one flock on one file for the whole of an audit pr,
// audit package or audit round start run. Two such runs would read the ledger for their targets
// before either appended its rows, pick the same target, empty each other's bundle directory and
// leave two rows for one audit (CRW-838). The lock is non-blocking: a second run is refused by
// name at once rather than waiting. The file itself is never removed, because another process may
// hold it, and the open refuses to follow a link planted at its path.
func auditRunLock(e *Env, cfg *Config) (func(), error) {
	path := auditRunLockPath(e, cfg)
	if err := os.MkdirAll(rootDir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another audit run holds %s", path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
