package spawn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// This file makes the hook safe to apply again to one hook event (CRW-1121). The oracle's answer is not: a reapplied message got the
// prompt override again, and a reapplied recursion request lost its grant marker to stripControlMarkers and got a plain guard
// stacked on the coordinator guard (known-defects.md, "reapplied grant request"). Two parts fix it:
//   - spawnHookOwnedGuard recognizes every guard the hook writes, plain or coordinator, with or without the coordinator's grant
//     instruction, at the start of a message, so the guard of this event replaces it instead of stacking on it. A guard the caller
//     wrote is recognized the same way, so a forged coordinator guard is replaced and authorizes nothing;
//   - an event that mints a grant records its answer under the grant's key directory, bound to the event's tool use id and to the
//     event's input. The same event applied again to that input or to the input it answered with is answered with the recorded
//     answer, so its grant is kept and no second one is minted.

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

// spawnHookReplayRecord writes the answer of the event toolUseID, which minted a grant in obj's scope: the event's input as
// JSON.stringify writes it (one line: the writer escapes every line break), then the answer as written, published by a rename. The
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
	_, err = file.WriteString(input + "\n" + answer)
	if closeErr := file.Close(); err == nil && closeErr == nil && dir.Rename(tmp, name) == nil {
		return // published whole, so a reader never sees a partial answer
	}
	_ = dir.Remove(tmp)
}
