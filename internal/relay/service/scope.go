package service

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

type ScopeRegistry struct {
	Root, Authority string
	file            *os.File
}

func ResolveScope() (*ScopeRegistry, error) {
	if override := os.Getenv(ScopeEnv); override != "" {
		path, err := store.ExpandUser(override)
		if err != nil {
			return nil, err
		}
		path, err = filepath.Abs(path)
		return &ScopeRegistry{Root: path, Authority: "isolated"}, err
	}
	current, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return nil, err
	}
	return &ScopeRegistry{Root: filepath.Join(current.HomeDir, ".codex-session-relay", "scopes"), Authority: "production"}, nil
}
func (s *ScopeRegistry) Key(socket string) string {
	canonical, err := store.CanonicalSocket(socket)
	if err != nil {
		canonical = socket
	}
	hash := sha256.Sum256([]byte(canonical))
	key := fmt.Sprintf("%x", hash[:8])
	if s.Authority != "production" {
		salt := sha256.Sum256([]byte(s.Root))
		key = fmt.Sprintf("isolated-%x-%s", salt[:4], key)
	}
	return key
}
func (s *ScopeRegistry) path(socket, suffix string) string {
	return filepath.Join(s.Root, s.Key(socket)+suffix)
}
func (s *ScopeRegistry) Read(socket string) Object { return read(s.path(socket, ".json")) }
func (s *ScopeRegistry) Prepare() error {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	info, err := os.Stat(s.Root)
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s is owned by uid %d, not this user", s.Root, stat.Uid)
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%s is group or world writable", s.Root)
	}
	return nil
}
func (s *ScopeRegistry) Claim(socket string, record Object) (Object, error) {
	return s.claim(socket, func() Object { return record }, nil)
}

// The store opener runs after both permanent-inode locks are held, before any
// registration is published. Its freshly read identity is used for the claim.
func (s *ScopeRegistry) claim(socket string, recordOf func() Object, prepare func() error) (Object, error) {
	record := recordOf()
	if err := s.Prepare(); err != nil {
		return nil, err
	}
	existing := s.Read(socket)
	if truth(get(existing, "storeId")) && truth(get(record, "storeId")) && !equal(get(existing, "storeId"), get(record, "storeId")) && !truth(get(record, "takeover")) {
		return obj("ok", false, "reason", "scope_registered_to_other_store", "held_by", existing, "scopeKey", s.Key(socket)), nil
	}
	f, err := lockIfFree(s.path(socket, ".lock"))
	if err != nil {
		return nil, err
	}
	if f == nil {
		return obj("ok", false, "reason", "scope_owned_by_other_store", "held_by", s.Read(socket), "scopeKey", s.Key(socket)), nil
	}
	if prepare != nil {
		if err = prepare(); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		record = recordOf()
		if truth(get(existing, "storeId")) && truth(get(record, "storeId")) && !equal(get(existing, "storeId"), get(record, "storeId")) && !truth(get(record, "takeover")) {
			return obj("ok", false, "reason", "scope_registered_to_other_store", "held_by", existing, "scopeKey", s.Key(socket)), f.Close()
		}
	}
	s.file = f
	payload := set(record, "scopeKey", s.Key(socket), "scopeAuthority", s.Authority, "socketPath", socket, "registeredAt", stamp())
	if err = atomicWrite(s.path(socket, ".json"), payload); err != nil {
		s.file = nil
		return nil, errors.Join(err, f.Close())
	}
	return obj("ok", true, "reason", nil, "record", payload, "lockFd", int(f.Fd())), nil
}
func (s *ScopeRegistry) Release(socket string) error {
	var err error
	if r := s.Read(socket); r != nil {
		err = atomicWrite(s.path(socket, ".json"), set(r, "pid", nil, "workerPid", nil, "startedAt", nil, "releasedAt", stamp()))
	}
	if s.file != nil {
		err = errors.Join(err, s.file.Close())
		s.file = nil
	}
	return err
}
func (s *ScopeRegistry) Conflicts(socket, storeID, state string) []any {
	out := []any{}
	entries, _ := filepath.Glob(filepath.Join(s.Root, "*.json"))
	for _, path := range entries {
		r := read(path)
		if text(get(r, "scopeKey")) != s.Key(socket) {
			continue
		}
		if storeID != "" && text(get(r, "storeId")) == storeID && text(get(r, "stateDir")) == state {
			continue
		}
		out = append(out, obj("stateDir", get(r, "stateDir"), "storeId", get(r, "storeId"), "installationId", get(r, "installationId"), "pid", get(r, "pid"), "live", live(r), "reason", "same_scope_different_store"))
	}
	return out
}
func live(r Object) bool {
	pid := num(get(r, "pid"))
	if pid == 0 {
		return false
	}
	if b := get(r, "bootId"); b != nil && !equal(b, BootID()) {
		return false
	}
	if state := ProcessState(pid); state == "" || state == "Z" {
		return false
	}
	ticks := StartTicks(pid)
	return ticks != nil && (get(r, "startTicks") == nil || equal(ticks, get(r, "startTicks")))
}

// Only flock contention is ownership evidence; an operational error stays an error.
func lockIfFree(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		closeErr := f.Close()
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return nil, closeErr
		}
		return nil, errors.Join(err, closeErr)
	}
	return f, nil
}
func (s *Service) LockIsHeld() bool {
	if _, err := os.Stat(s.path("daemon.lock")); err != nil {
		return false
	}
	f, err := lockIfFree(s.path("daemon.lock"))
	if err != nil {
		return false
	}
	if f == nil {
		return true
	}
	_ = f.Close()
	return false
}
func (s *Service) Conflicts() []any {
	if s.Socket == "" {
		return []any{}
	}
	return s.Scope.Conflicts(s.Socket, s.StoreID, s.Selection.Path)
}
