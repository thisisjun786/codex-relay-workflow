// Package guidancerecord remembers which SessionStart guidance a session has been given, so a resumed session is
// not given the same text again (CRW-1146). A record is one JSON line holding the SHA-256 of the text a hook leg
// delivered and, for a leg whose text spells the command that runs crw, that command, kept per session and leg under
// <CODEX_HOME>/crw/session-guidance; it holds no text and no payload. Every failure answers "not
// delivered", so a missing, unreadable or changed record makes the leg say the whole thing, as it did before records.
package guidancerecord

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

const dirName = "session-guidance"

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// line is the record of a text and the command it names ("" when it names none), as it is stored.
func line(text, command string) string {
	b, _ := json.Marshal(struct {
		SchemaVersion int    `json:"schemaVersion"`
		Digest        string `json:"digest"`
		Command       string `json:"command,omitempty"`
	}{1, digest(text), command})
	return string(b) + "\n"
}

// slot is the record file for a session and a leg, or "" when the host has no home, the session is not an identifier
// (1 to 256 UTF-16 units without a control character or space, as the hook observation requires), the leg is not a slug, or a
// test's guard refuses the path (RefuseAccountHome).
func slot(env host.LookupEnv, session, leg string) string {
	if n := len(utf16.Encode([]rune(session))); n == 0 || n > 256 || leg == "" || len(leg) > 96 {
		return ""
	}
	for _, r := range session {
		if r <= 0x20 || r == 0x7f {
			return ""
		}
	}
	for _, r := range leg {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return ""
		}
	}
	home, set := env("CODEX_HOME")
	if !set {
		h, err := host.Home(env)
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".codex")
	}
	if home == "" {
		return ""
	}
	path := filepath.Join(home, "crw", dirName, digest(session), leg)
	if !allowed(path) {
		return ""
	}
	return path
}

// Delivered reports whether this session was given exactly this text, naming exactly this command, by this leg. The text is what
// the leg says with the command spelled as a fixed word, so a path that differs between hosts and runs is compared as the command
// and is not part of the digest.
func Delivered(env host.LookupEnv, session, leg, text, command string) bool {
	path := slot(env, session, leg)
	if path == "" {
		return false
	}
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && string(data) == line(text, command)
}

// Record notes that this session was given this text by this leg, replacing the leg's earlier record. It is best effort. A whole
// output that is not a resume's starts a generation of its own, so it also ends the pair a resume left open (see RecordResume).
func Record(env host.LookupEnv, session, leg, text, command string) {
	path := slot(env, session, leg)
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.Remove(path + resumeSuffix)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return
	}
	tmp := filepath.Join(filepath.Dir(path), "."+hex.EncodeToString(suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, werr := f.WriteString(line(text, command))
	if err := f.Close(); werr != nil || err != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
	}
}

// resumeSuffix names the mark a resume that gave the whole text leaves beside the leg's record.
const resumeSuffix = ".resume"

// PairWindow bounds the pair: a compact start comes right after the resume start of the same turn (the host compacts a resumed
// session's first turn before it samples), so one that comes much later is a compaction of its own, which empties a context that
// still held what the resume said.
const PairWindow = 15 * time.Minute

// RecordResume is Record for a resume that gave the whole text: besides the record it leaves a mark that lets the compact start of
// the same turn stay silent, once (CompactRepeatsResume). Codex appends the resume's output after the compaction record, so the
// compact start would otherwise stack the same text a second time (CRW-1180).
func RecordResume(env host.LookupEnv, session, leg, text, command string) {
	Record(env, session, leg, text, command)
	path := slot(env, session, leg)
	if path == "" {
		return
	}
	_ = os.Remove(path + resumeSuffix) // a fresh mark, never a link or a file kept from before
	if f, err := os.OpenFile(path+resumeSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_ = f.Close()
	}
}

// ClearResume ends the pair a resume left open without writing anything where none is open: a start, a clear or a compact that said
// its text starts a new generation.
func ClearResume(env host.LookupEnv, session, leg string) {
	if path := slot(env, session, leg); path != "" {
		_ = os.Remove(path + resumeSuffix)
	}
}

// CompactRepeatsResume reports whether a compact start that would say this text is the second half of a resume that already gave
// exactly it in the same turn, and takes the pair: the next compact is a compaction of its own. A missing, stale or other-text pair
// answers false, so the compact says the text as it did before records.
func CompactRepeatsResume(env host.LookupEnv, session, leg, text, command string) bool {
	path := slot(env, session, leg)
	if path == "" {
		return false
	}
	mark := path + resumeSuffix
	st, err := os.Lstat(mark)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if age := time.Since(st.ModTime()); age > PairWindow {
		_ = os.Remove(mark)
		return false
	}
	if !Delivered(env, session, leg, text, command) {
		return false
	}
	// One compact takes the pair: of two that race, only the one whose removal succeeds stays silent.
	return os.Remove(mark) == nil
}

// StartOf reads the session id and the source out of a SessionStart payload; each is "" when the payload is not an object or the
// member is absent or not a string. It is for the legs whose text is live state (the provider line, the flag warning), which keep no
// record at a start and need the pair only.
func StartOf(raw string) (session, source string) {
	var p struct {
		Session any `json:"session_id"`
		Source  any `json:"source"`
	}
	if json.Unmarshal([]byte(raw), &p) != nil {
		return "", ""
	}
	session, _ = p.Session.(string)
	source, _ = p.Source.(string)
	return session, source
}

// SilentCompact reports whether a live-state leg that would say text on this SessionStart payload stays silent because it is the
// compact start of the turn in which a resume said exactly that (CRW-1180). It takes the pair.
func SilentCompact(env host.LookupEnv, raw, leg, text string) bool {
	session, source := StartOf(raw)
	return source == "compact" && session != "" && CompactRepeatsResume(env, session, leg, text, "")
}

// Said notes, after a live-state leg wrote text whole, what the start gave: a resume leaves the record and the mark that let the
// compact start of the same turn stay silent, and any other start ends a pair a resume left open without writing anything.
func Said(env host.LookupEnv, raw, leg, text string) {
	session, source := StartOf(raw)
	if session == "" {
		return
	}
	if source == "resume" {
		RecordResume(env, session, leg, text, "")
		return
	}
	ClearResume(env, session, leg)
}
