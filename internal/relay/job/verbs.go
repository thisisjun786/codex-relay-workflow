package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CLIResult retains the oracle value and exit code. Relay dispatch wraps Out in
// its answer envelope; JSON forms carry data rather than a JSON string.
type CLIResult struct {
	Out  any
	Code int
}

// MaxCLIStdinBytes is MAX_STDIN_BYTES, actually a UTF-16 unit limit.
const MaxCLIStdinBytes = 1024 * 1024
const cliUsage = `crw relay job run [--note "..."] -- <command...>   백그라운드로 실행하고 id를 반환
crw relay job list [--json]                        이 디렉터리의 백그라운드 작업
crw relay job get <id> [--tail N]                  상태와 출력 꼬리
crw relay job cancel <id>                          중지
crw relay job off | on | status                    완료 웨이크 스위치 (이 워크트리)
crw relay job drain --session <id> [--json]        미전달 완료를 받아가고 전달 표시
crw relay job removal                              제거 체크리스트`

// cliFormatList lists the records and then the broken record files, which are reported and left as they are (CRW-1134).
func cliFormatList(recs []BgRecord, broken []BrokenRecord) string {
	if len(recs) == 0 && len(broken) == 0 {
		return "백그라운드 작업 없음"
	}
	lines := make([]string, len(recs), len(recs)+len(broken))
	for i, rec := range recs {
		lines[i] = DescribeRecord(rec)
		if IsTerminal(rec.Status) {
			delivered := "미전달"
			if rec.DeliveredAt != nil {
				delivered = "전달됨"
			}
			lines[i] += "  [" + delivered + "]"
		}
	}
	for _, b := range broken {
		lines = append(lines, "- "+b.ID+" (손상된 기록: "+b.Reason+") — 파일을 그대로 둡니다")
	}
	return strings.Join(lines, "\n")
}

