package spawn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// This file makes the hook safe to apply again to one hook event (CRW-1121). The oracle's answer is not: a reapplied message got the
// prompt override again, and a reapplied recursion request lost its grant marker to stripControlMarkers and got a plain guard
// stacked on the coordinator guard (known-defects.md, "reapplied grant request"). Three parts fix it:
//   - spawnHookOwnedGuard recognizes every guard the hook writes, plain or coordinator, with or without the coordinator's grant
//     instruction, at the start of a message, so the guard of this event replaces it instead of stacking on it. A guard the caller
//     wrote is recognized the same way, so a forged coordinator guard is replaced and authorizes nothing;
//   - an event that mints a grant, or a subagent's event that spends one, records its answer under the grant's key directory, bound to
//     the event's tool use id, to the kind of its spawner and to the event's input (CRW-1118). The same event applied again to that
//     input or to the input it answered with is answered with the recorded answer, so its grant is kept and no second one is minted;
//   - deliveries of one event are serialized by a lock in the same directory, taken before a grant is minted or spent and held until
//     the answer is recorded, so of two deliveries that both passed the lookup one decides and the other finds its record.

// spawnHookReplayMax bounds a record that is read back: the hook's 4 MiB input and an answer of that input with room for the guard
// and skill blocks.
const spawnHookReplayMax = 16 << 20

// spawnHookOwnedGuard is s without the guards the hook writes at its start, and whether there was one. A coordinator guard's
// grant instruction goes with it, its grant marker (stripControlMarkers may have removed it, and a trim the trailing space of the
// instruction) included. A guard must end the text or be followed by a blank line.
func spawnHookOwnedGuard(s string) (string, bool) {
	marker := regexp.MustCompile(`^` + SubspawnGrantPattern)
	found := false
	for {
		rest, ok := "", false
		for _, block := range []struct {
			text        string
			coordinator bool
		}{{V1ScopeBlockCoordinator, true}, {LeafGuardBlockCoordinator, true}, {V1ScopeBlock, false}, {LeafGuardBlock, false}} {
			if !strings.HasPrefix(s, block.text) {
				continue
			}
			tail := s[len(block.text):]
			if block.coordinator {
				switch instruction := strings.TrimRight(spawnGrantInstruction, " "); {
				case strings.HasPrefix(tail, spawnGrantInstruction):
					tail = marker.ReplaceAllString(tail[len(spawnGrantInstruction):], "")
				case tail == instruction:
					tail = ""
				}
			}
			if tail == "" || strings.HasPrefix(tail, "\n\n") {
				rest, ok = strings.TrimPrefix(tail, "\n\n"), true
				break
			}
		}
		if !ok {
			return s, found
		}
		s, found = rest, true
	}
}

// spawnHookDigest is the hex sha256 of text.
func spawnHookDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// spawnHookReplayName is the record of one event in a grant key directory.
func spawnHookReplayName(toolUseID string) string {
	tag, _ := spawnGrantTag(toolUseID)
	return "event-" + tag
}

// spawnHookReplayKind is the first line of a record: who spawned, so a subagent never reads the answer of a root event (which carries
// the grant that event minted) and a root event never reads the answer of a subagent's.
func spawnHookReplayKind(obj map[string]any) string {
	if IsSubagentSpawner(obj) {
		return "subagent"
	}
	return "root"
}

