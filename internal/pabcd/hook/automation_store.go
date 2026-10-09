// CXC v0.2.40 (3c1459acadeb1906d97c00a598e1457327ae372d), automation-store.ts.
// A strict, read-only subset of the observed flat automation TOML store. Unsupported syntax is an
// unavailable ownership snapshot, never a guessed owner.
package hook

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// automationMaxStoreBytes is the oracle's MAX_STORE_BYTES (automation-store.ts:7).
const automationMaxStoreBytes = 64 * 1024

// automationMaxSafeInteger is Number.isSafeInteger's bound (automation-store.ts:85): a store with a
// larger integer is unsupported, and Go's int64 alone would accept it.
const automationMaxSafeInteger = 9007199254740991

// automationOwnershipSnapshot is the oracle's AutomationOwnershipSnapshot interface
// (automation-store.ts:12-16). It is unexported: only this package reads it.
type automationOwnershipSnapshot struct {
	ID             string
	Kind           string
	TargetThreadID string
}

// errAutomationStore is the oracle's invalid() (automation-store.ts:22). It is a function rather
// than a package-level value so the package declares no package-level variable, and it carries no
// path, as the oracle's message carries none.
func errAutomationStore() error { return errors.New("unsupported automation store") }

// automationSafeID is isSafeAutomationId (automation-store.ts:18-20).
func automationSafeID(value any) bool {
	text, ok := value.(string)
	return ok && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,199}$`).MatchString(text)
}

// automationStoreStringKey is the oracle's STRING_KEYS (automation-store.ts:8).
func automationStoreStringKey(key string) bool {
	switch key {
	case "id", "kind", "target_thread_id", "name", "prompt", "rrule", "status":
		return true
	}
	return false
}

// automationStoreNumberKey is the oracle's NUMBER_KEYS (automation-store.ts:9).
func automationStoreNumberKey(key string) bool {
	switch key {
	case "created_at", "updated_at", "version":
		return true
	}
	return false
}

// automationStoreControl is the oracle's control-character class (automation-store.ts:40,66).
func automationStoreControl(ch byte) bool {
	return ch <= 0x08 || ch == 0x0b || ch == 0x0c || (ch >= 0x0e && ch <= 0x1f) || ch == 0x7f
}

// automationStoreStringValue consumes one complete string before looking for another key, so a
// multiline prompt's lines are never scanned as TOML (automation-store.ts:25-63). A multiline value
// reads as empty, because only the prompt may be multiline and its text is never used.
func automationStoreStringValue(text string, start int, key string) (value string, end int, err error) {
	quote := text[start]
	multiline := start+3 <= len(text) && text[start:start+3] == strings.Repeat(string(quote), 3)
	if multiline && key != "prompt" {
		return "", 0, errAutomationStore()
	}
	i := start + 1
	if multiline {
		i = start + 3
	}
	contentStart := i
	for i < len(text) {
		ch := text[i]
		if ch == quote && (!multiline || (i+3 <= len(text) && text[i:i+3] == strings.Repeat(string(quote), 3))) {
			end = i + 1
			if multiline {
				end = i + 3
				// Four and five quote endings are valid TOML, but outside this supported subset.
				if end < len(text) && text[end] == quote {
					return "", 0, errAutomationStore()
				}
				return "", end, nil
			}
			if quote == '\'' {
				return text[contentStart:i], end, nil
			}
			var decoded string
			if json.Unmarshal([]byte(text[start:end]), &decoded) != nil {
				return "", 0, errAutomationStore()
			}
			return decoded, end, nil
		}
		if (!multiline && (ch == '\n' || ch == '\r')) || automationStoreControl(ch) {
			return "", 0, errAutomationStore()
		}
		if ch == '\\' && quote == '"' {
			if i+1 >= len(text) {
				return "", 0, errAutomationStore()
			}
			next := text[i+1]
			if multiline && (next == '\n' || next == '\r' || next == ' ' || next == '\t') {
				continuation := regexp.MustCompile(`^[ \t]*\r?\n[ \t\r\n]*`).FindString(text[i+1:])
				if continuation == "" {
					return "", 0, errAutomationStore()
				}
				i += 1 + len(continuation)
				continue
			}
			if next == 'u' {
				if i+6 > len(text) || !regexp.MustCompile(`^[0-9a-fA-F]{4}$`).MatchString(text[i+2:i+6]) {
					return "", 0, errAutomationStore()
				}
				code, convErr := strconv.ParseUint(text[i+2:i+6], 16, 32)
				if convErr != nil || (code >= 0xd800 && code <= 0xdfff) {
					return "", 0, errAutomationStore()
				}
				i += 6
				continue
			}
			if !strings.ContainsRune("btnfr\"\\", rune(next)) {
				return "", 0, errAutomationStore()
			}
			i += 2
			continue
		}
		i++
	}
	return "", 0, errAutomationStore()
}

// automationStoreParse is parseStore (automation-store.ts:65-97).
func automationStoreParse(text string) (*automationOwnershipSnapshot, error) {
	if regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`).MatchString(text) {
		return nil, errAutomationStore()
	}
	for i := 0; i < len(text); i++ {
		if text[i] == '\r' && (i+1 >= len(text) || text[i+1] != '\n') {
			return nil, errAutomationStore()
		}
	}
	values := map[string]string{}
	numbers := map[string]int64{}
	offset := 0
	for offset < len(text) {
		offset += len(regexp.MustCompile(`^[ \t\r\n]*(?:#[^\r\n]*(?:\r?\n|$)[ \t\r\n]*)*`).FindString(text[offset:]))
		if offset == len(text) {
			break
		}
		assignment := regexp.MustCompile(`^([a-z_]+)[ \t]*=[ \t]*`).FindStringSubmatch(text[offset:])
		if assignment == nil {
			return nil, errAutomationStore()
		}
		key := assignment[1]
		if _, seen := values[key]; seen {
			return nil, errAutomationStore()
		}
		if _, seen := numbers[key]; seen {
			return nil, errAutomationStore()
		}
		if !automationStoreStringKey(key) && !automationStoreNumberKey(key) {
			return nil, errAutomationStore()
		}
		offset += len(assignment[0])
		if automationStoreStringKey(key) {
			if offset >= len(text) || (text[offset] != '"' && text[offset] != '\'') {
				return nil, errAutomationStore()
			}
			value, end, err := automationStoreStringValue(text, offset, key)
			if err != nil {
				return nil, err
			}
			values[key] = value
			offset = end
		} else {
			integer := regexp.MustCompile(`^(?:0|[1-9][0-9]*)`).FindString(text[offset:])
			if integer == "" {
				return nil, errAutomationStore()
			}
			value, err := strconv.ParseInt(integer, 10, 64)
			if err != nil || value > automationMaxSafeInteger {
				return nil, errAutomationStore()
			}
			numbers[key] = value
			offset += len(integer)
		}
		tail := regexp.MustCompile(`^[ \t]*(?:#[^\r\n]*)?(?:\r?\n|$)`).FindStringIndex(text[offset:])
		if tail == nil {
			return nil, errAutomationStore()
		}
		offset += tail[1]
	}
	id, hasID := values["id"]
	kind, hasKind := values["kind"]
	owner, hasOwner := values["target_thread_id"]
	if !hasID || !automationSafeID(id) || !hasKind || !hasOwner || !automationSafeID(owner) {
		return nil, errAutomationStore()
	}
	if version, present := numbers["version"]; present && version != 1 {
		return nil, errAutomationStore()
	}
	return &automationOwnershipSnapshot{ID: id, Kind: kind, TargetThreadID: owner}, nil
}

