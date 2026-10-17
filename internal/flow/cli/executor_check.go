package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

const ExecutorResultCheckUsage = `crw executor result check --execution ID --actor TASK --epoch N --spool LOCAL_DIR
  [--result result.json] [--collection-root LOCAL_DIR]
  [--verification FILE] [--premerge FILE] [--state DIR] [--socket PATH]
Reads controller assignments and bounded local delivery files, then fetches the assigned
GitHub branch. Reports envelope_valid, candidate_valid, candidate_accepted and required_gates.
Exit 0 means the envelope/candidate checks succeeded; it never means candidate acceptance.
Exit 1 means invalid/incomplete evidence; exit 3 means retryable collection failure.`

type executorCheckCommand func(context.Context, string, string, int64, ops.ExecutorResultCheck) (executor.ResultCheckReport, error)

func executorResultCheck(ctx context.Context, args []string, out, errout io.Writer) int {
	return runExecutorResultCheck(ctx, args, out, errout, func(ctx context.Context, state, socket string, epoch int64, in ops.ExecutorResultCheck) (executor.ResultCheckReport, error) {
		reader, err := OpenReader(ctx, state, socket)
		if err != nil {
			return executor.ResultCheckReport{}, err
		}
		defer reader.Close()
		return ops.CheckExecutorResult(ctx, &dagsched.Scheduler{Store: reader.St, ExpectedEpoch: epoch}, in)
	})
}
func readCheckEvidence(file string) ([]byte, error) {
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > executor.DefaultLimits.EnvelopeBytes {
		return nil, errors.New("gate evidence must be a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, executor.DefaultLimits.EnvelopeBytes+1))
	if int64(len(b)) > executor.DefaultLimits.EnvelopeBytes {
		return nil, errors.New("gate evidence exceeds limit")
	}
	return b, err
}
func runExecutorResultCheck(ctx context.Context, args []string, out, errout io.Writer, check executorCheckCommand) int {
	fs := flag.NewFlagSet("executor result check", flag.ContinueOnError)
	fs.SetOutput(errout)
	fs.Usage = func() { fmt.Fprintln(errout, ExecutorResultCheckUsage) }
	var in ops.ExecutorResultCheck
	fs.StringVar(&in.Execution, "execution", "", "registered execution ID")
	fs.StringVar(&in.Actor, "actor", "", "current plan owner")
	fs.StringVar(&in.Spool, "spool", "", "local received private spool (not a worker path)")
	fs.StringVar(&in.ResultPath, "result", "result.json", "relative result file")
	fs.StringVar(&in.CollectionRoot, "collection-root", "", "local directory for a new isolated checkout (default TMPDIR)")
	state := fs.String("state", "", "controller state")
	socket := fs.String("socket", "", "controller socket identity")
	epoch := fs.Int64("epoch", 0, "current coordinator epoch")
	verification := fs.String("verification", "", "controller verification record file")
	premerge := fs.String("premerge", "", "controller pre-merge record file")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if fs.NArg() != 0 || in.Execution == "" || in.Actor == "" || *epoch < 1 || !filepath.IsAbs(in.Spool) || in.CollectionRoot != "" && !filepath.IsAbs(in.CollectionRoot) {
		fs.Usage()
		return 2
	}
	var err error
	if in.Verification, err = readCheckEvidence(*verification); err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	if in.Premerge, err = readCheckEvidence(*premerge); err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	report, err := check(ctx, *state, *socket, *epoch, in)
	if code := hostAnswer(out, errout, report, err); code != 0 {
		return code
	}
	if report.CollectionStatus == "retryable" {
		return 3
	}
	if !report.EnvelopeValid || !report.CandidateValid {
		return 1
	}
	return 0
}
