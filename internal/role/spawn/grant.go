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
// one grant marker; the grant is checked, reserved by a rename, checked again and removed, and the
// answer is whether it was unexpired at now. A directory that fails the checks of MintRecursionGrant
// answers false. The spawn hook splits the same steps around its other refusals (spawnGrantCheck).
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
	claim, ok := spawnGrantCheck(obj, message, tmpRoot, uid, now, "", "")
	if !ok || !claim.reserve(now) {
		return false
	}
	claim.commit()
	return true
}

// spawnGrantClaim is a grant a spawn may use, checked without being spent (CRW-1118). The hook checks it first, runs every
// refusal it can meet, and only then reserves it (a rename, so of two calls only one gets it) and commits it with the
// rest of its answer; a refusal after the check leaves the grant to the corrected retry. With the native tool use id the
// reservation and the spent record are bound to that call and to the input it was made with: the hook records the call's input and
// answer when it spends the grant (hook_replay.go), so a second delivery of the same call gets that answer before it comes here,
// while a delivery of that id with another input, or another call, finds nothing to use. A reservation a delivery left behind is
// finished only by the call and the input it was made for (the input is written beside it before the rename). Without a tool use id,
// the reservation has a name of its own and the spent grant is removed.
type spawnGrantClaim struct {
	tmpRoot, key, nonce string
	uid                 int
	tag                 string // the call's tag: a digest of the tool use id, or a unique name for a call without one
	bound               bool   // the tag is the call's tool use id, so its records are found again
	input               string // the call's tool_input as JSON.stringify writes it, "" when it has none to record
	held                string // the reservation this claim holds, once reserved
	committed           bool
}

// spawnGrantTag is the name part of a call's reservation and spent record: a digest of its tool use id, so the id itself is
// never a file name, or a unique name for a call without one.
func spawnGrantTag(toolUseID string) (string, bool) {
	if toolUseID != "" {
		sum := sha256.Sum256([]byte(toolUseID))
		return hex.EncodeToString(sum[:16]), true
	}
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	return "anon-" + strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(suffix[:]), false
}

func (c *spawnGrantClaim) reservedName() string {
	return spawnGrantFile(c.nonce) + ".reserved-" + c.tag
}
func (c *spawnGrantClaim) spentName() string { return spawnGrantFile(c.nonce) + ".used-" + c.tag }

// inputName is the record of the input a reservation was made for.
func (c *spawnGrantClaim) inputName() string { return spawnGrantFile(c.nonce) + ".input-" + c.tag }

// spawnGrantCheck finds the grant the message names for this call without spending it: this call's reservation left by a delivery
// that stopped before it committed (for the input it was made for), or an unexpired grant. It reads without a lock and decides
// nothing on its own: reserve is the step two calls cannot both pass. input is the call's tool_input as JSON.stringify writes it, ""
// for none. A call that spent its grant is answered before this, from its record; here its spent grant authorizes nothing.
func spawnGrantCheck(obj map[string]any, message, tmpRoot string, uid int, now time.Time, toolUseID, input string) (*spawnGrantClaim, bool) {
	nonce, ok := spawnGrantOnlyMarker(message)
	if !ok {
		return nil, false
	}
	key, ok := spawnGrantKey(obj)
	if !ok {
		return nil, false
	}
	dir := spawnGrantOpen(tmpRoot, uid, key, false)
	if dir == nil {
		return nil, false
	}
	defer dir.Close()
	c := &spawnGrantClaim{tmpRoot: tmpRoot, key: key, nonce: nonce, uid: uid, input: input}
	c.tag, c.bound = spawnGrantTag(toolUseID)
	if c.bound && spawnGrantRegular(dir, c.reservedName()) {
		// The reservation belongs to this call, for the input recorded beside it: no record, or another input, is a reservation nobody
		// can say what it was made for, and it stays where it is.
		if recorded, ok := spawnGrantReadInput(dir, c.inputName()); !ok || input == "" || recorded != input {
			return nil, false
		}
		c.held = c.reservedName()
		return c, true
	}
	if spawnGrantUnexpired(dir, spawnGrantFile(nonce), now) {
		return c, true
	}
	spawnGrantBeforeCleanup()
	// A grant that can never be used (expired, or not the minted shape) is spent as the oracle spends it, by a rename to a claimed
	// name and a removal, so it does not stay behind. The file is judged again after it is taken: a name that was empty when it was
	// judged is a grant another call holds, and a valid one that came back since (that call released it) is given back under its name
	// and answers this call as well, never removed (CRW-1118).
	if _, err := dir.Lstat(spawnGrantFile(nonce)); err != nil {
		return nil, false // nothing is there to spend
	}
	claimed := spawnGrantFile(nonce) + ".claimed-" + c.tag
	if dir.Rename(spawnGrantFile(nonce), claimed) != nil {
		return nil, false
	}
	if spawnGrantUnexpired(dir, claimed, now) {
		if dir.Rename(claimed, spawnGrantFile(nonce)) != nil {
			_ = dir.Remove(claimed)
			return nil, false
		}
		return c, true
	}
	_ = dir.Remove(claimed)
	return nil, false
}

