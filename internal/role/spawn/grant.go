package spawn

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// spawnGrantToken is the dispatcher's request for one child spawn with recursion; the grant
// functions below never read it, the hook that mints on it does.
const spawnGrantToken = "CRW-SUBSPAWN-ALLOWED"

// spawnGrantMarker opens the one-time capability a parent places in the spawn message: the
// marker, 64 hex digits and a closing bracket. The oracle matches its letters without regard to case.
const spawnGrantMarker = "[CRW-SUBSPAWN-GRANT:"

const spawnGrantTTL = 15 * time.Minute

// IsSubagentSpawner is true when the hook stdin names a collab subagent as the spawner: a
// non-empty string agent_id or agent_type.
func IsSubagentSpawner(obj map[string]any) bool {
	id, _ := obj["agent_id"].(string)
	kind, _ := obj["agent_type"].(string)
	return id != "" || kind != ""
}

// MintRecursionGrant writes one 15-minute grant for the spawner's cwd and session under
// tmpRoot and returns its nonce. Unlike the oracle it uses the per-uid and key directories only
// when they are real directories owned by the current uid with mode 0700; otherwise it returns
// no grant. The caller chooses tmpRoot (the oracle's os.tmpdir() reads TMPDIR, TMP and TEMP in
// that order, os.TempDir only TMPDIR) and the clock.
func MintRecursionGrant(obj map[string]any, tmpRoot string, now time.Time) (string, bool) {
	return spawnGrantMint(obj, tmpRoot, os.Getuid(), now)
}

// ConsumeRecursionGrant spends the grant a spawn message names. The message must carry exactly
// one grant marker; the grant is renamed away, read, deleted, and the answer is whether it had
// not expired at now. A directory that fails the checks of MintRecursionGrant answers false.
func ConsumeRecursionGrant(obj map[string]any, message, tmpRoot string, now time.Time) bool {
	return spawnGrantConsume(obj, message, tmpRoot, os.Getuid(), now)
}

func spawnGrantMint(obj map[string]any, tmpRoot string, uid int, now time.Time) (string, bool) {
	key, ok := spawnGrantKey(obj)
	if !ok {
		return "", false
	}
	dir := spawnGrantOpen(tmpRoot, uid, key, true)
	if dir == nil {
		return "", false
	}
	defer dir.Close()
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	nonce := hex.EncodeToString(raw[:])
	if !spawnGrantWrite(dir, nonce, now) {
		return "", false
	}
	return nonce, true
}

// spawnGrantWrite creates the grant file exclusively, so a name that exists is left alone.
func spawnGrantWrite(dir *os.Root, nonce string, now time.Time) bool {
	name := spawnGrantFile(nonce)
	file, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_, err = file.WriteString("{\"expiresAt\":" + strconv.FormatInt(now.Add(spawnGrantTTL).UnixMilli(), 10) + "}")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = dir.Remove(name)
		return false
	}
	return true
}

func spawnGrantConsume(obj map[string]any, message, tmpRoot string, uid int, now time.Time) bool {
	nonce, ok := spawnGrantOnlyMarker(message)
	if !ok {
		return false
	}
	key, ok := spawnGrantKey(obj)
	if !ok {
		return false
	}
	dir := spawnGrantOpen(tmpRoot, uid, key, false)
	if dir == nil {
		return false
	}
	defer dir.Close()
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	claimed := spawnGrantFile(nonce) + ".claimed-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(suffix[:])
	if dir.Rename(spawnGrantFile(nonce), claimed) != nil {
		return false
	}
	defer func() { _ = dir.Remove(claimed) }()
	return spawnGrantUnexpired(dir, claimed, now)
}

// spawnGrantUnexpired reads a claimed grant as JSON.parse followed by a typeof-number test does: one
// JSON object with no trailing text whose expiresAt is a number, an overflowing exponent reading
// as infinity, not before now; the decoder alone refuses nesting beyond 10,000 levels, which
// JSON.parse accepts (known-defects.md). The file must be a regular file reached without a link.
func spawnGrantUnexpired(dir *os.Root, name string, now time.Time) bool {
	named, err := dir.Lstat(name)
	if err != nil || !named.Mode().IsRegular() {
		return false
	}
	file, err := dir.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(named, opened) {
		return false
	}
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var body map[string]any
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	expires, ok := body["expiresAt"].(json.Number)
	if !ok {
		return false
	}
	millis, _ := strconv.ParseFloat(expires.String(), 64)
	return millis >= float64(now.UnixMilli())
}

