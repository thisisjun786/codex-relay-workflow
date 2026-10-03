package evidence

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// UnrecordableSubdir is the directory, under the state directory, of the markers that say a verdict existed and could not be
// recorded in the session file (EVIDENCE_UNRECORDABLE_SUBDIR). A marker is a file named <session>-<agent>-<ms>.json.
const UnrecordableSubdir = "evidence-unrecordable"

// VerdictStatus is what UnrecordableVerdictStatus found: Present is a marker for the session, Unreadable that the marker directory
// cannot be trusted to be empty.
type VerdictStatus struct{ Present, Unreadable bool }

func unrecordableDir(cwd string) string {
	return filepath.Join(cwd, crwdir.DirName, UnrecordableSubdir)
}

// makeMarkerDir creates the state directory (with its .gitignore, as ensureCodexclawDir does) and the marker directory.
func makeMarkerDir(cwd string) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	return os.MkdirAll(unrecordableDir(cwd), 0o777)
}

// createExclusive is writeFileSync(path, data, { flag: "wx" }): it fails when path exists, and a failed write leaves the file.
func createExclusive(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	return errors.Join(err, f.Close())
}

// dirNames is readdirSync: the names in dir, in no order, or the error of the first failure.
func dirNames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// jsString is JSON.stringify of a string. encoding/json cannot stand in for it: it writes U+2028 and U+2029 as escapes, which
// JSON.stringify never does.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// WriteUnrecordableMarker records, outside the session file, that the verdict of an agent could not be recorded in it: it needs
// no lock, cannot be lost to a concurrent writer and does not depend on the session file being readable. It has the type
// MarkerWriter, so the SubagentStop gate passes it to RecordTombstone. A second marker for one session and agent in the same
// millisecond fails on the exclusive create, and the failure is the caller's to swallow.
func WriteUnrecordableMarker(cwd, sessionID, agentID string) error {
	return writeUnrecordableMarker(cwd, sessionID, agentID, time.Now())
}

func writeUnrecordableMarker(cwd, sessionID, agentID string, now time.Time) error {
	if err := makeMarkerDir(cwd); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s-%d.json", state.SanitizeKey(sessionID), state.SanitizeKey(agentID), now.UnixMilli())
	body := `{"sessionId":` + jsString(sessionID) + `,"agentId":` + jsString(agentID) + `,"at":"` + now.UTC().Format("2006-01-02T15:04:05.000Z") + "\"}\n"
	return createExclusive(filepath.Join(unrecordableDir(cwd), name), body)
}

// markerDirWritable proves the marker directory can be written, not merely read: a readable but unwritable directory is
// otherwise an empty one whose marker was attempted and silently failed. It creates the state and marker directories when they
// are missing, writes an empty probe file in the marker directory and removes it again. The probe is named by the pid, the time
// and a random string (the oracle: the pid and the time), so two goroutines cannot collide on one name.
func markerDirWritable(cwd string) bool {
	if makeMarkerDir(cwd) != nil {
		return false
	}
	probe := filepath.Join(unrecordableDir(cwd), fmt.Sprintf(".probe-%d-%d-%s", os.Getpid(), time.Now().UnixMilli(), rand.Text()))
	if createExclusive(probe, "") != nil {
		return false
	}
	err := os.Remove(probe)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// UnrecordableVerdictStatus looks for the markers of a session. It is a tri-state on purpose: an unreadable marker directory
// must not read as a session that never delegated. Only a missing directory is an absence, and then only if a marker could have
// been written into it; a directory that holds no marker of the session is clean only if it is writable. A marker is a name
// that starts with <session>-, so the markers of a session whose id continues with a dash after this one's count too. The query
// is not read-only: it creates the state directory, its .gitignore and the marker directory, and writes and removes a probe.
func UnrecordableVerdictStatus(cwd, sessionID string) VerdictStatus {
	names, err := dirNames(unrecordableDir(cwd))
	switch {
	case err == nil:
		prefix := state.SanitizeKey(sessionID) + "-"
		present := slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, prefix) })
		return VerdictStatus{Present: present, Unreadable: !present && !markerDirWritable(cwd)}
	case errors.Is(err, fs.ErrNotExist):
		return VerdictStatus{Unreadable: !markerDirWritable(cwd)}
	}
	return VerdictStatus{Unreadable: true}
}