// spawnGrantBeforeCleanup runs between the judgement that a grant cannot be used and the clean-up of the file; a test acts there.
var spawnGrantBeforeCleanup = func() {}

// spawnGrantReadInput reads the input a reservation was made for.
func spawnGrantReadInput(dir *os.Root, name string) (string, bool) {
	if !spawnGrantRegular(dir, name) {
		return "", false
	}
	file, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > spawnHookReplayMax {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(file, spawnHookReplayMax+1))
	return string(data), err == nil
}

// reserve takes the grant for this call: the input it is taken for is recorded beside it, then a rename of the grant file to the
// call's reservation, which only one call can make, followed by a second check of the file it renamed. A reservation this call
// already holds passes.
func (c *spawnGrantClaim) reserve(now time.Time) bool {
	if c.held != "" {
		return true
	}
	dir := spawnGrantOpen(c.tmpRoot, c.uid, c.key, false)
	if dir == nil {
		return false
	}
	defer dir.Close()
	name := c.reservedName()
	recorded := false
	if c.bound && c.input != "" {
		recorded = spawnGrantWriteInput(dir, c.inputName(), c.input)
	}
	if dir.Rename(spawnGrantFile(c.nonce), name) != nil {
		if recorded {
			_ = dir.Remove(c.inputName())
		}
		return false
	}
	if !spawnGrantUnexpired(dir, name, now) {
		_ = dir.Remove(name)
		if recorded {
			_ = dir.Remove(c.inputName())
		}
		return false
	}
	c.held = name
	return true
}

// spawnGrantWriteInput writes the input of a reservation whole, replacing a record of an earlier try of the same call.
func spawnGrantWriteInput(dir *os.Root, name, input string) bool {
	tmp := name + ".tmp"
	file, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false
	}
	_, err = file.WriteString(input)
	if closeErr := file.Close(); err == nil && closeErr == nil && dir.Rename(tmp, name) == nil {
		return true
	}
	_ = dir.Remove(tmp)
	return false
}

// commit spends the reserved grant: the reservation becomes this call's spent record, or is removed for a call without a
// tool use id. Once committed, nothing gives the grant back: a lost answer is an unknown outcome, not a new capability.
func (c *spawnGrantClaim) commit() {
	if c.held == "" || c.committed {
		return
	}
	c.committed = true
	dir := spawnGrantOpen(c.tmpRoot, c.uid, c.key, false)
	if dir == nil {
		return
	}
	defer dir.Close()
	_ = dir.Remove(c.inputName())
	if c.bound && dir.Rename(c.held, c.spentName()) == nil {
		return
	}
	_ = dir.Remove(c.held)
}

// release gives back a grant this call reserved and did not commit, for a refusal met after the reservation (a managed
// issuance the ledger refused): the grant file returns under its own name for the corrected retry.
func (c *spawnGrantClaim) release() {
	if c.held == "" || c.committed {
		return
	}
	dir := spawnGrantOpen(c.tmpRoot, c.uid, c.key, false)
	if dir == nil {
		return
	}
	defer dir.Close()
	if dir.Rename(c.held, spawnGrantFile(c.nonce)) == nil {
		_ = dir.Remove(c.inputName())
		c.held = ""
	}
}

// spawnGrantRegular reports whether name is a regular file in dir, not a link.
func spawnGrantRegular(dir *os.Root, name string) bool {
	info, err := dir.Lstat(name)
	return err == nil && info.Mode().IsRegular()
}

// spawnGrantUnexpired reads a grant as JSON.parse does: one JSON object with no trailing text whose
// expiresAt (the last of a repeated key) is a finite integer of milliseconds within the minting TTL:
// not before now and not more than spawnGrantTTL after it (CRW-1118; the oracle took any number, a
// fraction or an overflow to infinity included, so a planted 1e999 never expired). The decoder alone
// refuses nesting beyond 10,000 levels, which JSON.parse accepts (known-defects.md). The file must be
// a regular file reached without a link.
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
	millis, err := strconv.ParseInt(expires.String(), 10, 64)
	return err == nil && millis >= now.UnixMilli() && millis <= now.Add(spawnGrantTTL).UnixMilli()
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
