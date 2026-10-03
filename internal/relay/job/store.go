// Package job is the file store of the CRW background jobs: the Go form of CXC v0.2.40 bg-wake/src/store.ts and removal.ts under the CRW
// names of contract/schema/cxc/name-substitution.json. Everything lives in <cwd>/.crw/bg: <id>.json (the record), <id>.out (merged
// output), <id>.exit (the exit code), disabled and enabled-at (the wake switch) and ledger.jsonl (the append-only events). The
// registry, the spawn, the verb and the hooks are later ports and import this package.
//
// Behaviour is the oracle's, byte for byte where it is observable. The deliberate differences confine the store to the workspace it
// serves, because the oracle accepted any path (an id such as "../../x", or a record file naming another cwd, made atomicWrite and
// removePath act outside .codexclaw/bg):
//   - AtomicWrite and RemovePath take the workspace they act for and refuse (ErrOutsideStore) a path that is not a file directly inside
//     its BGDir. The path is judged absolute and cleaned, and that path is the one used; BGDir must resolve, symlinks followed, to the
//     workspace or below it (the judgment of the plan gate in internal/pabcd/attest). A caller passes the workspace it trusts, never
//     the cwd field of a record it has read.
//   - EnsureDir makes the same judgment of .crw and bg, so the ledger and the spawn, which begin with it, stay inside the workspace.
//   - AtomicWrite creates its temporary file exclusively, and the ledger is opened without following a link.
//
// This is a work control, not a boundary against a writer that swaps a link between the check and the use; a hard link is a second
// name for its file and is not detected. Reads still follow links, as the oracle's did. Two oracle values have no Go spelling: a lone
// surrogate (such input becomes U+FFFD; a caller that must keep it passes the raw token as json.RawMessage) and
// integer-first object key order (every key is a literal name in a caller's source).
package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The names of the store (BG_DIRNAME, DISABLED_FILE, ENABLED_AT_FILE, LEDGER_FILE).
const (
	BGDirName     = "bg"
	DisabledFile  = "disabled"
	EnabledAtFile = "enabled-at"
	LedgerFile    = "ledger.jsonl"
)

type sentinel string

func (e sentinel) Error() string { return string(e) }

// ErrOutsideStore is the refusal of a write, a removal or a directory that is not inside the workspace's .crw/bg.
const ErrOutsideStore = sentinel("path is not a file directly inside the workspace's .crw/bg")

// BGDir is <cwd>/.crw/bg (bgDir). The builders clean as path.join does, so an id with ".." or a separator leaves it.
func BGDir(cwd string) string { return filepath.Join(cwd, crwdir.DirName, BGDirName) }

// RecordPath, OutPath and ExitPath are <id>.json, <id>.out and <id>.exit in BGDir (recordPath, outPath, exitPath).
func RecordPath(cwd, id string) string { return filepath.Join(BGDir(cwd), id+".json") }
func OutPath(cwd, id string) string    { return filepath.Join(BGDir(cwd), id+".out") }
func ExitPath(cwd, id string) string   { return filepath.Join(BGDir(cwd), id+".exit") }

// DisabledPath and EnabledAtPath are the wake switch files (disabledPath, enabledAtPath).
func DisabledPath(cwd string) string  { return filepath.Join(BGDir(cwd), DisabledFile) }
func EnabledAtPath(cwd string) string { return filepath.Join(BGDir(cwd), EnabledAtFile) }

