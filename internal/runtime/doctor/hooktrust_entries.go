// Hook trust entry listing, ported from CXC v0.2.40 hook-trust.ts:48-57 (HookEntry) and
// :140-216 (normalizeHookPath, containedPluginFile, assertSafeHeaderValue, listHookEntries),
// commit 3c1459acadeb1906d97c00a598e1457327ae372d. ListHookTrustEntries reads a plugin's manifest,
// follows each hook file it declares (refusing a path or a symlink that leaves the plugin root)
// and returns one entry per command hook Codex would ask to trust. It writes nothing and needs no
// Node; the identity hash of each hook is HookTrustIdentityHash.
//
// The oracle's structured errors keep their texts and their order. An engine error is the Go
// error of the same failure: a missing or unreadable file, a document the JSON reader refuses
// (the oracle's ENOENT, EISDIR and SyntaxError carry host paths and V8 texts), while the raw
// TypeError of a JSON null document is returned verbatim. Hook files and the manifest are read
// as the oracle read them: Buffer.toString("utf8") (one U+FFFD for each maximal invalid
// subpart, a byte order mark kept) and JSON.parse, through internal/pyjson with
// LoadOptions{Surrogates: true}, so a repeated key keeps its first place and its last value and
// a lone surrogate escape survives as the oracle held it.
//
// Several oracle behaviours are kept as they are, not repaired (one line each in
// docs/port-cxc/known-defects.md): the matcher test is JavaScript's RegExp grammar, which Go
// approximates; an event name that Object.prototype defines passes the oracle's "in" guard and
// is spelled into the key as the source of the inherited function; a "hooks" member that is a
// non-empty string or array is read as an object with index keys; "./" is stripped twice before
// the path is resolved; a plugin root of "/" rejects every reference (the prefix test appends a
// separator to a root that already ends with one); and a document nested deeper than the Go
// reader's limit is refused. Two behaviours are repaired, as answers to Devin's security
// findings (port: fixed): the manifest goes through the same containment check as a hook file,
// and each file is opened through an os.Root of the plugin root and read from the opened handle,
// so a link swapped in once the root is open cannot lead outside it
// (hookTrustEntriesReadContained).
package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// HookTrustEntry is HookEntry (hook-trust.ts:48-57): the trust key of one command hook, its
// identity hash, and the SHA-256 of the hook file's bytes. FileSha256 is evidence only; trust is
// decided by Hash.
type HookTrustEntry struct {
	Key        string
	Hash       string
	FileSha256 string
}

// hookTrustEntriesNullDocument is the TypeError V8 throws for the "hooks" read of a JSON null
// document or manifest (hook-trust.ts:166, :179).
const hookTrustEntriesNullDocument = "Cannot read properties of null (reading 'hooks')"

// ListHookTrustEntries is listHookEntries (hook-trust.ts:162-216): the entries of every hook
// file the manifest of the plugin at pluginRoot declares, in manifest order, events in the
// order of the file, then groups and handlers in array order. A group whose matcher is not a
// valid regular expression and a handler that is not a command, has no command or is async are
// skipped; every other defect of a document is an error.
func ListHookTrustEntries(pluginRoot, pluginKey string) ([]HookTrustEntry, error) {
	if err := hookTrustEntriesSafeValue(pluginKey, "plugin key"); err != nil {
		return nil, err
	}
	manifestBytes, err := hookTrustEntriesReadContained(pluginRoot, ".codex-plugin/plugin.json", nil)
	if err != nil {
		return nil, err
	}
	manifest, err := hookTrustEntriesParse(manifestBytes)
	if err != nil {
		return nil, err
	}
	declared, err := hookTrustEntriesMember(manifest)
	if err != nil {
		return nil, err
	}
	references, isArray := declared.([]any)
	entries := []HookTrustEntry{}
	if !isArray {
		return entries, nil
	}
	for _, reference := range references {
		name, isString := reference.(string)
		if !isString {
			return nil, errors.New("plugin manifest hook references must be strings")
		}
		relative := hookTrustEntriesStrip(name)
		if err := hookTrustEntriesSafeValue(relative, "hook path"); err != nil {
			return nil, err
		}
		raw, err := hookTrustEntriesReadContained(pluginRoot, relative, nil)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		document, err := hookTrustEntriesParse(raw)
		if err != nil {
			return nil, err
		}
		listed, err := hookTrustEntriesFromDocument(document, pluginKey, relative, hex.EncodeToString(digest[:]))
		if err != nil {
			return nil, err
		}
		entries = append(entries, listed...)
	}
	return entries, nil
}

