package source

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
)

// GitEnv is base without the variables that point git at another repository (inherited, they would redirect
// status and rev-parse to another tree); nil means the process environment. The receipt command runs with it; the
// probes of this package and of the session binding run under gitprobe's probe policy instead (CRW-1135).
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

// ProbeOptions bound one probe: the output limit (stdout and stderr together, Node's maxBuffer), the time limit,
// and the trusted environment overrides applied once after gitprobe's probe environment.
type ProbeOptions struct {
	Limit   int
	Timeout time.Duration
	Env     []string
}

// ExitError is a git that ran and failed: its exit status (-1 when a signal ended it) and what it wrote to stderr,
// which is never shown to the user.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string { return "git exited with status " + strconv.Itoa(e.Code) }

// NotARepository is true only when git ran and said the directory is in no repository: exit status 128 with the
// C locale's discovery answer, "not a git repository (or any of the parent directories): .git" or, at a file
// system boundary, "not a git repository (or any parent up to mount point /)" (a probe that asks for it sets
// LC_ALL=C). "not a git repository: <gitdir>" is not that answer: it is
// a repository whose Git directory git could not read or accept. A git that could not start, was stopped by the
// time limit, went over the output limit or failed for another reason is not that answer either.
func NotARepository(err error) bool {
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 128 {
		return false
	}
	return strings.Contains(exit.Stderr, "not a git repository (or any of the parent directories)") ||
		strings.Contains(exit.Stderr, "not a git repository (or any parent up to mount point")
}

// captureTimeout bounds the identity capture's status, which reads the whole tree (and runs its clean filters);
// a probe of the session binding is bound by gitprobe.Timeout.
const captureTimeout = 5 * time.Minute

// run runs git in cwd under the probe policy with the capture's bounds and returns its stdout.
func run(cwd string, limit int, args ...string) ([]byte, error) {
	return Probe(cwd, ProbeOptions{Limit: limit, Timeout: captureTimeout}, args...)
}

// Probe runs git in cwd under gitprobe's read-only probe policy (every inherited GIT_* variable removed, no
// prompt, no hooks, no file system monitor, no optional locks), given through the environment so the argument
// list stays the one the oracle ran, and returns its stdout. Stderr is kept
// only for the ExitError: git failing here is an outcome the caller handles, so its diagnostics must not reach
// the user. Going over the output limit, as Node's maxBuffer does, or past the time limit kills the child and
// returns at once, even if a grandchild still holds the pipes.
func Probe(cwd string, o ProbeOptions, args ...string) ([]byte, error) {
	return runBounded(cwd, "git", args, gitprobe.ProbeEnv(nil, o.Env...), o.Limit, o.Timeout)
}

// runBounded runs name with args in cwd and env and returns its stdout, within limit bytes of output and timeout.
func runBounded(cwd, name string, args, env []string, limit int, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
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
		late     bool
		out      bytes.Buffer
		stderr   bytes.Buffer
		drainers sync.WaitGroup
	)
	stopReading := func() { // with mu held
		_ = cmd.Process.Kill()
		_, _ = outR.Close(), errR.Close()
	}
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !over {
			late = true
			stopReading()
		}
	})
	defer stop()
	drain := func(r *os.File, keep *bytes.Buffer) {
		defer drainers.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			mu.Lock()
			if used += n; used > limit && !over && !late {
				over = true
				stopReading()
			}
			if !over && !late {
				keep.Write(buf[:n])
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}
	drainers.Add(2)
	go drain(outR, &out)
	go drain(errR, &stderr)
	drainers.Wait()
	werr := cmd.Wait()
	mu.Lock()
	defer mu.Unlock()
	switch {
	case over:
		return nil, errors.New("output exceeds the limit")
	case late:
		return nil, ctx.Err()
	}
	if werr != nil {
		var exit *exec.ExitError
		if errors.As(werr, &exit) {
			return nil, &ExitError{Code: exit.ExitCode(), Stderr: stderr.String()}
		}
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

// DecodeUTF8 is decodeUTF8 for a caller that reads paths and git output the way Node does.
func DecodeUTF8(b []byte) string { return decodeUTF8(b) }

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