// automationAfterRead is a test seam: it runs between the end of the read and the second fstat, the
// window in which the oracle's snapshot check must see the file change. Production leaves it nil.
var automationAfterRead func(path string)

// automationReadOwnership is readAutomationOwnership (automation-store.ts:102-129): a bounded,
// read-only open that rejects symlinks, non-regular files and a snapshot that changed under it.
// It is a plain function so a test can substitute one; nothing here ever writes.
//
// The oracle also compares ctimeMs after the read (automation-store.ts:124); so does this port, through
// automationCtime, whose linux and darwin files name the stat field each platform calls it (CRW-804). On a
// platform with neither, os.SameFile plus the size and ModTime comparisons stand in for it.
func automationReadOwnership(codexHome, id string) (*automationOwnershipSnapshot, error) {
	if !filepath.IsAbs(codexHome) || !automationSafeID(id) {
		return nil, errAutomationStore()
	}
	directories := []string{codexHome, filepath.Join(codexHome, "automations"), filepath.Join(codexHome, "automations", id)}
	for _, directory := range directories {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errAutomationStore()
		}
	}
	resolved, err := filepath.EvalSymlinks(directories[2])
	if err != nil {
		return nil, errAutomationStore()
	}
	path := filepath.Join(resolved, "automation.toml")
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > automationMaxStoreBytes {
		return nil, errAutomationStore()
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errAutomationStore()
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() > automationMaxStoreBytes {
		return nil, errAutomationStore()
	}
	buffer := make([]byte, automationMaxStoreBytes+1)
	size := 0
	for size < len(buffer) {
		count, readErr := file.Read(buffer[size:])
		size += count
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, errAutomationStore()
		}
		if count == 0 {
			break
		}
	}
	if automationAfterRead != nil {
		automationAfterRead(path)
	}
	after, err := file.Stat()
	if err != nil || size > automationMaxStoreBytes || int64(size) != opened.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || automationCtimeChanged(opened, after) {
		return nil, errAutomationStore()
	}
	raw := buffer[:size]
	if !utf8.Valid(raw) {
		return nil, errAutomationStore()
	}
	snapshot, err := automationStoreParse(string(raw))
	if err != nil {
		return nil, err
	}
	if snapshot.ID != id {
		return nil, errAutomationStore()
	}
	return snapshot, nil
}

// automationCtimeChanged reports whether the inode change time moved between the fstat before the read and
// the one after it, which a same-length rewrite with the mtime put back and a chmod or chown both do. A
// platform that cannot name the time reports no change, as the port did before the comparison existed.
func automationCtimeChanged(before, after os.FileInfo) bool {
	was, okWas := automationCtime(before)
	is, okIs := automationCtime(after)
	return okWas && okIs && was != is
}