// spawnGrantOnlyMarker returns the nonce of the one grant marker in message; no marker or more
// than one is false. A marker is searched from every byte, as the global regular expression
// retries after a failed candidate, and its nonce keeps the case it was written in: it is hashed
// as it stands.
func spawnGrantOnlyMarker(message string) (string, bool) {
	found, matches, width := "", 0, len(spawnGrantMarker)+64
	for i := 0; i+width+1 <= len(message); i++ {
		if message[i+width] != ']' || !spawnGrantFold(message[i:i+len(spawnGrantMarker)], spawnGrantMarker) || !spawnGrantHex(message[i+len(spawnGrantMarker):i+width]) {
			continue
		}
		if matches++; matches > 1 {
			return "", false
		}
		found = message[i+len(spawnGrantMarker) : i+width]
	}
	return found, matches == 1
}

// spawnGrantFold compares ASCII letters without case, like a JavaScript /i pattern without the u
// flag: U+017F and U+212A do not match s and k as Unicode folding would have them.
func spawnGrantFold(got, want string) bool {
	for i := 0; i < len(want); i++ {
		a, b := got[i], want[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

func spawnGrantHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// spawnGrantKey is the key directory name: sha256 of the resolved cwd, a NUL and the session, as
// UTF-8 in which a lone surrogate (kept as WTF-8 by this repository's JSON) reads as U+FFFD.
func spawnGrantKey(obj map[string]any) (string, bool) {
	cwd, session, ok := spawnGrantScope(obj)
	if !ok {
		return "", false
	}
	sum := sha256.New()
	sum.Write(spawnGrantUTF8(cwd))
	sum.Write([]byte{0})
	sum.Write(spawnGrantUTF8(session))
	return hex.EncodeToString(sum.Sum(nil)), true
}

// spawnGrantScope resolves the spawner's cwd like path.resolve: an absolute one is cleaned, a
// relative or missing one is joined to the kernel's working directory (syscall.Getwd, which is
// what process.cwd() answers; os.Getwd would trust $PWD). The session is unknown-session when
// absent. The oracle throws when the working directory cannot be read; here there is no grant.
func spawnGrantScope(obj map[string]any) (cwd, session string, ok bool) {
	cwd, _ = obj["cwd"].(string)
	if !filepath.IsAbs(cwd) {
		wd, err := syscall.Getwd()
		if err != nil {
			return "", "", false
		}
		cwd = filepath.Join(wd, cwd)
	}
	if session, _ = obj["session_id"].(string); session == "" {
		session = "unknown-session"
	}
	return filepath.Clean(cwd), session, true
}

func spawnGrantUTF8(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
		i += size
		out = utf8.AppendRune(out, r) // a surrogate is no valid rune: AppendRune writes U+FFFD for it
	}
	return out
}

func spawnGrantDirName(uid int) string {
	if uid < 0 {
		return "crw-subspawn-user"
	}
	return "crw-subspawn-" + strconv.Itoa(uid)
}

func spawnGrantFile(nonce string) string {
	digest := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(digest[:]) + ".json"
}

// spawnGrantOpen opens <tmpRoot>/crw-subspawn-<uid>/<key>, creating the two directories when
// create is set, and returns it only when each level is a real directory owned by uid with mode
// 0700. Every step runs on the handle the previous one opened, so a link or a directory another
// user planted in a shared tmpRoot is never followed, written through or opened.
func spawnGrantOpen(tmpRoot string, uid int, key string, create bool) *os.Root {
	if tmpRoot == "" {
		return nil
	}
	root, err := os.OpenRoot(tmpRoot)
	if err != nil {
		return nil
	}
	for _, name := range []string{spawnGrantDirName(uid), key} {
		next := spawnGrantPin(root, name, uid, create)
		root.Close()
		if next == nil {
			return nil
		}
		root = next
	}
	return root
}

// spawnGrantPin checks the entry before opening it, because opening a FIFO or a device another
// user planted would block, and again on the opened handle, which must be the checked entry.
func spawnGrantPin(parent *os.Root, name string, uid int, create bool) *os.Root {
	if create {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil
		}
	}
	named, err := parent.Lstat(name)
	if err != nil || !spawnGrantOwned(named, uid) {
		return nil
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil
	}
	opened, err := dir.Stat(".")
	if err != nil || !spawnGrantOwned(opened, uid) || !os.SameFile(named, opened) {
		dir.Close()
		return nil
	}
	return dir
}

// spawnGrantOwned is true for a real directory, not a link, owned by uid with mode 0700.
func spawnGrantOwned(info os.FileInfo, uid int) bool {
	stat, _ := info.Sys().(*syscall.Stat_t)
	return info.IsDir() && stat != nil && int(stat.Uid) == uid && info.Mode().Perm() == 0o700
}