// hookTrustEntriesFromDocument walks one hook document (hook-trust.ts:177-213).
func hookTrustEntriesFromDocument(document any, pluginKey, relative, fileSha256 string) ([]HookTrustEntry, error) {
	hooks, err := hookTrustEntriesMember(document)
	if err != nil {
		return nil, err
	}
	var entries []HookTrustEntry
	for _, event := range hookTrustEntriesEvents(hooks) {
		label, known := hookTrustEntriesLabel(event.Key)
		if !known {
			return nil, errors.New("unsupported hook event: " + event.Key)
		}
		groups, isArray := event.Value.([]any)
		if !isArray {
			return nil, fmt.Errorf("%s:%s must contain an array", relative, event.Key)
		}
		for groupIdx, rawGroup := range groups {
			at := fmt.Sprintf("%s:%s[%d]", relative, event.Key, groupIdx)
			group, isObject := hookTrustEntriesObject(rawGroup)
			if !isObject {
				return nil, errors.New(at + " is invalid")
			}
			var matcher *string
			if value, present := group.Lookup("matcher"); present {
				pattern, isString := value.(string)
				if !isString {
					return nil, errors.New(at + ".matcher must be a string")
				}
				matcher = &pattern
			}
			handlers, isArray := group.Get("hooks").([]any)
			if !isArray {
				return nil, errors.New(at + ".hooks must be an array")
			}
			if matcher != nil && *matcher != "" && *matcher != "*" && !hookTrustEntriesMatcherValid(*matcher) {
				continue
			}
			for handlerIdx, rawHandler := range handlers {
				handler, isObject := hookTrustEntriesObject(rawHandler)
				if !isObject {
					return nil, fmt.Errorf("%s.hooks[%d] is invalid", at, handlerIdx)
				}
				if kind, _ := handler.Get("type").(string); kind != "command" {
					continue
				}
				if command, isString := handler.Get("command").(string); !isString || text.Trim(command) == "" {
					continue
				}
				if async, _ := handler.Get("async").(bool); async {
					continue
				}
				fields := make(map[string]any, len(handler))
				for _, field := range handler {
					fields[field.Key] = field.Value
				}
				hash, err := HookTrustIdentityHash(event.Key, matcher, fields)
				if err != nil {
					return nil, err
				}
				entries = append(entries, HookTrustEntry{
					Key:        fmt.Sprintf("%s:%s:%s:%d:%d", pluginKey, relative, label, groupIdx, handlerIdx),
					Hash:       hash,
					FileSha256: fileSha256,
				})
			}
		}
	}
	return entries, nil
}

// hookTrustEntriesParse is JSON.parse(buffer.toString("utf8")).
func hookTrustEntriesParse(data []byte) (any, error) {
	return pyjson.Loads(hookTrustEntriesUTF8(data), pyjson.LoadOptions{Surrogates: true})
}

// hookTrustEntriesMember is the read of the "hooks" member of a parsed document: a JSON null
// throws, an object answers the member (nil when absent), and any other value has none.
func hookTrustEntriesMember(document any) (any, error) {
	if document == nil {
		//lint:ignore ST1005 Preserve the oracle's exact TypeError text.
		return nil, errors.New(hookTrustEntriesNullDocument)
	}
	object, _ := document.(pyjson.Object)
	return object.Get("hooks"), nil
}

// hookTrustEntriesObject is "an object" in JavaScript's typeof sense for a value that is not
// falsy: a JSON object, or an array, which has no members of its own to read.
func hookTrustEntriesObject(value any) (pyjson.Object, bool) {
	switch object := value.(type) {
	case pyjson.Object:
		return object, true
	case []any:
		return nil, true
	}
	return nil, false
}

// hookTrustEntriesEvents is Object.entries(hooks ?? {}) (hook-trust.ts:177): an object lists
// its array-index keys first, in ascending order, then every other key in file order; a
// non-empty string or array is an object whose first key is "0"; null, absent, numbers,
// booleans and empty strings and arrays have no entries.
func hookTrustEntriesEvents(hooks any) pyjson.Object {
	switch value := hooks.(type) {
	case pyjson.Object:
		var indexed, others pyjson.Object
		for _, field := range value {
			if _, isIndex := hookTrustEntriesIndex(field.Key); isIndex {
				indexed = append(indexed, field)
			} else {
				others = append(others, field)
			}
		}
		sort.SliceStable(indexed, func(i, j int) bool {
			a, _ := hookTrustEntriesIndex(indexed[i].Key)
			b, _ := hookTrustEntriesIndex(indexed[j].Key)
			return a < b
		})
		return append(indexed, others...)
	case []any:
		if len(value) > 0 {
			return pyjson.Object{{Key: "0"}}
		}
	case string:
		if value != "" {
			return pyjson.Object{{Key: "0"}}
		}
	}
	return nil
}

