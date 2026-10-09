package job

import (
	"errors"
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

func cliFormatList(recs []BgRecord) string {
	if len(recs) == 0 {
		return "백그라운드 작업 없음"
	}
	lines := make([]string, len(recs))
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
	return strings.Join(lines, "\n")
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
		result.Out = cliFormatList(recs)
		if err == nil && asJSON {
			items := make([]any, 0, len(recs))
			for _, rec := range recs {
				var item any
				if item, err = cliRecord(rec); err != nil {
					break
				}
				items = append(items, item)
			}
			result.Out = items
		}
	case "get", "cancel":
		rec, ok := ReadRecord(cwd, id)
		if !ok {
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
			body, _ := ReadText(OutPath(cwd, id))
			lines := text.SplitLinesByteExact(body)
			tail := cliTail(opts.Tail, len(lines))
			shown := ""
			if tail > 0 {
				shown = strings.Join(lines[len(lines)-tail:], "\n")
			}
			result.Out = strings.TrimRightFunc(DescribeRecord(rec)+"\n\n"+shown, func(r rune) bool { return text.Trim(string(r)) == "" })
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
			flag = "  파일 플래그: off (" + since + ")"
		}
		var recs []BgRecord
		recs, err = ListRecords(cwd, clock)
		result.Out = strings.Join([]string{"wake: " + wake, flag, "  " + EnvVar + ": " + env, "  작업 " + strconv.Itoa(len(recs)) + "건"}, "\n")
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
	if err := AtomicWrite(cwd, path, clock().UTC().Format(isoLayout)+"\n"); err != nil {
		return "", err
	}
	if verb == "on" {
		if err := RemovePath(cwd, DisabledPath(cwd)); err != nil {
			return "", err
		}
	}
	_ = appendLedger(cwd, Event{{"event", event}}, clock)
	return out, nil
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
// the relay's answer, which the dispatcher writes after it returns: the stamp still comes before that write.
func cliDrain(cwd string, session *string, clock func() time.Time) string {
	out, _ := deliver(cwd, session, clock, func(recs []BgRecord) string {
		lines := []string{"[crw bg] 백그라운드 작업 " + strconv.Itoa(len(recs)) + "건이 끝났습니다."}
		for _, rec := range recs {
			lines = append(lines, DescribeRecord(rec))
		}
		lines = append(lines, "출력은 `crw relay job get <id> --tail 40`으로 봅니다. 전체 목록은 `crw relay job list`.\n결과를 확인하고 필요한 후속 작업을 이어가세요.")
		return strings.Join(lines, "\n")
	}, acceptAll)
	return out
}
