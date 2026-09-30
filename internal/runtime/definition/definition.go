// Package definition is the one compatibility definition (OPS-1.1): the two components, the
// console-script names the installer places beside crw as links to it, each component's version
// and licence, and the tool that identifies the bridge.
//
// Until todo 44 scripts/crw_runtime/components.json was that definition, read by the Python
// installer and the developer harnesses, and this package carried the fields a Go install uses
// and a test kept them equal to the file. The file left with its last Python reader, and this
// package is the only copy (decision 47). The upstream provenance it recorded for the ported
// bridge is packages/codex-thread-bridge/PROVENANCE.md. Nothing here carries a per-target binary digest: release digests live in the
// release's SHA256SUMS and in the host record (decision 35).
package definition

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Version is the definitionVersion a host record states (decision 34: the host record stays at
// definitionVersion 1). It is not the version of a file: no file carries the definition since
// todo 44.
const Version = 1

// Component is one component of the definition.
type Component struct {
	Name          string
	ConsoleScript string
	Version       string
	LicencePath   string
	IdentityTool  string
}

// Names of the two components.
const (
	Bridge = "codex-thread-bridge"
	Relay  = "codex-session-relay"
)

// Components is the definition, in the order components.json listed them.
var Components = []Component{
	{
		Name: Bridge, ConsoleScript: "codex-thread-bridge", Version: "0.2.0",
		LicencePath: "packages/codex-thread-bridge/LICENSE", IdentityTool: "get_capabilities",
	},
	{
		Name: Relay, ConsoleScript: "codex-session-relay", Version: "0.2.0",
		LicencePath: "LICENSE",
	},
}

// HookScript is the third compatibility link beside crw: the completion hook's entry point.
const HookScript = "crw-completion-hook"

// Links are the names the installer places beside crw, each a symlink to it.
func Links() []string {
	return []string{Components[1].ConsoleScript, Components[0].ConsoleScript, HookScript}
}

// Of is the component with this name.
func Of(name string) (Component, bool) {
	for _, c := range Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// Digest is definition.ops12_digest: SHA-256 over a directory, walking every file except
// anything under __pycache__, sorted by POSIX relative path, each contributing its relative
// path, a zero byte and the SHA-256 of its bytes. A subtree that cannot be read fails the walk
// instead of being left out. Symbolic links to directories are not descended; a link to a file
// counts as the file it names. An entry whose target is absent (a dangling link) is not a file,
// as os.DirEntry.is_file answers; any other failure to examine one (a link loop, a directory
// without search permission) fails the walk, as it raises out of files_under.
//
// The relative paths are Python's: os.fsdecode spells a byte that is not part of UTF-8 as the
// lone surrogate U+DC80..U+DCFF, sorted() orders them by code point, and .encode() refuses a
// surrogate. So the files are taken in that order, and the first path that is not UTF-8 fails
// the walk where Python raises UnicodeEncodeError, after every file before it has been read:
// no digest is answered for a tree Python cannot digest.
func Digest(root string) (string, error) {
	type file struct {
		relative string
		key      []rune
	}
	var files []file
	pending := []string{root}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if entry.Name() == "__pycache__" {
					continue
				}
				pending = append(pending, path)
				continue
			}
			info, err := os.Stat(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				return "", err
			case !info.Mode().IsRegular():
				continue
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return "", err
			}
			relative = filepath.ToSlash(relative)
			files = append(files, file{relative: relative, key: fsdecode(relative)})
		}
	}
	sort.Slice(files, func(i, j int) bool { return slices.Compare(files[i].key, files[j].key) < 0 })
	digest := sha256.New()
	for _, f := range files {
		if strings.Contains("/"+f.relative+"/", "/__pycache__/") {
			continue
		}
		if err := encodeUTF8(f.key); err != nil {
			return "", err
		}
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(f.relative)))
		if err != nil {
			return "", err
		}
		inner := sha256.New()
		_, err = io.Copy(inner, file)
		_ = file.Close()
		if err != nil {
			return "", err
		}
		digest.Write([]byte(f.relative))
		digest.Write([]byte{0})
		digest.Write(inner.Sum(nil))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// fsdecode is os.fsdecode of a POSIX path as code points: UTF-8, with each byte that is not
// part of a valid sequence the lone surrogate U+DC00+byte (surrogateescape).
func fsdecode(path string) []rune {
	out := make([]rune, 0, len(path))
	for i := 0; i < len(path); {
		r, size := utf8.DecodeRuneInString(path[i:])
		if r == utf8.RuneError && size == 1 {
			r = 0xdc00 + rune(path[i])
		}
		out = append(out, r)
		i += size
	}
	return out
}

// UnicodeEncodeError is str.encode()'s refusal of a surrogate, in Python's words.
type UnicodeEncodeError struct {
	Start, End int // code point positions, End exclusive
	Char       rune
}

func (e *UnicodeEncodeError) Error() string {
	if e.End-e.Start == 1 {
		return fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", e.Char, e.Start)
	}
	return fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", e.Start, e.End-1)
}

// encodeUTF8 is whether str.encode() accepts the code points: it refuses the first run of
// surrogates.
func encodeUTF8(key []rune) error {
	for start, r := range key {
		if !isSurrogate(r) {
			continue
		}
		end := start + 1
		for end < len(key) && isSurrogate(key[end]) {
			end++
		}
		return &UnicodeEncodeError{Start: start, End: end, Char: r}
	}
	return nil
}

func isSurrogate(r rune) bool { return r >= 0xd800 && r <= 0xdfff }