// hookTrustEntriesIndex reports whether key is an array index: canonical decimal, 0 to 2^32-2.
func hookTrustEntriesIndex(key string) (uint64, bool) {
	number, err := strconv.ParseUint(key, 10, 32)
	return number, err == nil && number != math.MaxUint32 && strconv.FormatUint(number, 10) == key
}

// hookTrustEntriesLabel is "rawEventName in EVENT_LABELS" and the template-literal spelling of
// EVENT_LABELS[eventName] in the key (hook-trust.ts:181, :206): the ten labels, and the members
// Object.prototype defines, which spell as the source of the inherited function (constructor
// is Object) or, for __proto__, as the object.
func hookTrustEntriesLabel(event string) (string, bool) {
	kind, label, known := hookTrustIdentityEvent(event)
	switch {
	case !known:
		return "", false
	case kind == hookTrustIdentityProto:
		return "[object Object]", true
	case kind == hookTrustIdentityFunction:
		if event == "constructor" {
			event = "Object"
		}
		return "function " + event + "() { [native code] }", true
	}
	return label, true
}

// hookTrustEntriesStrip is normalizeHookPath (hook-trust.ts:140-142): one leading "./".
func hookTrustEntriesStrip(path string) string { return strings.TrimPrefix(path, "./") }

// hookTrustEntriesSafeValue is assertSafeHeaderValue (hook-trust.ts:158-160).
func hookTrustEntriesSafeValue(value, label string) error {
	if value == "" || strings.ContainsAny(value, "\"\\\r\n") {
		return errors.New(label + " contains characters unsafe for a TOML quoted key")
	}
	return nil
}

// hookTrustEntriesRootEscape is the text of the error os.Root returns for a name or a link that
// leaves the root (os.errPathEscapes, which the os package does not export). The tests assert the
// oracle text this produces, so a change of the os text fails them.
const hookTrustEntriesRootEscape = "path escapes from parent"

// hookTrustEntriesReadContained is containedPluginFile (hook-trust.ts:144-156) and the read that
// follows it: the file reference names has to lie inside the real path of the plugin root, both
// lexically and after symlinks. The reference is stripped of "./" a second time before it is
// resolved, and an absolute result of that resolves to itself, as path.resolve does. Two things
// differ from the oracle on purpose, answers to Devin's security findings on this port
// (docs/port-cxc/known-defects.md, port: fixed): the manifest goes through the same check, and
// the file is opened through an os.Root of the real plugin root and read from that handle. The
// guarantee covers what happens under the root once it is open: a link that stays inside is
// followed and one that leaves it, however it is swapped around the check, is refused at the
// open. The root path itself is resolved once by EvalSymlinks and opened by os.OpenRoot, which
// follows links in the root's own name. The Root also refuses an absolute link, even when it
// points inside, a link that leaves the root and comes back, and a chain of more than 8 links.
// The type of the opened file then decides what it is: a directory is EISDIR and any other file
// that is not regular is refused. observe is a test seam, called with "open" just before the
// open; production passes nil.
func hookTrustEntriesReadContained(pluginRoot, reference string, observe func(stage string)) ([]byte, error) {
	absolute, err := filepath.Abs(pluginRoot)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	if filepath.IsAbs(reference) {
		return nil, errors.New("plugin manifest path must be relative: " + reference)
	}
	stripped := hookTrustEntriesFSPath(hookTrustEntriesStrip(reference))
	candidate := filepath.Join(root, stripped)
	if filepath.IsAbs(stripped) {
		candidate = filepath.Clean(stripped)
	}
	if !hookTrustEntriesInside(root, candidate) {
		return nil, errors.New("plugin manifest path escapes plugin root: " + reference)
	}
	within, err := filepath.Rel(root, candidate)
	if err != nil {
		return nil, err
	}
	rooted, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	if observe != nil {
		observe("open")
	}
	file, err := rooted.Open(within)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) && pathErr.Err.Error() == hookTrustEntriesRootEscape {
			return nil, errors.New("plugin manifest symlink escapes plugin root: " + reference)
		}
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case info.IsDir():
		return nil, &fs.PathError{Op: "read", Path: candidate, Err: syscall.EISDIR}
	case !info.Mode().IsRegular():
		return nil, &fs.PathError{Op: "read", Path: candidate, Err: errors.New("not a regular file")}
	}
	return io.ReadAll(file)
}