// EnsureDir creates .crw with its .gitignore when it is new, then bg, and returns bg (ensureDir). It refuses a .crw or bg that
// resolves outside the workspace.
func EnsureDir(cwd string) (string, error) {
	crw, err := crwdir.EnsureDir(cwd)
	if err != nil {
		return "", err
	}
	if err := inside(cwd, crw); err != nil {
		return "", err
	}
	dir := BGDir(cwd)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	if err := inside(cwd, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// inside is nil when dir, with its symlinks followed, is the workspace or lies below it. A directory that does not exist yet escapes
// nothing; a path that cannot be resolved for any other reason is refused.
func inside(cwd, dir string) error {
	realDir, err := resolved(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrOutsideStore
	}
	realBase, err := resolved(cwd)
	if err != nil {
		return ErrOutsideStore
	}
	rel, err := filepath.Rel(realBase, realDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ErrOutsideStore
	}
	return nil
}

// resolved is the absolute path with its symlinks followed.
func resolved(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// confined returns path, absolute and cleaned, when it is a file directly inside BGDir(cwd) and that directory is inside the workspace.
func confined(cwd, path string) (string, error) {
	abs, err1 := filepath.Abs(path)
	dir, err2 := filepath.Abs(BGDir(cwd))
	if err1 != nil || err2 != nil || filepath.Dir(abs) != dir {
		return "", ErrOutsideStore
	}
	if err := inside(cwd, dir); err != nil {
		return "", err
	}
	return abs, nil
}

// AtomicWrite publishes text at path through a temporary file beside it and a rename, so a reader never sees a torn file
// (atomicWrite). The temporary file is <path>.tmp-<pid>-<ms>; a rename that fails leaves it behind, as the oracle did.
func AtomicWrite(cwd, path, text string) error {
	return atomicWrite(cwd, path, text, os.Getpid(), time.Now().UnixMilli())
}

func atomicWrite(cwd, path, text string, pid int, ms int64) error {
	path, err := confined(cwd, path)
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(ms, 10)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return crwdir.Rename(tmp, path)
}

// ReadText is the text of the file as Node reads it with "utf8", or false when it is missing or cannot be read (readTextOrNull).
func ReadText(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return decodeUTF8(b), true
}

// ReadJSON is the text of a file that holds one JSON object or array, or false for anything else (readJsonOrNull; its typeof test lets
// an array through).
func ReadJSON(path string) (json.RawMessage, bool) {
	text, ok := ReadText(path)
	if !ok || !json.Valid([]byte(text)) {
		return nil, false
	}
	if first := strings.TrimLeft(text, " \t\n\r"); first[0] != '{' && first[0] != '[' {
		return nil, false
	}
	return json.RawMessage(text), true
}

// decodeUTF8 is Buffer.toString("utf8"): one U+FFFD for each maximal invalid subpart, as the WHATWG decoder defines it.
func decodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out []byte
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			out, size = utf8.AppendRune(out, utf8.RuneError), subpart(b[i:])
		} else {
			out = append(out, b[i:i+size]...)
		}
		i += size
	}
	return string(out)
}

// subpart is the length of the longest prefix of b that starts a well-formed UTF-8 sequence.
func subpart(b []byte) int {
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch c := b[0]; {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c == 0xED:
		need, hi = 2, 0x9F
	case c >= 0xE1 && c <= 0xEF:
		need = 2
	case c == 0xF0:
		need, lo = 3, 0x90
	case c == 0xF4:
		need, hi = 3, 0x8F
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	}
	n := 1
	for ; n <= need && n < len(b) && b[n] >= lo && b[n] <= hi; n++ {
		lo, hi = 0x80, 0xBF
	}
	return n
}

// Member is one key of a ledger row, and Event lists them in print order. A value is nil, a string, bool, int, int64, float64 (NaN and
// the infinities print as null), []string, []any, a nested Event or a json.RawMessage, written compact as it is given (JSON.stringify's
// normalising of escapes, numbers and repeated keys is not applied).
type (
	Member struct {
		Key   string
		Value any
	}
	Event []Member
)

// AppendLedger appends {"at": the time, ...event} as one line (appendLedger). A row that cannot be written is lost: ledger loss must
// never break a hook.
func AppendLedger(cwd string, event Event) { _ = appendLedger(cwd, event, time.Now) }

// isoLayout is Date.prototype.toISOString: UTC with three fraction digits.
const isoLayout = "2006-01-02T15:04:05.000Z"

// appendLedger is AppendLedger with the clock as an argument and the failure returned. As in a JavaScript spread, an event's own "at"
// replaces the time and keeps the first place.
func appendLedger(cwd string, event Event, now func() time.Time) error {
	dir, err := EnsureDir(cwd)
	if err != nil {
		return err
	}
	row, err := object(append([]Member{{"at", now().UTC().Format(isoLayout)}}, event...), 0)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, LedgerFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return err
	}
	_, err = f.Write(append(row, '\n'))
	return errors.Join(err, f.Close())
}

