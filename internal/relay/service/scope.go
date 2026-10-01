package service

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"golang.org/x/sys/unix"
)

type ScopeRegistry struct {
	Root, Authority string
	file            *os.File
}

func ResolveScope() (*ScopeRegistry, error) {
	if override := os.Getenv(ScopeEnv); override != "" {
		// Python's spelling, '..' included: K.lock and the isolated salt must name the
		// same file and key in both runtimes (cutover.md: Go reproduces the key).
		root, err := ownership.ScopeRoot(override)
		if err != nil {
			return nil, err
		}
		return &ScopeRegistry{Root: root, Authority: "isolated"}, nil
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
	return ownership.ScopeKeyIn(canonical, s.Root, s.Authority != "production")
}

// path is pathlib's root / f"{key}{suffix}": the root joined as spelled, never
// cleaned, so K.lock and K.json are the files the kernel resolves for Python's same
// spelling even when '..' follows a symlink (filepath.Join would drop the '..'
// lexically and name another file). The controller's barrier locks this same path.
func (s *ScopeRegistry) path(socket, suffix string) string {
	return lexicalJoin(s.Root, s.Key(socket)+suffix)
}

// lexicalJoin appends one name to a directory spelling without cleaning it.
func lexicalJoin(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
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
	// sorted(root.glob("*.json")): listed and read through the root as spelled.
	entries, _ := os.ReadDir(s.Root)
	for _, entry := range entries {
		if ok, _ := filepath.Match("*.json", entry.Name()); !ok {
			continue
		}
		r := read(lexicalJoin(s.Root, entry.Name()))
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

// ServedStore is what the scope claim for socket records about the store its relay service
// serves: the claim's stateDir, socketPath and storeId, whether the process that registered it
// still runs, and the claim's own path. It reads that one file, takes no lock and creates nothing;
// nil when the scope root cannot be named or no claim names a state directory.
func ServedStore(socket string) Object {
	scope, err := ResolveScope()
	if err != nil {
		return nil
	}
	r := scope.Read(socket)
	if text(get(r, "stateDir")) == "" {
		return nil
	}
	return obj("stateDirectory", get(r, "stateDir"), "socketPath", get(r, "socketPath"), "storeId", get(r, "storeId"),
		"live", live(r), "scopeRecord", scope.path(socket, ".json"))
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
	if err := os.MkdirAll(ownership.LexicalDir(path), 0700); err != nil {
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
