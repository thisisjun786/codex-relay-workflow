package source

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf16"
)

// maxContentSize is the largest file the oracle hashes: Node's readFileSync refuses a bigger one, and the oracle
// then treats the entry as having no content.
const maxContentSize = 1<<31 - 1

// contentHash is the hash of an entry's bytes when the path exists, false when it has none. Existence decides this,
// not the status letter: a renamed file edited again stays "RM", so the letter would hide later edits. A symlink
// hashes its target string; anything that is neither a regular file nor a symlink has no content.
func contentHash(abs string) (string, bool) {
	info, err := os.Lstat(abs)
	if err != nil {
		return "", false
	}
	sum := sha256.New()
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return "", false
		}
		sum.Write([]byte(decodeUTF8([]byte(target))))
	case info.Mode().IsRegular() && info.Size() <= maxContentSize:
		file, err := os.Open(abs)
		if err != nil {
			return "", false
		}
		defer file.Close()
		if _, err := io.Copy(sum, file); err != nil {
			return "", false
		}
	default:
		return "", false
	}
	return hex.EncodeToString(sum.Sum(nil)), true
}

// hashRecords hashes the whole non-clean set ordered by path (UTF-16 units, as the oracle's comparison orders). There
// is no metadata-only shortcut: path+size+mtime collides on a same-size edit with a restored timestamp.
func hashRecords(cwd string, records []statusRecord) string {
	sorted := slices.Clone(records)
	slices.SortStableFunc(sorted, func(a, b statusRecord) int { return slices.Compare(a.path, b.path) })
	h := sha256.New()
	for _, rec := range sorted {
		path := text(rec.path)
		h.Write([]byte(text(rec.xy) + "\x00" + path + "\x00"))
		if rec.hasOrig {
			h.Write([]byte(text(rec.origPath) + "\x00"))
		}
		if content, ok := contentHash(filepath.Join(cwd, path)); ok {
			h.Write([]byte(content))
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// text encodes UTF-16 units as UTF-8; a lone surrogate becomes U+FFFD, as Node encodes it.
func text(units []uint16) string { return string(utf16.Decode(units)) }
