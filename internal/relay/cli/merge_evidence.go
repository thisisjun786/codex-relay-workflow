package cli

import (
	"context"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
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

var mergeEvidenceCommand = dispatch.Command{Name: "merge-evidence", Unselected: true, Exempt: true, ReadOnly: true, Defaults: map[string]any{"page-size": int64(100), "page-budget": int64(50), "call-budget": int64(300), "timeout": int64(60)}, Run: func(ctx context.Context, _ dispatch.Services, args dispatch.Args) (any, error) {
	repository, _ := args.String("repository")
	if _, _, err := evidence.SplitRepository(repository); err != nil {
		return nil, &dispatch.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	numberValue := args.Integer("pull-request")
	number, err := evidence.PullRequestNumber(numberValue)
	if err != nil {
		return nil, &dispatch.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	forge := evidence.NewForge(forgeRunner(ctx))
	forge.PageSize = args.Integer("page-size")
	forge.PageBudget = args.Integer("page-budget")
	forge.CallBudget = args.Integer("call-budget")
	seconds := args.Integer("timeout")
	forge.TimeoutSeconds = seconds.String()
	forge.Timeout = subprocessTimeout(seconds.Int64())
	snapshot, err := evidence.Collect(forge, repository, number)
	if err != nil {
		if _, lookupErr := exec.LookPath("gh"); lookupErr != nil {
			return nil, dispatch.Host(lookupErr.Error())
		}
		return nil, dispatch.Host(err.Error())
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
			return nil, &dispatch.UsageError{Detail: "the record does not say which head it is about; pass --restate-head", Code: contract.ExitUsage}
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
		return nil, &dispatch.PayloadExit{Payload: payload, Code: contract.ExitRefused}
	}
	return payload, nil
}}

// subprocessTimeout is how long one gh call may take: --timeout seconds, none at all for zero or
// less (the call times out at once), and the longest a time.Duration holds for more than that.
func subprocessTimeout(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return math.MaxInt64
	}
	return time.Duration(seconds) * time.Second
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
		return nil, &dispatch.UsageError{Detail: "the record to restate could not be read: " + err.Error(), Code: contract.ExitUsage}
	}
	document, err := decodeInput(raw)
	if err != nil {
		return nil, &dispatch.UsageError{Detail: "the record to restate is not JSON: " + err.Error(), Code: contract.ExitUsage}
	}
	out, ok := document.(contract.OrderedObject)
	if !ok {
		return nil, &dispatch.UsageError{Detail: "the record to restate must be a JSON object, not " + jsonKind(document), Code: contract.ExitUsage}
	}
	return out, nil
}
