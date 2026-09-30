// Package ownership implements the fenced, never time-based, writer protocol.
// schema_meta is authoritative; takeover.json is only an admission mirror.
package ownership

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const Protocol = 1
const PythonBuild = "codex-session-relay/0.2.0"

var Keys = []string{"writer_protocol", "owner", "owner_epoch", "takeover_id", "rollback_allowed", "python_compatibility_build"}

// Refused is an ownership refusal. Queueable marks the three the fence answers a queueable
// command's refusal by publishing it in the takeover inbox instead (ownership.py
// OwnershipRefused.queueable, decision 25): another runtime owns the store, it is draining, or
// it is starting and this process is not the designated candidate.
type Refused struct {
	Detail    string
	Queueable bool
}

func (e *Refused) Error() string              { return "ownership refused: " + e.Detail }
func refuse(format string, args ...any) error { return &Refused{Detail: fmt.Sprintf(format, args...)} }
func queueable(detail string) error           { return &Refused{Detail: detail, Queueable: true} }

type Database struct {
	RealPath           string `json:"realPath"`
	Device             uint64 `json:"device"`
	Inode              uint64 `json:"inode"`
	WALDirectoryDevice uint64 `json:"walDirectoryDevice"`
	WALDirectoryInode  uint64 `json:"walDirectoryInode"`
	WALBasename        string `json:"walBasename"`
}
type Identity struct {
	BootID     string `json:"bootId"`
	PID        int    `json:"pid"`
	StartTicks int64  `json:"startTicks"`
	Build      string `json:"build,omitempty"`
}
type Transition struct {
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	TargetEpoch int64  `json:"targetEpoch"`
}
type Record struct {
	Protocol                 int         `json:"protocol"`
	StoreID                  string      `json:"storeId"`
	Database                 Database    `json:"database"`
	AppServerSocket          *string     `json:"appServerSocket"`
	ScopeKey                 *string     `json:"scopeKey"`
	Epoch                    int64       `json:"epoch"`
	Owner                    string      `json:"owner"`
	Phase                    string      `json:"phase"`
	Transition               *Transition `json:"transition"`
	Holder                   *Identity   `json:"holder"`
	Controller               *Identity   `json:"controller"`
	RollbackAllowed          bool        `json:"rollbackAllowed"`
	PythonCompatibilityBuild string      `json:"pythonCompatibilityBuild"`
	RelayRPCSocket           string      `json:"relayRPCSocket"`
	UpdatedAt                string      `json:"updatedAt"`
}
type Stamp struct {
	Protocol                 int    `json:"protocol"`
	StoreID                  string `json:"storeId"`
	Owner                    string `json:"owner"`
	Epoch                    int64  `json:"epoch"`
	TakeoverID               string `json:"takeoverId"`
	RollbackAllowed          bool   `json:"rollbackAllowed"`
	PythonCompatibilityBuild string `json:"pythonCompatibilityBuild"`
	SocketPath               string `json:"socketPath"`
}

type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func Physical(path string) (Database, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Database{}, err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return Database{}, err
	}
	var file, dir unix.Stat_t
	if err = unix.Stat(real, &file); err != nil {
		return Database{}, err
	}
	if file.Mode&unix.S_IFMT != unix.S_IFREG {
		return Database{}, refuse("database is not a regular file")
	}
	if err = unix.Stat(filepath.Dir(real), &dir); err != nil {
		return Database{}, err
	}
	return Database{real, uint64(file.Dev), file.Ino, uint64(dir.Dev), dir.Ino, filepath.Base(real)}, nil
}

// resolvedParent is Python's Path(path).resolve().parent: the state directory both
// runtimes name, however it was reached. A symlinked state directory is the same S.
func resolvedParent(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if real, err = filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			real = filepath.Join(real, filepath.Base(path))
		}
	}
	if err != nil {
		return "", err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", err
	}
	return filepath.Dir(real), nil
}

