package hook

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

var prescanRowFields = []string{"recordVersion", "event", "at", "adapterOutcome", "processEnding", "stdoutReading", "guardState", "guardDecision", "guardMode", "assignmentId", "guardRecordedAs", "held", "eventKey", "eventIdentity", "identityScanMs", "acceptance", "acceptedAs", "guardInvoked", "configuration", "elapsedMs", "sessionId", "turnId", "stopHookActive", "exitCode", "signal", "errno", "guardElapsedMs", "guardStderr", "detail"}

// NativePrescanUnreachable is the narrow decision-22 reader extension chosen by
// Jun on 2026-09-28. Only a failed native dial may carry payload fields without an
// identity scan. It proves non-evaluation, never acceptance or a guard verdict.
// Existing readers must retain their other validation; this is not a general
// relaxation for rows whose acceptance happens to be null.
func NativePrescanUnreachable(row Object) bool {
	if len(row) != len(prescanRowFields) {
		return false
	}
	for _, name := range prescanRowFields {
		if _, ok := evidence.Lookup(row, name); !ok {
			return false
		}
	}
	version, integer := evidence.PyInt(get(row, "recordVersion"))
	if !integer || version != 2 {
		return false
	}
	if get(row, "event") != "Stop" || get(row, "adapterOutcome") != "guard_unreachable" || get(row, "processEnding") != "not_started" || get(row, "stdoutReading") != "said_nothing" || get(row, "guardInvoked") != true || get(row, "held") != false || get(row, "guardStderr") != "" {
		return false
	}
	if mode := get(row, "guardMode"); mode != Observe && mode != Hold {
		return false
	}
	for _, name := range []string{"acceptance", "acceptedAs", "identityScanMs", "eventKey", "eventIdentity", "guardState", "guardDecision", "assignmentId", "guardRecordedAs", "exitCode", "signal"} {
		if get(row, name) != nil {
			return false
		}
	}
	for _, name := range []string{"elapsedMs", "guardElapsedMs"} {
		n, ok := evidence.PyInt(get(row, name))
		if !ok || n < 0 {
			return false
		}
	}
	stamp, ok := get(row, "at").(string)
	if !ok {
		return false
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", stamp)
	if err != nil || at.Format("2006-01-02T15:04:05Z") != stamp {
		return false
	}
	if !journalAbsolutePath(text(get(row, "configuration"))) {
		return false
	}
	var number syscall.Errno
	switch get(row, "errno") {
	case "ENOENT":
		number = syscall.ENOENT
	case "ECONNREFUSED":
		number = syscall.ECONNREFUSED
	case "EACCES":
		number = syscall.EACCES
	case "EAGAIN":
		number = syscall.EAGAIN
	case "ENOTDIR":
		number = syscall.ENOTDIR
	default:
		return false
	}
	message := number.Error()
	message = strings.ToUpper(message[:1]) + message[1:]
	prefix := fmt.Sprintf("the configured runtime could not be run: [Errno %d] %s: ", number, message)
	detail, ok := get(row, "detail").(string)
	if !ok || !strings.HasPrefix(detail, prefix) {
		return false
	}
	spelled := strings.TrimPrefix(detail, prefix)
	socket, ok := pythonQuotedPath(spelled)
	return ok && journalAbsolutePath(socket) && filepath.Base(socket) == "control.sock" && evidence.StrRepr(socket) == spelled
}

func journalAbsolutePath(path string) bool {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || !utf8.ValidString(path) || len(path) >= 4096 {
		return false
	}
	normalized := filepath.Clean(path)
	// posixpath.normpath preserves exactly two leading slashes.
	if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
		normalized = "/" + normalized
	}
	if normalized != path {
		return false
	}
	for _, name := range strings.Split(path, "/") {
		if len(name) > 255 {
			return false
		}
	}
	return true
}
func pythonQuotedPath(spelled string) (string, bool) {
	if len(spelled) < 2 {
		return "", false
	}
	quote := spelled[0]
	if (quote != '\'' && quote != '"') || spelled[len(spelled)-1] != quote {
		return "", false
	}
	source := spelled[1 : len(spelled)-1]
	var out strings.Builder
	for source != "" {
		r, _, rest, err := strconv.UnquoteChar(source, quote)
		if err != nil {
			return "", false
		}
		out.WriteRune(r)
		source = rest
	}
	return out.String(), true
}

// ReadNativePrescanRow enforces the same regular-file, no-symlink and exact-byte
// journal boundary as completion._read_record before accepting the new shape.
// A false result says only that this extension cannot accept it; other row kinds
// remain the owning reader's responsibility.
func ReadNativePrescanRow(path string) (Object, bool) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil || len(raw) > maxInputBytes {
		return nil, false
	}
	row, err := decodeObject(raw)
	if err != nil || !NativePrescanUnreachable(row) {
		return nil, false
	}
	if !bytes.Equal(raw, []byte(evidence.Dumps(row, false, true, true)+"\n")) {
		return nil, false
	}
	return row, true
}
