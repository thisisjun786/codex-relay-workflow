package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The managed marker (marker.py): create-once facts on a filesystem the relay database never
// sees. Every fact is published once, by writing a sibling temp, fsyncing it and linking it onto
// the target, so first-publication-wins is an operating-system fact; the layout and every rule
// here are fixed by skills/crw-run/references/hook-contract.md.

// Marker words and names (marker.py).
const (
	MarkerEnv           = "CODEX_SESSION_RELAY_MARKER_ROOT"
	markerDirectoryName = "codex-session-marker"
	Published           = "published"
	Exists              = "exists"
	claimFile           = "claim.json"
)

var singleFacts = []struct{ key, name string }{{"intent", "intent.json"}, {"bound", "bound.json"}, {"relationship", "relationship.json"}}
var numberedFacts = []string{"attempts", "conflicts", "resolutions"}

var assignmentPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidAssignment is valid_assignment: the hex sha256 of a dispatch request id.
func ValidAssignment(assignment any) bool {
	text, _ := assignment.(string)
	return assignmentPattern.MatchString(text)
}

// Named is named: only a string with a non-blank character (str.strip) names an identity.
func Named(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimFunc(text, isPySpace) != ""
}

// isPySpace is str.isspace for one character, the set str.strip removes.
func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// SameIdentity is same_identity: unnamed on either side is never a match.
func SameIdentity(left, right any) bool {
	return Named(left) && Named(right) && left == right
}

// ValidSegment is valid_segment: an identity that may be used as a directory name.
func ValidSegment(value any) bool {
	if !Named(value) {
		return false
	}
	text := value.(string)
	if text == "." || text == ".." {
		return false
	}
	return !strings.ContainsAny(text, "/\\\x00")
}

// MarkerSelection is marker.MarkerSelection: which rule chose the root, and the value that won.
type MarkerSelection struct{ Path, Source, Detail string }

// ResolveMarkerRoot is resolve_marker_root: flag, then the environment, then XDG, then home.
// Deliberately a different directory from the relay state directory.
func ResolveMarkerRoot(explicit string) (MarkerSelection, error) {
	if explicit != "" {
		path, err := absoluteUser(explicit)
		return MarkerSelection{path, "flag", "--marker-root " + explicit}, err
	}
	if override := os.Getenv(MarkerEnv); override != "" {
		path, err := absoluteUser(override)
		return MarkerSelection{path, "env", MarkerEnv + "=" + override}, err
	}
	var base, source, detail string
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		expanded, err := store.ExpandUser(xdg)
		if err != nil {
			return MarkerSelection{}, err
		}
		base, source, detail = expanded, "xdg", "XDG_STATE_HOME="+xdg
	} else {
		home, err := store.Home()
		if err != nil {
			return MarkerSelection{}, err
		}
		base, source = store.PathlibSpelling(strings.TrimSuffix(home, "/")+"/.local/state"), "home"
		detail = "default under " + base
	}
	path, err := absolutePath(base + "/" + markerDirectoryName)
	return MarkerSelection{path, source, detail}, err
}

// absoluteUser is Path(value).expanduser().absolute(): no symlink is resolved and no ".." folded.
func absoluteUser(value string) (string, error) {
	expanded, err := store.ExpandUser(value)
	if err != nil {
		return "", err
	}
	return absolutePath(expanded)
}

// absolutePath is Path.absolute(): the pathlib spelling, prefixed by the kernel's working
// directory when relative (store.Absolute).
func absolutePath(value string) (string, error) { return store.Absolute(value) }

// resolved is Path(value).expanduser().resolve(): symlinks followed, a missing tail kept. It
// is strict where resolve() is not: a loop or a component it cannot search is refused rather than
// kept as spelled (docs/port/known-defects.md, Python defects not carried over).
func resolved(value string) (string, error) {
	expanded, err := store.ExpandUser(value)
	if err != nil {
		return "", err
	}
	return store.ResolvePath(expanded)
}

// WorkspaceKey is workspace_key: sha256 of the resolved workspace path, so a symlinked or
// relative cwd reaches the same assignment the coordinator declared against.
func WorkspaceKey(workspace string) (string, error) {
	path, err := resolved(workspace)
	if err != nil {
		return "", err
	}
	// str.encode("utf-8") of the path, which refuses one holding a surrogate escape.
	if err = store.EncodeUTF8(path); err != nil {
		return "", err
	}
	return pyvalue.SHA256Hex(path), nil
}

// AssignmentID is assignment_id: the hash of the dispatch request id, never the id itself.
func AssignmentID(dispatchRequestID string) string { return pyvalue.SHA256Hex(dispatchRequestID) }

