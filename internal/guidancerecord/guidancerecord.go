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
	record(env, session, leg, text, command, nil)
}

// record writes the record and, when m is not nil, the resume mark that goes with it, holding the session's lock, so the pair a
// resume leaves and the one a whole output ends are never mixed with a prompt or a compact of another hook. Without the lock it
// writes nothing and ends the pair, so the leg says more next time.
func record(env host.LookupEnv, session, leg, text, command string, m *mark) {
	path := slot(env, session, leg)
	if path == "" {
		return
	}
	unlock, ok := lockSession(filepath.Dir(path), true)
	if !ok {
		_ = os.Remove(path + resumeSuffix)
		return
	}
	defer unlock()
	_ = os.Remove(path + resumeSuffix)
	if writeRecord(path, text, command) && m != nil {
		writeMark(path+resumeSuffix, *m)
	}
}

func writeRecord(path, text, command string) bool {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return false
	}
	tmp := filepath.Join(filepath.Dir(path), "."+hex.EncodeToString(suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.WriteString(line(text, command))
	if err := f.Close(); werr != nil || err != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// resumeSuffix names the mark a resume leaves beside the leg's record.
const resumeSuffix = ".resume"

// PairWindow bounds the pair from above: a compact start comes right after the resume start of the same turn (the host compacts a
// resumed session's first turn before it samples). The window alone does not tell a turn from the next one, so the user prompts
// that follow the resume end the pair too (NoteUserPrompt); the window only covers a session whose prompt hook did not run.
const PairWindow = 15 * time.Minute

// mark is what a resume leaves: what it gave (the whole text, or only the Part whose SHA-256 is named, the rest of the text being in the
// context from before) and the user prompts seen since, which end the pair when a second one arrives.
type mark struct {
	SchemaVersion int    `json:"schemaVersion"`
	Part          string `json:"part,omitempty"`
	Prompts       int    `json:"prompts,omitempty"`
	Turn          string `json:"turn,omitempty"`
}

// Pair is what a compact start finds of the resume of its turn.
type Pair int

const (
	// PairNone: no resume left an open pair for this text, so the compact says the text as it did before records.
	PairNone Pair = iota
	// PairWhole: the resume gave exactly this text, so the compact adds nothing.
	PairWhole
	// PairPart: the resume gave only the part (see PartDigest) of this text: the compact leaves that part out and says the rest.
	PairPart
)

// PartDigest names a part of a text a resume gave, as RecordResumePart keeps it and TakePair returns it.
func PartDigest(text string) string {
	if text == "" {
		return ""
	}
	return digest(text)
}

func readMark(path string) (mark, os.FileInfo, bool) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return mark{}, nil, false
	}
	data, err := os.ReadFile(path)
	var m mark
	if err != nil || json.Unmarshal(data, &m) != nil || m.SchemaVersion != 1 || m.Prompts < 0 {
		return mark{}, nil, false
	}
	return m, st, true
}