// object is JSON.stringify of an object with those members in that order; a repeated key keeps its first place with its last value.
func object(members []Member, depth int) ([]byte, error) {
	if depth > maxDepth {
		return nil, errTooDeep
	}
	keys := make([]Member, 0, len(members))
	for _, m := range members {
		if i := slices.IndexFunc(keys, func(x Member) bool { return x.Key == m.Key }); i >= 0 {
			keys[i].Value = m.Value
		} else {
			keys = append(keys, m)
		}
	}
	b := []byte{'{'}
	for i, m := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		v, err := value(m.Value, depth)
		if err != nil {
			return nil, err
		}
		b = append(append(append(b, quote(m.Key)...), ':'), v...)
	}
	return append(b, '}'), nil
}

// maxDepth bounds the nesting of a row, so a value that contains itself is an error and not a stack overflow (JSON.stringify throws
// at a similar depth).
const (
	maxDepth       = 4096
	errTooDeep     = sentinel("ledger value nested too deeply")
	errUnsupported = sentinel("ledger value of a type the encoder does not print")
)

func value(v any, depth int) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return []byte("null"), nil
	case string:
		return []byte(quote(v)), nil
	case bool:
		return strconv.AppendBool(nil, v), nil
	case int:
		return integer(int64(v), depth)
	case int64:
		return integer(v, depth)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return []byte("null"), nil
		}
		if v == 0 { // -0 prints as 0
			return []byte("0"), nil
		}
		return json.Marshal(v) // ECMAScript's spelling of a number: 1e+21, 1e-7
	case []string:
		items := make([]any, len(v))
		for i, s := range v {
			items[i] = s
		}
		return array(items, depth)
	case []any:
		return array(v, depth)
	case Event:
		return object(v, depth+1)
	case json.RawMessage:
		var b bytes.Buffer
		if !utf8.Valid(v) || json.Compact(&b, v) != nil {
			return nil, errUnsupported
		}
		return b.Bytes(), nil
	}
	return nil, errUnsupported
}

// integer prints v as JavaScript prints the Number that holds it: exactly up to 2^53, rounded beyond.
func integer(v int64, depth int) ([]byte, error) {
	if v > 1<<53 || v < -(1<<53) {
		return value(float64(v), depth)
	}
	return strconv.AppendInt(nil, v, 10), nil
}

func array(items []any, depth int) ([]byte, error) {
	if depth > maxDepth {
		return nil, errTooDeep
	}
	b := []byte{'['}
	for i, item := range items {
		if i > 0 {
			b = append(b, ',')
		}
		v, err := value(item, depth+1)
		if err != nil {
			return nil, err
		}
		b = append(b, v...)
	}
	return append(b, ']'), nil
}

// quote is JSON.stringify of a string: the quote, the backslash and the control characters escaped, everything else, U+2028 and U+2029
// included, as it is. A string that is not UTF-8 is decoded as Node decodes its arguments.
func quote(s string) string {
	if !utf8.ValidString(s) {
		s = decodeUTF8([]byte(s))
	}
	const hex, controls, letters = "0123456789abcdef", "\b\f\n\r\t", "bfnrt"
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case strings.ContainsRune(controls, r):
			b.WriteByte('\\')
			b.WriteByte(letters[strings.IndexRune(controls, r)])
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[r>>4])
			b.WriteByte(hex[r&15])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ListRecordIDs is the names in BGDir that end in ".json" (a directory counts), the suffix cut, sorted by UTF-16 code unit as
// JavaScript's sort does (listRecordIds); an unreadable BGDir has none.
func ListRecordIDs(cwd string) []string {
	ids := []string{}
	entries, err := os.ReadDir(BGDir(cwd))
	if err != nil {
		return ids
	}
	for _, e := range entries {
		if name := decodeUTF8([]byte(e.Name())); strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	slices.SortFunc(ids, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
	return ids
}

// MtimeMs is the modification time of the file in milliseconds, fractions included, or false (mtimeMs, which is statSync's mtimeMs).
func MtimeMs(path string) (float64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return msOf(info.ModTime()), true
}

// msOf takes seconds and nanoseconds apart: a count of nanoseconds since 1970 overflows after the year 2262.
func msOf(t time.Time) float64 { return float64(t.Unix())*1e3 + float64(t.Nanosecond())/1e6 }

// RemovePath removes a file or a link of the store and nothing else (removePath: rmSync with force, without recursive). A missing path,
// a directory and every other failure are nothing, as the oracle's best effort is; only a path outside the store is an error.
func RemovePath(cwd, path string) error {
	path, err := confined(cwd, path)
	if err != nil {
		return err
	}
	_ = syscall.Unlink(path)
	return nil
}
