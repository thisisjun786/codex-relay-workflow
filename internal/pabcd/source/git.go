package source

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// GitEnv is base without the variables that point git at another repository (inherited, they would redirect
// status and rev-parse to another tree, so a capture "for" a bound worktree could describe the native checkout);
// nil means the process environment.
func GitEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	return slices.DeleteFunc(slices.Clone(base), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return isRoutingVar(name)
	})
}

func isRoutingVar(name string) bool {
	return slices.Contains([]string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"}, name)
}

// run runs a command in cwd with the routing variables removed and returns its stdout. Stderr is read and dropped:
// git failing here is an outcome the caller handles, so its diagnostics must not reach the user. Every failure
// (command absent, cwd missing, non-zero exit, signal, over the limit) is the same error. The limit is Node's
// maxBuffer: stdout and stderr share it, and going over it kills the child and returns at once, even if a
// grandchild still holds the pipes.
func run(cwd string, limit int, name string, args ...string) ([]byte, error) {
	return Run(cwd, GitEnv(nil), limit, name, args...)
}

// Run is run for a caller that brings its own environment (the session source binding removes fewer routing variables than
// GitEnv); nil is the process environment.
func Run(cwd string, env []string, limit int, name string, args ...string) ([]byte, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return nil, err
	}
	defer func() { _, _ = outR.Close(), errR.Close() }()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = cwd, env, outW, errW
	err = cmd.Start()
	_, _ = outW.Close(), errW.Close() // the child holds its own copies
	if err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		used     int
		over     bool
		out      bytes.Buffer
		drainers sync.WaitGroup
	)
	drain := func(r *os.File, keep bool) {
		defer drainers.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			mu.Lock()
			if used += n; used > limit && !over {
				over = true
				_ = cmd.Process.Kill()
				_, _ = outR.Close(), errR.Close()
			}
			if keep && !over {
				out.Write(buf[:n])
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}
	drainers.Add(2)
	go drain(outR, true)
	go drain(errR, false)
	drainers.Wait()
	werr := cmd.Wait()
	if over {
		return nil, errors.New("output exceeds the limit")
	}
	if werr != nil {
		return nil, werr
	}
	return out.Bytes(), nil
}

// statusRecord is one entry of git status --porcelain=v1 -z. Its texts stay UTF-16 code unit sequences, as the
// oracle's JavaScript strings are, until they are hashed or used as a path: cutting a field can leave a lone
// surrogate, which orders and compares by its own value and only becomes U+FFFD when encoded.
type statusRecord struct {
	xy, path []uint16
	origPath []uint16 // the rename or copy source, when hasOrig
	hasOrig  bool
}

// parseStatusZ parses git status --porcelain=v1 -z --untracked-files=all. Without -z git C-quotes a path holding
// a space, quote, tab or newline; without --untracked-files=all an untracked directory collapses into one entry
// that does not change when a file inside it changes.
//
// Records are "XY<space><path>\0"; one whose index column is R or C is followed by a field holding the original
// path. The oracle works on JavaScript strings, so fields are decoded like Node decodes UTF-8 and cut in UTF-16
// units; a field that is not NUL-terminated is dropped and one shorter than four units is skipped.
func parseStatusZ(out []byte) []statusRecord {
	var fields [][]uint16
	for _, field := range bytes.Split(out, []byte{0}) {
		fields = append(fields, utf16.Encode([]rune(decodeUTF8(field))))
	}
	fields = fields[:len(fields)-1] // what follows the last NUL is an unterminated field, which is dropped
	var records []statusRecord
	for i := 0; i < len(fields); i++ {
		u := fields[i]
		if len(u) < 4 {
			continue
		}
		rec := statusRecord{xy: u[:2], path: u[3:]}
		if u[0] == 'R' || u[0] == 'C' {
			rec.hasOrig = true
			if i++; i < len(fields) {
				rec.origPath = fields[i]
			}
		}
		records = append(records, rec)
	}
	return records
}

// decodeUTF8 decodes b as Node's Buffer.toString("utf8") does: each maximal invalid subpart becomes one U+FFFD
// (the WHATWG rule), where Go's own conversion emits one per byte.
func decodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			n = maximalSubpart(b)
		}
		sb.WriteRune(r)
		b = b[n:]
	}
	return sb.String()
}

// maximalSubpart is the length of the longest prefix of b, whose first rune is invalid, that could begin a
// well-formed sequence: the lead byte plus the following bytes that some completion with 0x80 keeps valid.
func maximalSubpart(b []byte) int {
	want := 1
	switch c := b[0]; {
	case c >= 0xC2 && c <= 0xDF:
		want = 2
	case c >= 0xE0 && c <= 0xEF:
		want = 3
	case c >= 0xF0 && c <= 0xF4:
		want = 4
	}
	n := 1
	for n < min(want, len(b)) && utf8.Valid(append(slices.Clone(b[:n+1]), bytes.Repeat([]byte{0x80}, want-n-1)...)) {
		n++
	}
	return n
}
