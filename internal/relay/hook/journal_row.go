package hook

import (
	"encoding/json"
	"fmt"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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
		if _, ok := row.Lookup(name); !ok {
			return false
		}
	}
	version, integer := evidence.PyInt(row.Get("recordVersion"))
	if !integer || version != 2 {
		return false
	}
	if row.Get("event") != "Stop" || row.Get("adapterOutcome") != "guard_unreachable" || row.Get("processEnding") != "not_started" || row.Get("stdoutReading") != "said_nothing" || row.Get("guardInvoked") != true || row.Get("held") != false || row.Get("guardStderr") != "" {
		return false
	}
	if mode := row.Get("guardMode"); mode != Observe && mode != Hold {
		return false
	}
	for _, name := range []string{"acceptance", "acceptedAs", "identityScanMs", "eventKey", "eventIdentity", "guardState", "guardDecision", "assignmentId", "guardRecordedAs", "exitCode", "signal"} {
		if row.Get(name) != nil {
			return false
		}
	}
	for _, name := range []string{"elapsedMs", "guardElapsedMs"} {
		if !Count(row.Get(name), true) {
			return false
		}
	}
	stamp, ok := row.Get("at").(string)
	if !ok {
		return false
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", stamp)
	if err != nil || at.Format("2006-01-02T15:04:05Z") != stamp {
		return false
	}
	if !journalAbsolutePath(pyjson.Text(row.Get("configuration"))) {
		return false
	}
	var number syscall.Errno
	switch row.Get("errno") {
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
	detail, ok := row.Get("detail").(string)
	if !ok || !strings.HasPrefix(detail, prefix) {
		return false
	}
	spelled := strings.TrimPrefix(detail, prefix)
	socket, ok := pythonQuotedPath(spelled)
	return ok && journalAbsolutePath(socket) && filepath.Base(socket) == "control.sock" && pyvalue.StrRepr(socket) == spelled
}

// Count is completion._is_count: an integer of any size, never a bool, that is non-negative
// (zero) or positive. Python's int has no width, so a count past int64 is still one: Decode
// hands it over as the json.Number it could not narrow.
func Count(v any, zero bool) bool {
	var sign int
	switch n := v.(type) {
	case int64:
		sign = big.NewInt(n).Sign()
	case int:
		sign = big.NewInt(int64(n)).Sign()
	case json.Number:
		i, ok := new(big.Int).SetString(n.String(), 10)
		if !ok {
			return false
		}
		sign = i.Sign()
	default:
		return false
	}
	if zero {
		return sign >= 0
	}
	return sign > 0
}

// journalAbsolutePath is os.path.isabs(path) and path == os.path.normpath(path) and
// completion._path_the_system_takes(path): a lone surrogate is judged as os.fsencode encodes it,
// so a path whose name holds a byte that is not UTF-8 (U+DC80..U+DCFF) is one the system takes.
func journalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && PathTheSystemTakes(path) && store.Normpath(path) == path
}

// pythonQuotedPath is ast.literal_eval of a quoted str literal. A \u or \U escape of a surrogate
// is the lone surrogate Python makes of it (repr() writes one so), which strconv refuses; it is
// kept as the WTF-8 a JSON string holds one in.
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
		if r, rest, ok := surrogateEscape(source); ok {
			out.Write([]byte{0xe0 | byte(r>>12), 0x80 | byte(r>>6)&0x3f, 0x80 | byte(r)&0x3f})
			source = rest
			continue
		}
		r, _, rest, err := strconv.UnquoteChar(source, quote)
		if err != nil {
			return "", false
		}
		out.WriteRune(r)
		source = rest
	}
	return out.String(), true
}

// surrogateEscape is the surrogate a \uXXXX or \UXXXXXXXX escape at the start of s names.
func surrogateEscape(s string) (rune, string, bool) {
	width := 0
	switch {
	case strings.HasPrefix(s, `\u`):
		width = 4
	case strings.HasPrefix(s, `\U`):
		width = 8
	}
	if width == 0 || len(s) < 2+width {
		return 0, s, false
	}
	v, err := strconv.ParseUint(s[2:2+width], 16, 32)
	if err != nil || v < 0xd800 || v > 0xdfff {
		return 0, s, false
	}
	return rune(v), s[2+width:], true
}
