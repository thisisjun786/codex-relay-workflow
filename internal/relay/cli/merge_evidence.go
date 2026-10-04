package cli

import (
	"context"
	"maps"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// forgeRunner is the process runner merge-evidence reads the forge through; tests replace it.
var forgeRunner = evidence.ExecRunner

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
	lateSource, lateGiven := args.TruthyString("late-dispositions")
	if _, restating := args.TruthyString("restate"); lateGiven && !restating {
		return nil, &dispatch.UsageError{Detail: "--late-dispositions grades a restated record; pass --restate too", Code: contract.ExitUsage}
	}
	expectReview := args.Bool("expect-independent-review")
	if _, restating := args.TruthyString("restate"); expectReview && !restating {
		return nil, &dispatch.UsageError{Detail: "--expect-independent-review grades a restated record; pass --restate too", Code: contract.ExitUsage}
	}
	// A file the command cannot use is refused before anything is read from the forge.
	var lateDocument any
	if lateGiven {
		late, readErr := readLateDispositions(lateSource)
		if readErr != nil {
			return nil, readErr
		}
		lateDocument = late
	}
	forge := evidence.NewForge(forgeRunner(ctx))
	forge.PageSize = args.Integer("page-size")
	forge.PageBudget = args.Integer("page-budget")
	forge.CallBudget = args.Integer("call-budget")
	seconds := args.Integer("timeout")
	forge.TimeoutSeconds = strconv.FormatInt(seconds, 10)
	forge.Timeout = subprocessTimeout(seconds)
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
		problems, lateResults := evidence.RestateWithDispositions(pyvalue.Str(head), handoff, snapshot, lateDocument)
		items := make([]any, len(problems))
		for i, p := range problems {
			items[i] = map[string]any{"code": p.Code, "detail": p.Detail}
		}
		restatement := map[string]any{"headSha": head, "current": len(problems) == 0, "problems": items}
		if lateGiven {
			restatement["lateDispositions"] = lateResults
		}
		snapshot["restatement"] = restatement
		// The child's statement about its independent review is read beside the restatement and never
		// in it: whatever it finds, the problems above, current, the verdict and the exit code stand.
		if coverage := evidence.IndependentReviewCoverage(pyvalue.Str(head), handoff, nil); coverage.Stated || expectReview {
			warnings := make([]any, len(coverage.Warnings))
			for i, w := range coverage.Warnings {
				warnings[i] = map[string]any{"code": w.Code, "detail": w.Detail}
			}
			snapshot[evidence.IndependentReviewMember] = map[string]any{"stated": coverage.Stated, "warnings": warnings}
		}
		ready = ready && len(problems) == 0
	}
	payload := sortedObject(answerValue(snapshot).(map[string]any))
	if !ready {
		return nil, &dispatch.PayloadExit{Payload: payload, Code: contract.ExitRefused}
	}
	return payload, nil
}}

// sortedObject is m as an ordered object with its keys in sorted order, which is how Emit prints a
// map; a refusal's payload has to be an ordered object.
func sortedObject(m map[string]any) contract.OrderedObject {
	out := make(contract.OrderedObject, 0, len(m))
	for _, key := range slices.Sorted(maps.Keys(m)) {
		out = append(out, contract.Field{Key: key, Value: m[key]})
	}
	return out
}

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

// readLateDispositions reads the coordinator's dispositions document from a file path. Unlike
// --restate it has no stdin form: '-' would collide with a restated record read from stdin.
func readLateDispositions(path string) (contract.OrderedObject, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &dispatch.UsageError{Detail: "the late-thread dispositions could not be read: " + err.Error(), Code: contract.ExitUsage}
	}
	document, err := decodeInput(raw)
	if err != nil {
		return nil, &dispatch.UsageError{Detail: "the late-thread dispositions are not JSON: " + err.Error(), Code: contract.ExitUsage}
	}
	out, ok := document.(contract.OrderedObject)
	if !ok {
		return nil, &dispatch.UsageError{Detail: "the late-thread dispositions must be a JSON object holding " + evidence.LateDispositionsMember + ", not " + jsonKind(document), Code: contract.ExitUsage}
	}
	return out, nil
}