func writeMark(path string, m mark) bool {
	m.SchemaVersion = 1
	data, _ := json.Marshal(m)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return false
	}
	tmp := filepath.Join(filepath.Dir(path), "."+hex.EncodeToString(suffix)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.Write(append(data, '\n'))
	if err := f.Close(); werr != nil || err != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// RecordResume is Record for a resume that gave the whole text: besides the record it leaves a mark that lets the compact start of
// the same turn stay silent, once (TakePair). Codex appends the resume's output after the compaction record, so the compact start
// would otherwise stack the same text a second time (CRW-1180).
func RecordResume(env host.LookupEnv, session, leg, text, command string) {
	record(env, session, leg, text, command, &mark{})
}

// RecordResumePart notes that a resume of a session that was given the text before gave only this part of it (the session binding and
// the PATH banner): the compact start of the same turn must say the rest of the text, and leave that part out. The record is the
// earlier one and stays; an empty part gave nothing and leaves no pair.
func RecordResumePart(env host.LookupEnv, session, leg, part string) {
	path := slot(env, session, leg)
	if path == "" {
		return
	}
	unlock, ok := lockSession(filepath.Dir(path), true)
	if !ok {
		_ = os.Remove(path + resumeSuffix)
		return
	}
	defer unlock()
	_ = os.Remove(path + resumeSuffix)
	if d := PartDigest(part); d != "" {
		writeMark(path+resumeSuffix, mark{Part: d})
	}
}

// ClearResume ends the pair a resume left open without writing anything where none is open: a start, a clear or a compact starts a
// new generation, whether or not it says anything.
func ClearResume(env host.LookupEnv, session, leg string) {
	path := slot(env, session, leg)
	if path == "" {
		return
	}
	if unlock, ok := lockSession(filepath.Dir(path), false); ok {
		defer unlock()
	}
	_ = os.Remove(path + resumeSuffix)
}

// TakePair reports whether a compact start that would say this text follows a resume of its own turn, and takes the pair: the next
// compact is a compaction of its own. A missing, stale, other-text or later-turn pair answers PairNone, so the compact says the text
// as it did before records. A pair ends with the second user prompt after its resume (NoteUserPrompt), and after PairWindow. The pair
// is read and taken holding the session's lock, so of two compacts, or of a compact and a prompt, that race, a taken pair stays taken.
func TakePair(env host.LookupEnv, session, leg, text, command string) (Pair, string) {
	kind, part, _ := takePair(env, session, leg, text, command, false)
	return kind, part
}

// takePair is TakePair; with end set, a start that does not take the pair ends it under the same lock (Begin).
func takePair(env host.LookupEnv, session, leg, text, command string, end bool) (Pair, string, bool) {
	path := slot(env, session, leg)
	if path == "" {
		return PairNone, "", false
	}
	markPath := path + resumeSuffix
	unlock, ok := lockSession(filepath.Dir(path), false)
	if !ok {
		_ = os.Remove(markPath) // no lock, no pair: the compact says the text
		return PairNone, "", true
	}
	defer unlock()
	m, st, ok := readMark(markPath)
	switch {
	case !ok, m.Prompts > 1, time.Since(st.ModTime()) > PairWindow:
		_ = os.Remove(markPath) // a mark that cannot be read, or of a later turn, or stale, is not a pair
		return PairNone, "", true
	case !Delivered(env, session, leg, text, command):
		if end {
			_ = os.Remove(markPath)
		}
		return PairNone, "", end
	}
	if os.Remove(markPath) != nil {
		return PairNone, "", true
	}
	if m.Part != "" {
		return PairPart, m.Part, true
	}
	return PairWhole, "", true
}

// CompactRepeatsResume reports whether a compact start that would say this text is the second half of a resume that already gave
// exactly it in the same turn, and takes the pair (see TakePair).
func CompactRepeatsResume(env host.LookupEnv, session, leg, text, command string) bool {
	kind, _ := TakePair(env, session, leg, text, command)
	return kind == PairWhole
}

// Begin is what every SessionStart hook does first with the text it is about to say (the empty text when it has none to say): a
// compact start that repeats a whole resume of its turn takes the pair and answers true, and the hook says nothing; any other start
// ends a pair a resume left open, whether or not the hook says anything, because it begins a new generation. A resume that says
// something leaves its own pair after the hook wrote (RecordResume, Said). The returned pair and part are for a leg that can take
// part of a pair; the live-state legs ignore them.
func Begin(env host.LookupEnv, session, source, leg, text, command string) (Pair, string) {
	if session == "" {
		return PairNone, ""
	}
	if source == "compact" {
		if kind, part, ended := takePair(env, session, leg, text, command, true); kind != PairNone || ended {
			return kind, part
		}
	}
	ClearResume(env, session, leg)
	return PairNone, ""
}

// NoteUserPrompt counts a user prompt of a session against every pair its resumes left open: the first prompt after a resume is the
// turn the pair belongs to, and a prompt of another turn ends it, so a compaction of a later turn is a compaction of its own and says
// the text. Without a turn id every prompt counts. It counts holding the session's lock, so it never writes back a mark a compact took
// or a start ended meanwhile. Best effort: whatever it cannot read or lock is removed, which only makes a hook say more.
func NoteUserPrompt(env host.LookupEnv, session, turn string) {
	path := slot(env, session, "x")
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	unlock, locked := lockSession(dir, false)
	if locked {
		defer unlock()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) <= len(resumeSuffix) || name[len(name)-len(resumeSuffix):] != resumeSuffix {
			continue
		}
		markPath := filepath.Join(dir, name)
		m, st, ok := readMark(markPath)
		if !ok || !locked {
			_ = os.Remove(markPath)
			continue
		}
		if turn != "" && m.Turn == turn {
			continue
		}
		m.Prompts++
		m.Turn = turn
		if m.Prompts > 1 {
			_ = os.Remove(markPath)
			continue
		}
		if writeMark(markPath, m) {
			_ = os.Chtimes(markPath, st.ModTime(), st.ModTime()) // the window runs from the resume
		} else {
			_ = os.Remove(markPath)
		}
	}
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
// compact start of the turn in which a resume said exactly that (CRW-1180), and ends any pair a start that is not that compact left
// open. The text is "" for a leg that has nothing to say, which then only ends the pair. It takes the pair.
func SilentCompact(env host.LookupEnv, raw, leg, text string) bool {
	session, source := StartOf(raw)
	kind, _ := Begin(env, session, source, leg, text, "")
	return kind == PairWhole
}

// Said notes, after a live-state leg wrote text whole, what the start gave: a resume leaves the record and the mark that let the
// compact start of the same turn stay silent. Any other start already ended the pair (SilentCompact).
func Said(env host.LookupEnv, raw, leg, text string) {
	if session, source := StartOf(raw); session != "" && source == "resume" {
		RecordResume(env, session, leg, text, "")
	}
}