// spawnHookReplayLookup is the recorded answer of the event toolUseID in the key directory of obj's grant scope when the event's
// tool_input (as JSON.stringify writes it) is the input that event was first given or the updatedInput of its recorded answer.
func spawnHookReplayLookup(obj map[string]any, tmpRoot, toolUseID, input string) (string, bool) {
	key, ok := spawnGrantKey(obj)
	if !ok {
		return "", false
	}
	dir := spawnGrantOpen(tmpRoot, os.Getuid(), key, false)
	if dir == nil {
		return "", false
	}
	defer dir.Close()
	file, err := dir.OpenFile(spawnHookReplayName(toolUseID), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > spawnHookReplayMax {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(file, spawnHookReplayMax+1))
	if err != nil {
		return "", false
	}
	kind, data, ok := bytes.Cut(data, []byte("\n"))
	if !ok || string(kind) != spawnHookReplayKind(obj) {
		return "", false
	}
	first, answer, ok := bytes.Cut(data, []byte("\n"))
	if !ok {
		return "", false
	}
	if string(first) == input || spawnHookReplayUpdated(string(answer)) == input {
		return string(answer), true
	}
	return "", false
}

// spawnHookReplayUpdated is the updatedInput of a recorded allow answer as JSON.stringify writes it, or "".
func spawnHookReplayUpdated(answer string) string {
	v, err := pyjson.Loads(answer, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	if err != nil {
		return ""
	}
	o, _ := v.(pyjson.Object)
	out, _ := o.Get("hookSpecificOutput").(pyjson.Object)
	updated, ok := out.Get("updatedInput").(pyjson.Object)
	if !ok {
		return ""
	}
	return spawnHookRouteStringify(updated)
}

// spawnHookReplayRecord writes the answer of the event toolUseID, which minted a grant in obj's scope or spent one: the kind of its
// spawner, the event's input as JSON.stringify writes it (one line: the writer escapes every line break), then the answer as written, published by a rename. The
// record holds no digest or clock, so it is the same text for the same event. A record that exists is kept, and a record that
// cannot be written is skipped: the event then only loses the replay.
func spawnHookReplayRecord(obj map[string]any, tmpRoot, toolUseID, input, answer string) {
	key, ok := spawnGrantKey(obj)
	if !ok {
		return
	}
	dir := spawnGrantOpen(tmpRoot, os.Getuid(), key, false)
	if dir == nil {
		return
	}
	defer dir.Close()
	name := spawnHookReplayName(toolUseID)
	if _, err := dir.Lstat(name); err == nil {
		return
	}
	tmp := name + ".tmp-" + spawnHookDigest(answer)[:16]
	file, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	_, err = file.WriteString(spawnHookReplayKind(obj) + "\n" + input + "\n" + answer)
	if closeErr := file.Close(); err == nil && closeErr == nil && dir.Rename(tmp, name) == nil {
		return // published whole, so a reader never sees a partial answer
	}
	_ = dir.Remove(tmp)
}

// spawnHookEventLockWait bounds the wait for another delivery of the same event; a hook has 10 seconds in all.
var spawnHookEventLockWait = 3 * time.Second

var (
	errSpawnHookEventNoDir = errors.New("the event's grant directory is not available")
	errSpawnHookEventBusy  = errors.New("another delivery of the event holds its lock")
)

// spawnHookEventLock takes the lock of the event toolUseID in the key directory of obj's grant scope, creating the directory when
// create is set, and returns what releases it. The lock is a flock on a file of its own (event-<tag>.lock, which stays like the
// record), so it is released when the process ends however it ends. A directory that is not the 0700 key directory of this user is
// errSpawnHookEventNoDir; a lock another delivery keeps past spawnHookEventLockWait is errSpawnHookEventBusy.
func spawnHookEventLock(obj map[string]any, tmpRoot, toolUseID string, create bool) (func(), error) {
	key, ok := spawnGrantKey(obj)
	if !ok {
		return nil, errSpawnHookEventNoDir
	}
	dir := spawnGrantOpen(tmpRoot, os.Getuid(), key, create)
	if dir == nil {
		return nil, errSpawnHookEventNoDir
	}
	defer dir.Close()
	file, err := dir.OpenFile(spawnHookReplayName(toolUseID)+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, errSpawnHookEventNoDir
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errSpawnHookEventNoDir
	}
	for deadline := time.Now().Add(spawnHookEventLockWait); ; time.Sleep(2 * time.Millisecond) {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR || time.Now().After(deadline) {
			file.Close()
			return nil, errSpawnHookEventBusy
		}
	}
}