// cliJSONList is the JSON list: an array of the records, as it was. With broken record files in the store it is an object that holds
// the records and the broken files instead, which the caller sees with a non-zero exit code, so a damaged store is never read as an
// empty one (CRW-1134).
func cliJSONList(recs []BgRecord, broken []BrokenRecord) (any, error) {
	items := make([]json.RawMessage, 0, len(recs))
	for _, rec := range recs {
		b, err := encode(rec)
		if err != nil {
			return nil, err
		}
		items = append(items, b)
	}
	var doc any = items
	if len(broken) > 0 {
		list := make([]map[string]string, len(broken))
		for i, b := range broken {
			list[i] = map[string]string{"id": b.ID, "reason": b.Reason}
		}
		doc = struct {
			Records []json.RawMessage   `json:"records"`
			Broken  []map[string]string `json:"broken"`
		}{items, list}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(string(b), pyjson.LoadOptions{Surrogates: true})
}

func cliRecord(rec BgRecord) (any, error) {
	b, err := encode(rec)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(string(b), pyjson.LoadOptions{Surrogates: true})
}

// CLIOptions keeps parsed values separate from command syntax.
type CLIOptions struct {
	Verb, ID            string
	Command             []string
	Note, Tail, Session *string
	JSON                bool
}

// RunParsedCLI executes validated values without interpreting metadata as argv.
func RunParsedCLI(opts CLIOptions, cwd string, getenv func(string) (string, bool), clock func() time.Time) (result CLIResult, err error) {
	result.Out = ""
	verb, id, asJSON := opts.Verb, opts.ID, opts.JSON
	switch verb {
	case "run":
		var session *string
		if s, ok := getenv("CODEX_THREAD_ID"); ok {
			session = &s
		}
		var rec BgRecord
		rec, err = RunBackground(cwd, RunOptions{SessionID: session, Command: opts.Command, Note: opts.Note}, clock)
		if err == nil {
			result.Out = rec.ID
			if rec.Status != StatusRunning { // the shell could not start: the record already says failed (CRW-1134)
				result = CLIResult{Out: rec.ID + " " + string(rec.Status) + " (셸을 시작하지 못했습니다)", Code: 1}
			}
			if asJSON {
				result.Out, err = cliRecord(rec)
			}
		}
	case "list":
		var recs []BgRecord
		recs, err = ListRecords(cwd, clock)
		broken := BrokenRecords(cwd)
		result.Out = cliFormatList(recs, broken)
		if len(broken) > 0 {
			result.Code = 1 // a broken record is not "no job": the exit code says so in the text and the JSON form alike (CRW-1134)
		}
		if err == nil && asJSON {
			result.Out, err = cliJSONList(recs, broken)
		}
	case "get", "cancel":
		rec, readErr := readRecord(cwd, id)
		var broken BrokenRecord
		if errors.As(readErr, &broken) {
			return CLIResult{broken.Error() + " — 파일을 그대로 둡니다: " + RecordPath(cwd, id), 1}, nil
		}
		if readErr != nil {
			code := 0
			if verb == "get" {
				code = 1
			}
			return CLIResult{"없는 id: " + id, code}, nil
		}
		if verb == "cancel" {
			return cliCancel(cwd, rec, clock)
		} else {
			rec, err = Reconcile(cwd, rec, clock)
			// The final newline ends the last line and is not a line of its own, and the lines are shown as they are, spaces and
			// blank lines included (CRW-1134; the oracle counted the empty string after the last newline and trimmed the output).
			body, _ := ReadText(OutPath(cwd, id))
			var lines []string
			if body != "" {
				lines = text.SplitLinesByteExact(strings.TrimSuffix(body, "\n"))
			}
			result.Out = DescribeRecord(rec)
			if tail := cliTail(opts.Tail, len(lines)); tail > 0 {
				result.Out = DescribeRecord(rec) + "\n\n" + strings.Join(lines[len(lines)-tail:], "\n")
			}
		}
	case "off", "on":
		result.Out, err = cliSwitch(cwd, verb, clock)
	case "status":
		state := ReadDisabledState(cwd)
		envOff := EnvDisabled(func(k string) string { v, _ := getenv(k); return v })
		wake, env, flag := "ON", "unset/on", "  파일 플래그: on"
		if state.Disabled || envOff {
			wake = "OFF"
		}
		if envOff {
			env = "off"
		}
		if state.Disabled {
			since := "?"
			if state.Since != nil {
				since = *state.Since
			}
			if state.Err != nil {
				since = "읽을 수 없음: " + state.Err.Error()
			}
			flag = "  파일 플래그: off (" + since + ")"
		}
		var recs []BgRecord
		recs, err = ListRecords(cwd, clock)
		count := "  작업 " + strconv.Itoa(len(recs)) + "건"
		if broken := BrokenRecords(cwd); len(broken) > 0 {
			count += ", 손상된 기록 " + strconv.Itoa(len(broken)) + "건 (crw relay job list)"
		}
		result.Out = strings.Join([]string{"wake: " + wake, flag, "  " + EnvVar + ": " + env, count}, "\n")
	case "drain":
		session := opts.Session
		if session == nil {
			return CLIResult{cliUsage, 1}, nil
		}
		body := cliDrain(cwd, session, clock)
		result.Out = body
		if body == "" {
			result.Out = "미전달 완료 없음"
		}
		if asJSON {
			result.Out = pyjson.Object{{Key: "delivered", Value: body != ""}, {Key: "text", Value: body}}
		}
	case "removal":
		result.Out = RemovalText()
	default:
		result.Out = cliUsage
	}
	if err != nil {
		result = CLIResult{Out: "", Code: 1}
	}
	return result, err
}

// cliCancel prints the record's status after the cancel. A cancel that has not seen the job stop says so with how to follow it (the
// status is cancellation-requested, not terminal), and a cancel that signalled nothing, or whose signal failed, exits 1 with why
// (CRW-1155).
func cliCancel(cwd string, rec BgRecord, clock func() time.Time) (CLIResult, error) {
	got, err := Cancel(cwd, rec, clock)
	var unproven ErrOwnerUnproven
	var signal SignalError
	switch {
	case errors.As(err, &unproven):
		return CLIResult{Out: got.ID + " " + string(got.Status) + "\n" + unproven.Error(), Code: 1}, nil
	case errors.As(err, &signal):
		return CLIResult{Out: got.ID + " " + string(got.Status) + "\n" + signal.Error(), Code: 1}, nil
	case err != nil:
		return CLIResult{Out: "", Code: 1}, err
	}
	out := got.ID + " " + string(got.Status)
	if got.Status == StatusCancelRequested {
		out += "\n아직 멈추지 않았습니다: 종료 신호를 보냈고 프로세스 그룹이 남아 있습니다. `crw relay job get " + got.ID +
			"`로 cancelled가 될 때까지 확인하고, 계속 남으면 다시 cancel하면 SIGKILL을 보냅니다."
	}
	return CLIResult{Out: out, Code: 0}, nil
}

func cliSwitch(cwd, verb string, clock func() time.Time) (string, error) {
	if _, err := EnsureDir(cwd); err != nil {
		return "", err
	}
	if verb == "on" && !ReadDisabledState(cwd).Disabled {
		return "bg wake 이미 ON", nil
	}
	path, event := DisabledPath(cwd), "disabled"
	out := "bg wake OFF (이 워크트리). 다시 켜려면: crw relay job on"
	if verb == "on" {
		path, event = EnabledAtPath(cwd), "enabled"
		out = "bg wake ON. 꺼져 있는 동안 끝난 작업은 웨이크하지 않고 crw relay job list 에만 남습니다."
	}
	// The switch and the enabled-at time change together under the store lock, and a failed on leaves both as they were: a
	// completion that ended while the wake was off is held back by that time, so a time written by an on that did not turn the wake on
	// would hide completions from the wake that is still off (CRW-1134).
	err := withLock(cwd, false, func() error {
		prev, prevErr := readText(path)
		if err := AtomicWrite(cwd, path, clock().UTC().Format(isoLayout)+"\n"); err != nil {
			return err
		}
		if verb != "on" {
			return nil
		}
		if err := RemovePath(cwd, DisabledPath(cwd)); err != nil {
			restoreText(cwd, path, prev, prevErr)
			return err
		}
		// RemovePath ignores what it cannot remove (a directory): the switch would stay off while on says ON (CRW-1134).
		if _, err := os.Lstat(DisabledPath(cwd)); !errors.Is(err, os.ErrNotExist) {
			restoreText(cwd, path, prev, prevErr)
			return fmt.Errorf("the off switch %s cannot be removed, so the wake stays off", DisabledPath(cwd))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	_ = appendLedger(cwd, Event{{"event", event}}, clock)
	return out, nil
}

// restoreText puts a file back as readText found it: its text, or no file when it was not there.
func restoreText(cwd, path, prev string, prevErr error) {
	if prevErr == nil {
		_ = AtomicWrite(cwd, path, prev)
	} else if errors.Is(prevErr, os.ErrNotExist) {
		_ = RemovePath(cwd, path)
	}
}

func cliTail(arg *string, available int) int {
	if arg == nil {
		return min(20, available)
	}
	body, end := text.Trim(*arg), 0
	if body != "" && (body[0] == '+' || body[0] == '-') {
		end++
	}
	for end < len(body) && body[end] >= '0' && body[end] <= '9' {
		end++
	}
	n, _ := strconv.ParseFloat(body[:end], 64)
	if n >= float64(available) {
		return available
	}
	return max(0, int(n))
}

// cliDrain selects and stamps under the store lock (deliver), so a drain and a hook never hand out one completion twice. Its text is
// the relay's answer, which the dispatcher writes after it returns: the stamp still comes before that write. The text keeps the wake
// budget as a JSON string, and a job it does not describe stays pending (CRW-1095).
func cliDrain(cwd string, session *string, clock func() time.Time) string {
	out, _ := deliver(cwd, session, clock, func(recs []BgRecord) (string, []BgRecord) {
		return fitWake(recs, completionBody, jsonSize)
	}, acceptAll)
	return out
}