// encodedAssignmentID is assignment_id as the marker commands meet it first: an id holding a
// surrogate escape (an argv byte that is not UTF-8) raises str.encode("utf-8")'s
// UnicodeEncodeError before anything is compared or written.
func encodedAssignmentID(dispatchRequestID string) (string, error) {
	if err := store.EncodeUTF8(dispatchRequestID); err != nil {
		return "", err
	}
	return AssignmentID(dispatchRequestID), nil
}

// WorkspaceDir is workspace_dir.
func WorkspaceDir(root, workspace string) (string, error) {
	key, err := WorkspaceKey(workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, key), nil
}

// AssignmentDir is assignment_dir: the assignment owns a level under its workspace. A value that
// is not an assignment id is ValueError, as _checked_assignment raises it.
func AssignmentDir(root, workspace string, assignment any) (string, error) {
	if !ValidAssignment(assignment) {
		return "", &dispatch.HostError{Class: "ValueError", Detail: "an assignment id is the hex sha256 of a dispatch request id, not " + pyReprValue(assignment)}
	}
	directory, err := WorkspaceDir(root, workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, assignment.(string)), nil
}

// FactDigest is fact_digest: SHA-256 over the fact's canonical JSON with factId removed.
func FactDigest(payload Obj) string {
	body := Obj{}
	for _, f := range payload {
		if f.Key != "factId" {
			body = append(body, f)
		}
	}
	return pyvalue.SHA256Hex(canonical(body))
}

// canonical is _canonical: json.dumps(sort_keys=True, separators=(",", ":")), ASCII escaped.
func canonical(value any) string {
	return pyjson.Dumps(value, pyjson.Options{Compact: true, SortKeys: true})
}

// fsyncDirectory is _fsync_directory: best effort, the link already decided who won.
func fsyncDirectory(directory string) {
	handle, err := os.Open(directory)
	if err != nil {
		return
	}
	_ = handle.Sync()
	_ = handle.Close()
}

