package dagsched

import (
	"context"
	"fmt"
	"math/big"
	"os/exec"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// ExecRunner is the forge process runner of production: it runs the argv it is given and reports the exit code and both streams. It is the one cli.forgeRunner is, copied so
// this package does not import package cli.
func ExecRunner(ctx context.Context) evidence.Runner {
	return func(argv []string, timeout time.Duration) (int, string, string, error) {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
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

// ForgePullRequestReader reads a pull request the way merge-evidence does (evidence.Collect over the runner newRunner makes) and projects the snapshot. The collector
// reports unreadable, truncated or moved evidence as snapshot problems with a nil error, so the caller classifies the answer by its Verdict (ClassifyPullRequest).
func ForgePullRequestReader(newRunner func(context.Context) evidence.Runner) PullRequestReader {
	return func(ctx context.Context, repository string, number int64) (PullRequest, error) {
		if _, _, err := evidence.SplitRepository(repository); err != nil {
			return PullRequest{}, err
		}
		snapshot, err := evidence.Collect(evidence.NewForge(newRunner(ctx)), repository, big.NewInt(number))
		if err != nil {
			return PullRequest{}, err
		}
		return projectSnapshot(snapshot, repository, number), nil
	}
}

func textOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func mapField(m map[string]any, key string) map[string]any {
	out, _ := m[key].(map[string]any)
	return out
}

// projectSnapshot is the part of a merge-evidence snapshot the scheduler reads: the pull request the relay itself saw (head, base, draft flag, state), the checks of
// that head, the required-check declaration, the relay's own verdict and its problem codes.
func projectSnapshot(snapshot map[string]any, repository string, number int64) PullRequest {
	pinned, handoff := mapField(snapshot, "pinned"), mapField(snapshot, "handoff")
	pr := PullRequest{Repository: repository, Number: number, State: textOf(pinned["state"]), HeadSHA: textOf(pinned["headSha"]), BaseRef: textOf(pinned["baseRef"]),
		BaseSHA: textOf(pinned["baseSha"]), Verdict: textOf(snapshot["verdict"])}
	pr.IsDraft, _ = pinned["isDraft"].(bool)
	if merged, _ := pinned["merged"].(bool); merged {
		pr.State = "merged"
	}
	checkItems, _ := handoff["checks"].([]any)
	for _, item := range checkItems {
		c, _ := item.(map[string]any)
		attempt := int64(1)
		if c["attempt"] != nil {
			attempt = evidence.Integer(c["attempt"]).Int64()
		}
		pr.Checks = append(pr.Checks, Check{RunID: textOf(c["runId"]), Name: textOf(c["name"]), HeadSHA: textOf(c["headSha"]), Conclusion: textOf(c["conclusion"]), Provider: textOf(c["provider"]), Attempt: attempt})
	}
	// the collector holds the declared list as []string when it read it and nil when it could not: nil is "not declared", an empty list is "requires none".
	switch declared := handoff["requiredDeclared"].(type) {
	case []string:
		pr.RequiredReadable, pr.RequiredDeclared = true, append([]string{}, declared...)
	case []any:
		pr.RequiredReadable, pr.RequiredDeclared = true, []string{}
		for _, name := range declared {
			pr.RequiredDeclared = append(pr.RequiredDeclared, textOf(name))
		}
	}
	pr.RequiredProviders = evidence.ProviderMap(handoff["requiredProviders"])
	problems, _ := snapshot["problems"].([]any)
	for _, item := range problems {
		p, _ := item.(map[string]any)
		pr.Problems = append(pr.Problems, Problem{Code: textOf(p["code"]), Detail: textOf(p["detail"])})
	}
	var required []string
	if pr.RequiredReadable {
		required = pr.RequiredDeclared
	}
	var rows []any
	for _, c := range pr.Checks {
		rows = append(rows, map[string]any{"runId": c.RunID, "name": c.Name, "headSha": c.HeadSHA, "conclusion": c.Conclusion, "attempt": c.Attempt, "provider": optionalProvider(c.Provider)})
	}
	for _, p := range evidence.ChecksProblemsWith(pr.HeadSHA, required, rows, true, pr.RequiredProviders) {
		pr.CheckProblems = append(pr.CheckProblems, p.Code+": "+p.Detail)
	}
	pr.ReviewDigest = reviewDigest(snapshot["findings"])
	return pr
}

// reviewDigest digests the review findings of a snapshot, so evidence that differs only in what reviewers said still differs.
func reviewDigest(findings any) (digest string) {
	defer func() {
		if recover() != nil {
			digest = ""
		}
	}()
	return digestOf(findings)
}

// ClassifyPullRequest is the fail-closed rule every consumer of a snapshot applies (010): a verdict of unknown means the evidence was unreadable, truncated or unstable and is
// the host's failure, whatever the checks it did read say; stale means the candidate or its gates moved while it was read and is refused with the existing reasons; not_ready
// is a judgement (a failing check, an open thread) and is not a read failure.
func ClassifyPullRequest(pr PullRequest) error {
	codes := make([]string, 0, len(pr.Problems))
	moved, malformed := false, false
	for _, p := range pr.Problems {
		codes = append(codes, p.Code)
		moved = moved || p.Code == evidence.CandidateMoved
		malformed = malformed || p.Code == evidence.GatesMoved || p.Code == evidence.BaseRefMissing
	}
	switch pr.Verdict {
	case evidence.UnknownVerdict:
		return fmt.Errorf("the pull request %s#%d could not be read completely (%s); nothing was written", pr.Repository, pr.Number, strings.Join(codes, ", "))
	case evidence.Stale:
		if moved {
			return refuseCandidateMoved("pull request %s#%d moved while it was read (%s)", pr.Repository, pr.Number, strings.Join(codes, ", "))
		}
		if malformed {
			return refuseEvidenceMalformed("the gates of %s#%d moved or its base branch is missing (%s)", pr.Repository, pr.Number, strings.Join(codes, ", "))
		}
	case evidence.Ready, evidence.NotReady:
		return nil
	}
	return fmt.Errorf("the pull request %s#%d has a verdict the scheduler does not know, %q", pr.Repository, pr.Number, pr.Verdict)
}

// optionalProvider is a check's provider as the collector gives it: absent is nil, not an empty text.
func optionalProvider(p string) any {
	if p == "" {
		return nil
	}
	return p
}