func ReadRecord(path string) (Record, error) {
	dir, err := resolvedParent(path)
	if err != nil {
		return Record{}, refuse("resolve state directory: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "takeover.json"))
	if err != nil {
		return Record{}, refuse("read mirror: %v", err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return Record{}, refuse("decode mirror: %v", err)
	}
	for _, key := range []string{"protocol", "storeId", "database", "appServerSocket", "scopeKey", "epoch", "owner", "phase", "transition", "holder", "controller", "rollbackAllowed", "pythonCompatibilityBuild", "relayRPCSocket", "updatedAt"} {
		if _, ok := fields[key]; !ok {
			return Record{}, refuse("mirror missing %s", key)
		}
	}
	var r Record
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, refuse("decode mirror: %v", err)
	}
	if string(fields["rollbackAllowed"]) != "true" && string(fields["rollbackAllowed"]) != "false" {
		return r, refuse("invalid rollbackAllowed")
	}
	if r.Protocol != Protocol || r.Epoch < 1 || !owner(r.Owner) || r.StoreID == "" || r.PythonCompatibilityBuild != PythonBuild {
		return r, refuse("unsupported or incomplete mirror")
	}
	if r.Phase != "active" && r.Phase != "draining" && r.Phase != "starting" {
		return r, refuse("invalid phase")
	}
	if r.RelayRPCSocket != filepath.Join(dir, "control.sock") {
		return r, refuse("wrong control socket")
	}
	if r.Transition != nil {
		t := r.Transition
		if t.ID == "" || t.ID == "." || t.ID == ".." || strings.ContainsAny(t.ID, "/\\\\") || !owner(t.From) || !owner(t.To) || t.From == t.To || t.TargetEpoch < 2 {
			return r, refuse("invalid transition")
		}
	}
	if r.Phase != "active" && r.Transition == nil {
		return r, refuse("transition missing")
	}
	return r, nil
}
func owner(s string) bool { return s == "python" || s == "go" }

func ReadStamp(ctx context.Context, db Queryer) (Stamp, error) {
	rows, err := db.QueryContext(ctx, "SELECT key,value FROM schema_meta")
	if err != nil {
		return Stamp{}, refuse("read durable ownership: %v", err)
	}
	defer rows.Close()
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			return Stamp{}, err
		}
		meta[k] = v
	}
	if err = rows.Err(); err != nil {
		return Stamp{}, err
	}
	for _, k := range Keys {
		if _, ok := meta[k]; !ok {
			return Stamp{}, refuse("durable ownership missing %s", k)
		}
	}
	epoch, err := strconv.ParseInt(meta["owner_epoch"], 10, 64)
	if err != nil || epoch < 1 || strconv.FormatInt(epoch, 10) != meta["owner_epoch"] {
		return Stamp{}, refuse("invalid durable epoch")
	}
	if meta["writer_protocol"] != "1" || !owner(meta["owner"]) || meta["store_id"] == "" || meta["version"] != "1" || meta["python_compatibility_build"] != PythonBuild || (meta["rollback_allowed"] != "0" && meta["rollback_allowed"] != "1") {
		return Stamp{}, refuse("unsupported durable ownership/schema")
	}
	return Stamp{Protocol, meta["store_id"], meta["owner"], epoch, meta["takeover_id"], meta["rollback_allowed"] == "1", meta["python_compatibility_build"], meta["socket_path"]}, nil
}

