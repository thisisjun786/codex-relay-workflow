package cli

import (
	"context"
	"flag"
	"math"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var forgeRunner = func(ctx context.Context) evidence.Runner {
	return func(argv []string, timeout time.Duration) (int, string, string, error) {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if runCtx.Err() != nil {
			return 0, "", "", runCtx.Err()
		}
		if err == nil {
			return 0, stdout.String(), stderr.String(), nil
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return 0, stdout.String(), stderr.String(), err
	}
}

var mergeEvidenceCommand = Command{Name: "merge-evidence", Exempt: true, Flags: func(f *flag.FlagSet) {
	f.String("repository", "", "")
	f.Int("pull-request", 0, "")
	f.String("restate", "", "")
	f.String("restate-head", "", "")
	f.Int("page-size", 100, "")
	f.Int("page-budget", 50, "")
	f.Int("call-budget", 300, "")
	f.Int("timeout", 60, "")
}, Run: func(ctx context.Context, _ Services, args Args) (any, error) {
	repository, _ := args.String("repository")
	if _, _, err := evidence.SplitRepository(repository); err != nil {
		return nil, &UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	numberValue := args.Integer("pull-request")
	number, err := evidence.PullRequestNumber(numberValue)
	if err != nil {
		return nil, &UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	forge := evidence.NewForge(forgeRunner(ctx))
	forge.PageSize = args.Integer("page-size")
	forge.PageBudget = args.Integer("page-budget")
	forge.CallBudget = args.Integer("call-budget")
	seconds := args.Integer("timeout")
	forge.TimeoutSeconds = seconds.String()
	forge.Timeout, err = subprocessTimeout(seconds)
	if err != nil {
		failure := err
		forge.Run = func(argv []string, _ time.Duration) (int, string, string, error) {
			// subprocess spawns before converting its timeout. Preserve missing-gh
			// precedence, and keep a spent call budget from evaluating the timeout.
			if _, err := exec.LookPath(argv[0]); err != nil {
				return 0, "", "", &HostError{Class: "FileNotFoundError", Detail: "[Errno 2] No such file or directory: 'gh'"}
			}
			return 0, "", "", failure
		}
	}
	snapshot, err := evidence.Collect(forge, repository, number)
	if err != nil {
		if host, ok := err.(*HostError); ok {
			return nil, host
		}
		if host, ok := err.(*evidence.PythonError); ok {
			return nil, &HostError{Class: host.Class, Detail: host.Detail}
		}
		if _, lookupErr := exec.LookPath("gh"); lookupErr != nil {
			return nil, &HostError{Class: "FileNotFoundError", Detail: "[Errno 2] No such file or directory: 'gh'"}
		}
		return nil, &HostError{Class: "RuntimeError", Detail: err.Error()}
	}
	ready := snapshot["verdict"] == evidence.Ready
	if source, given := args.TruthyString("restate"); given {
		document, readErr := readRestatement(source)
		if readErr != nil {
			return nil, readErr
		}
		headText, _ := args.String("restate-head")
		var head any = headText
		if !pyvalue.Truthy(head) {
			head = get(document, "headSha")
		}
		if !pyvalue.Truthy(head) {
			head = get(get(document, "pinned"), "headSha")
		}
		if !pyvalue.Truthy(head) {
			return nil, &UsageError{Detail: "the record does not say which head it is about; pass --restate-head", Code: contract.ExitUsage}
		}
		handoff := document
		if h, ok := get(document, "handoff").(contract.OrderedObject); ok && len(h) > 0 {
			handoff = h
		}
		problems := evidence.RestateProblems(pyvalue.Str(head), handoff, snapshot)
		items := make([]any, len(problems))
		for i, p := range problems {
			items[i] = map[string]any{"code": p.Code, "detail": p.Detail}
		}
		snapshot["restatement"] = map[string]any{"headSha": head, "current": len(problems) == 0, "problems": items}
		ready = ready && len(problems) == 0
	}
	payload := supervisorOrdered(snapshot).(contract.OrderedObject)
	if !ready {
		return nil, &PayloadExit{Payload: payload, Code: contract.ExitRefused}
	}
	return payload, nil
}}

// subprocessTimeout follows Linux CPython's communicate -> PollSelector -> poll
// conversion chain: float seconds, ceil milliseconds, signed PyTime_t, then C int.
// Negative finite seconds time out before reaching poll, but still convert to float.
func subprocessTimeout(seconds *big.Int) (time.Duration, error) {
	fail := func(detail string) (time.Duration, error) {
		return 0, &HostError{Class: "OverflowError", Detail: detail}
	}
	value, _ := new(big.Float).SetInt(seconds).Float64()
	if math.IsInf(value, 0) {
		return fail("int too large to convert to float")
	}
	if value <= 0 {
		return 0, nil
	}
	milliseconds := math.Ceil(value * 1e3)
	if math.IsInf(milliseconds, 0) {
		return fail("cannot convert float infinity to integer")
	}
	// poll converts its integer milliseconds to nanoseconds before narrowing to int32.
	if milliseconds >= math.Exp2(63)/1e6 {
		return fail("timestamp too large to convert to C PyTime_t")
	}
	if milliseconds > math.MaxInt32 {
		return fail("timeout is too large")
	}
	return time.Duration(value * float64(time.Second)), nil
}

func readRestatement(source string) (contract.OrderedObject, error) {
	var raw []byte
	var err error
	if source == "-" {
		raw, err = os.ReadFile("/dev/stdin")
	} else {
		raw, err = os.ReadFile(source)
	}
	if err != nil {
		return nil, &UsageError{Detail: "the record to restate could not be read: " + store.PythonOSErrorText(err), Code: contract.ExitUsage}
	}
	text, err := store.DecodeUTF8(raw)
	if err != nil {
		return nil, &UsageError{Detail: "the record to restate could not be read: " + err.Error(), Code: contract.ExitUsage}
	}
	// Path.read_text uses universal newlines; sys.stdin retains CR bytes.
	if source != "-" {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	}
	// CPython's C scanner allows 9998 nested containers at this CLI boundary
	// (the outer record plus 9997 nested values), independently of Python frames.
	if message, recursion := store.PythonJSONErrorWithLimit(text, 9998); message != "" {
		if recursion {
			return nil, &HostError{Class: "RecursionError", Detail: message}
		}
		return nil, &UsageError{Detail: "the record to restate is not JSON: " + message, Code: contract.ExitUsage}
	}
	document, err := decodeJSON([]byte(text))
	if err != nil {
		return nil, &UsageError{Detail: "the record to restate is not JSON: " + err.Error(), Code: contract.ExitUsage}
	}
	out, ok := document.(contract.OrderedObject)
	if !ok {
		return nil, &UsageError{Detail: "the record to restate is an object, not a " + pyvalue.TypeName(document), Code: contract.ExitUsage}
	}
	return out, nil
}
