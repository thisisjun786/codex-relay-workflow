package sandboxroot

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// CRW-1308 (Jun, 10-13): no caller refuses on the rule's verdict. The rule's weak point was never the sandbox: it is a tool that
// refuses to work when any directory above it holds a .git (AAS fixes its own check, AAS-89). Check, CheckWritable, CheckDestination
// and CheckAll still judge, and every caller hands their verdict to Warn and goes on: the App Server call, the resume, the spawn
// and the Codex run happen as they would for an allowed root, and the verdict is left as a warning line and a record.

// WarningPrefix starts the warning line Warn writes to standard error.
const WarningPrefix = "[crw warning] sandbox root not refused (CRW-1308): "

// DecisionWarn is the decision every record names: the verdict was a refusal, and the caller went on.
const DecisionWarn = "warn"

// RecordFile is the record of the warnings, one JSON object a line, in the crw directory of the Codex home
// (<CODEX_HOME>/crw/sandbox-root-warnings.jsonl, beside CRW-1307's hook-decisions).
const RecordFile = "sandbox-root-warnings.jsonl"

// warningOutput is where the warning line goes; a test may replace it (SetWarningOutput).
var warningOutput struct {
	sync.Mutex
	w io.Writer
}

// SetWarningOutput sends the warning lines to w until the returned restore runs (nil restores standard error).
func SetWarningOutput(w io.Writer) (restore func()) {
	warningOutput.Lock()
	prev := warningOutput.w
	warningOutput.w = w
	warningOutput.Unlock()
	return func() {
		warningOutput.Lock()
		warningOutput.w = prev
		warningOutput.Unlock()
	}
}

// Record is one line of the record. It names the caller, the rule and the paths the verdict was made on; never a message, a
// prompt or a setting other than the paths.
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	Caller        string `json:"caller"`
	Decision      string `json:"decision"`
	Rule          string `json:"rule"`
	Path          string `json:"path"`
	Resolved      string `json:"resolved,omitempty"`
	Project       string `json:"project,omitempty"`
	DecidedAt     string `json:"decidedAt"`
}

// Warn is a caller's whole answer to a verdict: nil leaves nothing; a refusal is recorded and one warning line names it, and the
// caller goes on. caller names the path that asked ("bridge/create_thread", "relay/send_message_to_thread", ...). It returns the
// warning text ("" for nil), for a caller that also has a place of its own to show it. A record that cannot be written is said in
// the line and never stops the caller.
func Warn(caller string, verdict error) string {
	if verdict == nil {
		return ""
	}
	record := Record{SchemaVersion: 1, Caller: caller, Decision: DecisionWarn, Rule: "error", DecidedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
	var refusal *Refusal
	if errors.As(verdict, &refusal) {
		record.Rule, record.Path, record.Resolved, record.Project = refusal.Rule, refusal.Path, refusal.Resolved, refusal.Project
	}
	text := caller + ": " + verdict.Error()
	if !writeRecord(record) {
		text += " (no record written)"
	}
	warningOutput.Lock()
	out := warningOutput.w
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintln(out, WarningPrefix+text)
	warningOutput.Unlock()
	return WarningPrefix + text
}

// recordMu keeps the lines of one process whole; O_APPEND keeps those of several processes whole.
var recordMu sync.Mutex

// RecordPath is the record file under a Codex home.
func RecordPath(codexHome string) string {
	return crwconfig.JoinRoot(crwconfig.JoinRoot(codexHome, "crw"), RecordFile)
}

// writeRecord appends one line to the record of the Codex home the rule reads (CODEX_HOME, else HOME/.codex, else the passwd
// home's .codex), and reports whether it did.
func writeRecord(record Record) bool {
	getenv, accountHome := facts()
	home := codexHome(getenv, accountHome)
	if home == "" || !filepath.IsAbs(home) {
		return false
	}
	line, err := json.Marshal(record)
	if err != nil {
		return false
	}
	path := RecordPath(home)
	recordMu.Lock()
	defer recordMu.Unlock()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.Write(append(line, '\n'))
	return f.Close() == nil && werr == nil
}

// ReadRecords is every record under a Codex home, in the order written, for a test or a diagnostic reader. A line that is not a
// record is skipped.
func ReadRecords(codexHome string) []Record {
	f, err := os.Open(RecordPath(codexHome))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Record
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record Record
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			out = append(out, record)
		}
	}
	return out
}