// OpenExisting never creates a database or executes initialization DDL.
func OpenExisting(ctx context.Context, path, mode string) (*sql.DB, error) {
	if _, err := Physical(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", mode)
	q.Set("_pragma", "busy_timeout(0)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// ScopeKey is the scope-registry key of a canonical socket path (service.py
// ScopeRegistry.key): the first 16 hex digits of its SHA-256, namespaced as
// isolated-<salt>- when CODEX_SESSION_RELAY_SCOPE_DIR overrides the registry root.
func ScopeKey(socket string) (string, error) {
	hash := sha256.Sum256([]byte(socket))
	key := fmt.Sprintf("%x", hash[:8])
	override := os.Getenv("CODEX_SESSION_RELAY_SCOPE_DIR")
	if override == "" {
		return key, nil
	}
	root, err := ScopeRoot(override)
	if err != nil {
		return "", err
	}
	salt := sha256.Sum256([]byte(root))
	return fmt.Sprintf("isolated-%x-%s", salt[:4], key), nil
}

// ScopeRoot is str(Path(override).expanduser().absolute()) (service.py
// resolve_scope_root), the spelling both runtimes salt isolated keys with and name
// K.lock by. It is lexical: '..' and symlinks are kept, never cleaned or resolved.
func ScopeRoot(override string) (string, error) {
	path := override
	if strings.HasPrefix(path, "~") {
		name, rest, _ := strings.Cut(path[1:], "/")
		home, err := UserHome(name)
		if err != nil {
			return "", err
		}
		// pathlib joins the remaining components to the home (with_segments).
		path = home
		if rest = strings.TrimLeft(rest, "/"); rest != "" {
			path = strings.TrimSuffix(home, "/") + "/" + rest
		}
	}
	if !strings.HasPrefix(path, "/") {
		// Python's getcwd is the physical directory; os.Getwd prefers $PWD.
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		path = JoinCwd(cwd, path)
	}
	return PathlibSpelling(path), nil
}

// JoinCwd is a relative path joined to the working directory as os.path.join and pathlib join it:
// under the root it gains no second slash, which PathlibSpelling and normpath would keep as a root
// of two slashes.
func JoinCwd(cwd, path string) string {
	if strings.HasSuffix(cwd, "/") {
		return cwd + path
	}
	return cwd + "/" + path
}

// ErrNoHome is pathlib's RuntimeError("Could not determine home directory."): a ~ or ~user that
// nothing answers.
var ErrNoHome = errors.New("could not determine home directory")

// UserHome is the home posixpath.expanduser puts in place of a leading ~name, "" naming ~ alone:
// HOME whenever HOME is set, else this user's passwd entry (pwd.getpwuid(os.getuid())), and for a
// name that user's passwd entry. Trailing slashes are stripped, and a home that is nothing (an
// empty HOME, or HOME="/") is the root. Where nothing answers, the error wraps ErrNoHome.
func UserHome(name string) (string, error) {
	var home string
	if name == "" {
		if value, ok := os.LookupEnv("HOME"); ok {
			home = value
		} else if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
			home = u.HomeDir
		} else {
			return "", fmt.Errorf("%w: %w", ErrNoHome, err)
		}
	} else {
		u, err := user.Lookup(name)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrNoHome, err)
		}
		home = u.HomeDir
	}
	if home = strings.TrimRight(home, "/"); home == "" {
		home = "/"
	}
	return home, nil
}

// PathlibSpelling is str(PurePosixPath(path)): empty and '.' components are dropped, every '..'
// is kept, and exactly two leading slashes remain a distinct root (POSIX leaves "//"
// implementation-defined) where three or more fold to one.
func PathlibSpelling(path string) string {
	root := ""
	if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
		root = "//"
	} else if strings.HasPrefix(path, "/") {
		root = "/"
	}
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if root == "" && len(parts) == 0 {
		return "."
	}
	return root + strings.Join(parts, "/")
}

func Validate(path string, r Record, s Stamp) error {
	if r.AppServerSocket == nil {
		if r.ScopeKey != nil || s.SocketPath != "" {
			return refuse("scope without socket")
		}
	} else {
		socket := *r.AppServerSocket
		if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || r.ScopeKey == nil || socket != s.SocketPath {
			return refuse("invalid socket/scope identity")
		}
		key, err := ScopeKey(socket)
		if err != nil {
			return err
		}
		if *r.ScopeKey != key {
			return refuse("scope key disagrees with lock authority")
		}
	}
	physical, err := Physical(path)
	if err != nil {
		return err
	}
	if r.Database != physical || r.StoreID != s.StoreID || r.Protocol != s.Protocol || r.PythonCompatibilityBuild != s.PythonCompatibilityBuild {
		return refuse("physical store or compatibility identity disagrees")
	}
	if r.Owner != s.Owner || r.Epoch != s.Epoch || (r.RollbackAllowed != s.RollbackAllowed && !committedTear(r, s)) {
		return refuse("mirror disagrees with durable ownership")
	}
	if r.Transition != nil {
		t := r.Transition
		if r.Phase == "draining" {
			if t.From != s.Owner || t.TargetEpoch != s.Epoch+1 {
				return refuse("drain transition disagrees")
			}
		} else if t.ID != s.TakeoverID || t.To != s.Owner || t.TargetEpoch != s.Epoch {
			return refuse("installed transition disagrees")
		}
	} else if s.TakeoverID != "" {
		return refuse("installed transition missing")
	}
	return nil
}