// Publish is marker.publish: create-once publication, "published" when this writer won and
// "exists" when it lost. root, when not empty, confines the write under it before and after the
// directory is made. It never links a fact after ctx is done: a blocked fsync cannot spend a hold
// after the hook exits.
func Publish(ctx context.Context, target string, payload any, root string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	directory := filepath.Dir(target)
	if root != "" {
		if _, err := confined(directory, root); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	if root != "" {
		if _, err := confined(directory, root); err != nil {
			return "", err
		}
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	temp := filepath.Join(directory, "."+filepath.Base(target)+".tmp."+strconv.Itoa(os.Getpid())+"."+hex.EncodeToString(suffix))
	body := []byte(canonical(payload))
	handle, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := handle.Write(body); err != nil {
		_ = handle.Close()
		_ = os.Remove(temp)
		return "", err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		_ = os.Remove(temp)
		return "", err
	}
	if err := handle.Close(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	outcome := Published
	linkErr := os.Link(temp, target)
	_ = os.Remove(temp)
	switch {
	case errors.Is(linkErr, fs.ErrExist):
		outcome = Exists
	case linkErr != nil:
		return "", linkErr
	}
	fsyncDirectory(directory)
	return outcome, nil
}

// Fact read answers (_read_fact): present, absent, or unreadable.
const (
	factPresent = iota
	factAbsent
	factUnreadable
)

// readFact is _read_fact: "it is not there" and "I could not look" are different answers. A
// deadline on ctx is carried through the read.
func readFact(ctx context.Context, path string) (any, int) {
	var data []byte
	var err error
	if _, bounded := ctx.Deadline(); bounded {
		if ctx.Err() != nil {
			return nil, factUnreadable
		}
		file, openErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
		err = openErr
		if err == nil {
			defer file.Close()
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				return nil, factUnreadable
			}
			data, err = io.ReadAll(file)
			if ctx.Err() != nil {
				return nil, factUnreadable
			}
		}
	} else {
		data, err = os.ReadFile(path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, factAbsent
	}
	if err != nil || !utf8.Valid(data) {
		return nil, factUnreadable
	}
	value, err := loads(string(data))
	if err != nil {
		return nil, factUnreadable
	}
	return value, factPresent
}

// listing is marker.listing: every entry of a directory, sorted, and whether it could be read.
// FileNotFound is the only absence. only is "" (any), "directories" or "entries".
func listing(directory, pattern, only string) ([]string, bool) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var found []string
	for _, entry := range entries {
		if pattern != "" {
			if matched, _ := filepath.Match(pattern, entry.Name()); !matched {
				continue
			}
		}
		path := filepath.Join(directory, entry.Name())
		if only != "" {
			info, err := os.Stat(path)
			isDirectory := false
			if err == nil {
				isDirectory = info.IsDir()
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, false
			}
			if (only == "directories") != isDirectory {
				continue
			}
		}
		found = append(found, path)
	}
	sort.Strings(found)
	return found, true
}

// confined is marker.confined: resolve a path and require it to stay under the root owning it.
func confined(path, root string) (string, error) {
	resolvedPath, err := store.ResolvePath(path)
	if err != nil {
		return "", err
	}
	base, err := store.ResolvePath(root)
	if err != nil {
		return "", err
	}
	if resolvedPath != base && !strings.HasPrefix(resolvedPath, strings.TrimSuffix(base, "/")+"/") {
		return "", &dispatch.HostError{Class: "ValueError", Detail: "refusing to write outside the marker root: " + resolvedPath + " is not under " + base}
	}
	return resolvedPath, nil
}

// identified is _identified: the factId the READER walked to, never one copied out of a body.
// A fact that is not a record reaches the reader as it is.
func identified(value any, factID string) any {
	record, ok := value.(Obj)
	if !ok {
		return value
	}
	out := append(Obj(nil), record...)
	return out.Set("factId", factID)
}

func stem(name string) string { return strings.TrimSuffix(name, filepath.Ext(name)) }

// ReadAssignment is read_assignment: every published fact, plus the labels of anything that could
// not be read. Nothing here fails; every outside-world step answers with a label instead. The
// hook's deadline on ctx is carried through the fact walk.
func ReadAssignment(ctx context.Context, directory string) (Obj, []string) {
	marker, unreadable := Obj{}, []string{}
	for _, fact := range singleFacts {
		if ctx.Err() != nil {
			return marker, append(unreadable, fact.key)
		}
		value, status := readFact(ctx, filepath.Join(directory, fact.name))
		switch status {
		case factAbsent:
			continue
		case factUnreadable:
			unreadable = append(unreadable, fact.key)
			continue
		}
		marker = append(marker, F{Key: fact.key, Value: identified(value, fact.key)})
	}
	for _, key := range numberedFacts {
		if ctx.Err() != nil {
			return marker, append(unreadable, key)
		}
		entries, readable := listing(filepath.Join(directory, key), "*.json", "")
		if !readable {
			unreadable = append(unreadable, key)
			continue
		}
		items := []any{}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return marker, append(unreadable, key)
			}
			name := filepath.Base(entry)
			if strings.HasPrefix(name, ".") {
				continue
			}
			value, status := readFact(ctx, entry)
			switch status {
			case factAbsent:
				continue
			case factUnreadable:
				unreadable = append(unreadable, key+"/"+stem(name))
				continue
			}
			items = append(items, identified(value, key+"/"+stem(name)))
		}
		marker = append(marker, F{Key: key, Value: items})
	}
	sessions, readable := listing(filepath.Join(directory, "claims"), "", "directories")
	if !readable {
		return marker, append(unreadable, "claims")
	}
	claims := []any{}
	for _, session := range sessions {
		if ctx.Err() != nil {
			return marker, append(unreadable, "claims")
		}
		factID := "claims/" + filepath.Base(session) + "/" + claimFile
		value, status := readFact(ctx, filepath.Join(session, claimFile))
		switch status {
		case factAbsent:
			continue
		case factUnreadable:
			unreadable = append(unreadable, factID)
			continue
		}
		claims = append(claims, identified(value, factID))
	}
	return append(marker, F{Key: "claims", Value: claims}), unreadable
}

// ReadDisposition is read_disposition: the disposition this session recorded for this turn, read
// at the path the Stop identity derives. (nil, true) when absent or when the identity names no
// path; (nil, false) when it could not be read. ctx bounds the delivered identity's fact read.
func ReadDisposition(ctx context.Context, directory string, sessionID, turnID any) (any, bool) {
	if !ValidSegment(sessionID) || !ValidSegment(turnID) {
		return nil, true
	}
	session, turn := sessionID.(string), turnID.(string)
	value, status := readFact(ctx, filepath.Join(directory, "dispositions", session, turn+".json"))
	switch status {
	case factAbsent:
		return nil, true
	case factUnreadable:
		return nil, false
	}
	return identified(value, "dispositions/"+session+"/"+turn), true
}

// ListAssignments is list_assignments: every assignment declared for this workspace, oldest name
// first, and whether the workspace directory could be read.
func ListAssignments(root, workspace string) ([]string, bool, error) {
	directory, err := WorkspaceDir(root, workspace)
	if err != nil {
		return nil, false, err
	}
	found, readable := listing(directory, "", "directories")
	return found, readable, nil
}

// markerFactList is a numbered or claims fact list as the reader returned it.
func markerFactList(marker Obj, key string) []any {
	v, _ := marker.Lookup(key)
	list, _ := v.([]any)
	return list
}

func markerFact(marker Obj, key string) Obj {
	v, _ := marker.Lookup(key)
	record, _ := v.(Obj)
	return record
}

func fieldOf(record Obj, key string) any {
	v, _ := record.Lookup(key)
	return v
}
