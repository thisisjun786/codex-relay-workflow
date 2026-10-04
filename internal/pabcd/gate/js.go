package gate

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// maxText is V8's longest string, as bytes: the most the oracle's readFileSync(path, "utf8") returns. A longer file is refused.
const maxText = 0x1fffffe8

// readFile is the bytes of the regular file at path, refused when there are more than limit. It opens the file as the oracle's reads
// cannot: a link at the last element is refused (ELOOP) and a pipe does not block, so a file swapped in after the path was checked
// is not followed (a link in a directory above is not defended).
func readFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("file is too long")
	}
	return data, err
}

// decodeJSON is JSON.parse of utf8 bytes: one value and nothing after it, and a number keeps its text so 1e999 reads as Infinity.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(source.DecodeUTF8(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

// jsNumber is v as a JavaScript number (typeof v === "number"); a literal too large for a float64 is ±Infinity.
func jsNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil || errors.Is(err, strconv.ErrRange)
}

// jsNumberText is a number in a template literal: encoding/json prints a float64 as ECMAScript does, bar the infinities and -0.
func jsNumberText(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	text, _ := json.Marshal(f)
	return string(text)
}

// dateParses is !Number.isNaN(Date.parse(s)) for the standard spellings only (known-defects names the differences).
func dateParses(s string) bool {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02", "2006-01", "2006", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// resolve is path.resolve(base, p): an absolute p stands alone, a relative one is taken from base, and the result is clean.
func resolve(base, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// within is path.relative(base, p) naming something below base: not empty (filepath.Rel says "."), not "..", not "../...".
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// posixDirname is path.posix.dirname of a relative path: unlike filepath.Dir it does not clean ("a//b" is "a/", "./a/b" is "./a").
func posixDirname(p string) string {
	slashes := true
	for i := len(p) - 1; i >= 1; i-- {
		if p[i] != '/' {
			slashes = false
		} else if !slashes {
			return p[:i]
		}
	}
	return "."
}

// The oracle's patterns as predicates: p.split(/[\\/]/).includes(".."), /^[0-9a-f]{64}$/ and /^c-[1-9]\d*$/ (\d is ASCII).
func hasDotDot(p string) bool {
	return slices.Contains(strings.Split(strings.ReplaceAll(p, "\\", "/"), "/"), "..")
}

func isSHA256(s string) bool { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }

func isCriterionID(s string) bool {
	n, ok := strings.CutPrefix(s, "c-")
	return ok && n != "" && n[0] != '0' && strings.Trim(n, "0123456789") == ""
}