// committedTear is the one rollbackAllowed disagreement every admitted Go writer
// accepts: takeover commit's durable 1->0 write before its mirror publication. The DB
// is authoritative, owner and epoch are unchanged, and the flag never returns to 1;
// only the controller republishes the mirror (cutover.md Commit point).
func committedTear(r Record, s Stamp) bool {
	return r.RollbackAllowed && !s.RollbackAllowed && r.Owner == "go" && s.Owner == "go" && r.Epoch == s.Epoch && r.Phase == "active"
}

// InitialRecord is the mirror an initial stamp implies, derived only from the stamp and the
// physical store (cutover.md Record, Torn publications): protocol 1, storeId, database,
// appServerSocket and scopeKey from socket_path (both null without one), epoch 1, the stamped
// owner, phase active, no transition, holder or controller, rollbackAllowed and
// pythonCompatibilityBuild from the stamp, and relayRPCSocket S/control.sock. It is what an
// absent-store initializer publishes after its COMMIT (Python Admission.initialize, Go
// createAbsent), and what Controller.RepairMirror publishes when that publication was lost.
func InitialRecord(path string, s Stamp) (Record, error) {
	physical, err := Physical(path)
	if err != nil {
		return Record{}, err
	}
	record := Record{Protocol: Protocol, StoreID: s.StoreID, Database: physical, Epoch: 1, Owner: s.Owner, Phase: "active", RollbackAllowed: s.RollbackAllowed, PythonCompatibilityBuild: s.PythonCompatibilityBuild, RelayRPCSocket: filepath.Join(filepath.Dir(physical.RealPath), "control.sock")}
	if s.SocketPath != "" {
		key, err := ScopeKey(s.SocketPath)
		if err != nil {
			return Record{}, err
		}
		socket := s.SocketPath
		record.AppServerSocket, record.ScopeKey = &socket, &key
	}
	return record, nil
}

// Publish implements write, fsync, close, rename, directory fsync. Fault is a
// deterministic crash boundary seam; production callers leave it nil.
func Publish(path string, r Record, fault func(string) error) (err error) {
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return publishFile(filepath.Join(filepath.Dir(path), "takeover.json"), raw, fault)
}
func hit(f func(string) error, point string) error {
	if f != nil {
		return f(point)
	}
	return nil
}
func publishFile(path string, raw []byte, fault func(string) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".takeover-")
	if err != nil {
		return err
	}
	defer func() {
		if e := os.Remove(f.Name()); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if _, err = f.Write(raw); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = hit(fault, "temp-written"); err != nil {
		return errors.Join(err, f.Close())
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = hit(fault, "file-synced"); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err = hit(fault, "renamed"); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return hit(fault, "directory-synced")
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// SnapshotMeta reads a disposable copy including WAL; a refused admission must
// not create -shm or change any byte beside the original DB. A concurrent writer
// can make the copy inconsistent: that refuses, never repairs the source.
func SnapshotMeta(ctx context.Context, path string) (Stamp, error) {
	dst, cleanup, err := CopySnapshot(path)
	if err != nil {
		return Stamp{}, err
	}
	defer cleanup()
	db, err := OpenExisting(ctx, dst, "ro")
	if err != nil {
		return Stamp{}, err
	}
	stamp, err := ReadStamp(ctx, db)
	return stamp, errors.Join(err, db.Close())
}

// CopySnapshot is for read-only preflight only, NOT the transfer backup. The
// latter uses sqlite3_backup under the complete transfer barrier.
func CopySnapshot(path string) (dst string, cleanup func() error, err error) {
	dir, err := os.MkdirTemp("", "crw-ownership-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() error { return os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	dst = filepath.Join(dir, filepath.Base(path))
	for _, suffix := range []string{"", "-wal"} {
		src, e := os.Open(path + suffix)
		if errors.Is(e, os.ErrNotExist) && suffix != "" {
			continue
		}
		if e != nil {
			return "", nil, e
		}
		out, e := os.OpenFile(dst+suffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return "", nil, errors.Join(e, src.Close())
		}
		_, e = io.Copy(out, src)
		e = errors.Join(e, src.Close(), out.Close())
		if e != nil {
			return "", nil, e
		}
	}
	return dst, cleanup, nil
}