// hookTrustEntriesInside is "path === root || path.startsWith(root + sep)".
func hookTrustEntriesInside(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// hookTrustEntriesFSPath is how Node hands a string to the file system: a lone surrogate, which
// the JSON reader holds as its three WTF-8 bytes, is U+FFFD there. Keys and messages keep the
// reference as written.
func hookTrustEntriesFSPath(path string) string {
	if utf8.ValidString(path) {
		return path
	}
	var builder strings.Builder
	for i := 0; i < len(path); {
		r, size := pyjson.CodePoint(path, i)
		if pyjson.IsSurrogate(r) {
			r = utf8.RuneError
		}
		builder.WriteRune(r)
		i += size
	}
	return builder.String()
}

// hookTrustEntriesUTF8 is Buffer.toString("utf8"): one U+FFFD for each maximal invalid subpart
// (the WHATWG rule), where strings.ToValidUTF8 merges a run into one. A byte order mark stays.
func hookTrustEntriesUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var builder strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			size = hookTrustEntriesSubpart(data)
		}
		if r == utf8.RuneError {
			builder.WriteRune(utf8.RuneError)
		} else {
			builder.Write(data[:size])
		}
		data = data[size:]
	}
	return builder.String()
}

// hookTrustEntriesSubpart is the length of the maximal subpart of a well-formed UTF-8 sequence
// that starts data, which does not start a well-formed one.
func hookTrustEntriesSubpart(data []byte) int {
	lower, upper, need := byte(0x80), byte(0xbf), 0
	switch lead := data[0]; {
	case lead >= 0xc2 && lead <= 0xdf:
		need = 1
	case lead == 0xe0:
		lower, need = 0xa0, 2
	case lead >= 0xe1 && lead <= 0xec, lead == 0xee, lead == 0xef:
		need = 2
	case lead == 0xed:
		upper, need = 0x9f, 2
	case lead == 0xf0:
		lower, need = 0x90, 3
	case lead >= 0xf1 && lead <= 0xf3:
		need = 3
	case lead == 0xf4:
		upper, need = 0x8f, 3
	default:
		return 1
	}
	length := 1
	for ; length <= need && length < len(data); length++ {
		if data[length] < lower || data[length] > upper {
			break
		}
		lower, upper = 0x80, 0xbf
	}
	return length
}

// hookTrustEntriesMatcherValid answers new RegExp(matcher) (hook-trust.ts:191-197) with Go's
// grammar: a pattern regexp.Compile accepts is valid, and so is one it refuses only for
// lookaround or a backreference, which JavaScript accepts and RE2 has no syntax for. The
// grammars still differ (docs/port-cxc/known-defects.md, port: kept).
func hookTrustEntriesMatcherValid(matcher string) bool {
	if _, err := regexp.Compile(matcher); err == nil {
		return true
	}
	_, err := regexp.Compile(hookTrustEntriesAdmit(matcher))
	return err == nil
}

// hookTrustEntriesAdmit rewrites the lookaround openers to a plain group and every numbered or
// named backreference to one character (\x01, which keeps a range that ends in an escape in
// order), skipping an escaped pair so that \\1 is not a backreference. The rewrite is applied
// inside classes too, where it only changes which characters the class holds.
func hookTrustEntriesAdmit(pattern string) string {
	var builder strings.Builder
	for i := 0; i < len(pattern); {
		rest := pattern[i:]
		switch {
		case rest[0] == '\\' && len(rest) > 1:
			length := 2
			switch next := rest[1]; {
			case next >= '1' && next <= '9':
				for length < len(rest) && rest[length] >= '0' && rest[length] <= '9' {
					length++
				}
				builder.WriteString(`\x01`)
			case next == 'k' && length < len(rest) && rest[length] == '<' && strings.IndexByte(rest[length:], '>') > 0:
				length += strings.IndexByte(rest[length:], '>') + 1
				builder.WriteString(`\x01`)
			default:
				builder.WriteString(rest[:length])
			}
			i += length
		case strings.HasPrefix(rest, "(?=") || strings.HasPrefix(rest, "(?!"):
			builder.WriteString("(?:")
			i += 3
		case strings.HasPrefix(rest, "(?<=") || strings.HasPrefix(rest, "(?<!"):
			builder.WriteString("(?:")
			i += 4
		default:
			builder.WriteByte(rest[0])
			i++
		}
	}
	return builder.String()
}
