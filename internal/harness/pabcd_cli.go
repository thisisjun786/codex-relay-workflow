package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
)

// The adapters preserve cli.ts's stream choice, trailing newline and exit code.
func planVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed, err := cli.ParsePlanCliArgs(args, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "plan: "+err.Error())
		return 1
	}
	result := cli.RunPlanCli(parsed)
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

func receiptVerb(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed, err := cli.ParseReceiptCLIArgs(args, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "receipt: "+err.Error())
		return 1
	}
	result, err := cli.RunReceiptCLI(parsed, cli.ReceiptRunOptions{Context: ctx, Stdin: in, Stdout: stdout, Stderr: stderr})
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

func evidenceVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed, err := cli.ParseEvidenceCLIArgs(args, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "evidence: "+err.Error())
		return 1
	}
	output, code := cli.RunEvidenceCLI(parsed)
	fmt.Fprintln(stdout, output)
	return code
}

// memoryVerb is the memory row. It takes the invocation's context (CRW-1074): the oracle's process dies at the first
// SIGINT and records nothing, so an allow-write whose session lock wait ends with that context, or which finds it
// ended with the lock held and before the grant is written, answers Interrupted (130) with nothing printed. Once the
// write has started the command finishes and prints its own answer. The help and parse answers read nothing and are
// printed under any context.
func memoryVerb(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, cli.MemoryUsage)
		return 0
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed := cli.ParseMemoryCLIArgs(args, cwd)
	if parsed.Help {
		fmt.Fprintln(stdout, cli.MemoryUsage)
		return 0
	}
	if parsed.Error != "" {
		fmt.Fprintln(stderr, "memory: "+parsed.Error+"\n"+cli.MemoryUsage)
		return 2
	}
	output, code, err := cli.RunMemoryCLIContext(ctx, *parsed.Args)
	if err != nil {
		return Interrupted
	}
	stream := stdout
	if code != 0 {
		stream = stderr
	}
	fmt.Fprintln(stream, output)
	return code
}

// configInterviewUsage is the oracle's usage text (pabcd-state/src/cli.ts:224-228) with the CRW
// names substituted; the bracket lists the policy owner's values in its own order, so the text
// cannot drift from the policies the writer accepts.
const configInterviewUsage = "Usage:\n  crw pabcd config interview [" +
	string(projectcfg.PolicyOff) + "|" + string(projectcfg.PolicyNewUnit) + "|" + string(projectcfg.PolicyAlways) + "]\n\n" +
	"  off       only an explicit 'interview' / '인터뷰' request opens the Interview\n" +
	"  new-unit  also open it on the first plan request of a new unit (default)\n" +
	"  always    open it on every plan request\n\n" +
	"The Interview is advisory: it never changes the PABCD phase, and goal mode always suppresses it.\n"

// configVerb is the config row. Only the interview subcommand belongs to this component; every
// other subcommand gets the oracle's refusal (cli.ts:229-232). The policy write goes through the
// projectcfg owner, which is also what the hooks read, so the CLI cannot drift from it
// (cli.ts:242-252). The wrong-subcommand and unknown-policy answers read no cwd, as the oracle's
// checks precede its process.cwd() calls; the help and write paths resolve it at their own point of
// use (cli.ts:234, :242).
func configVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "interview" {
		fmt.Fprint(stderr, "config: this component handles 'config interview' only\n"+configInterviewUsage)
		return 2
	}
	if len(args) < 2 || args[1] == "--help" || args[1] == "-h" {
		cwd, err := syscall.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
			return 1
		}
		fmt.Fprint(stdout, configInterviewUsage+"current: "+string(projectcfg.ReadPolicy(cwd))+"\n")
		return 0
	}
	value := args[1]
	if !projectcfg.IsPolicy(value) {
		fmt.Fprintf(stderr, "config interview: unknown policy '%s'\n%s", value, configInterviewUsage)
		return 2
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	result, err := projectcfg.WritePolicy(cwd, projectcfg.Policy(value))
	if err != nil {
		fmt.Fprintln(stderr, "config interview: "+err.Error())
		return 1
	}
	message := "interview policy: " + value + " (" + result.Path + ")"
	if result.ReplacedMalformed {
		message += " — the previous file was not valid JSON and was replaced"
	}
	fmt.Fprintln(stdout, message)
	return 0
}

// scanVerb is the scan row: the parser and the runner this repository already ported, driven
// exactly as the oracle's scan branch does (cli.ts:325-336). A parse error keeps the "scan: "
// prefix on stderr and exit 1; every other outcome goes to stdout with the runner's own exit code.
//
// The row takes the invocation's context (CRW-1074): a record whose session lock wait ends with it, or which finds it
// ended with the lock held and before the scan_completed row is appended, answers Interrupted (130) with nothing
// printed and nothing written, as the oracle's process dies at the signal. Once the append has started the command
// finishes and prints its own answer.
func scanVerb(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed := cli.ParseScanCliArgs(args, cwd)
	if parsed.Error != "" {
		fmt.Fprintln(stderr, "scan: "+parsed.Error)
		return 1
	}
	result, err := cli.RunScanCliContext(ctx, *parsed.Args)
	if err != nil {
		return Interrupted
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

// reviewRoundVerb is the review-round row: the parser and the runner this repository already
// ported, driven exactly as the oracle's branch does (cli.ts:310-330). A parse refusal keeps the
// "review-round: " prefix on stderr and exit 1; a library error is the oracle's uncaught throw and
// answers with the generic "crw cli failed: " prefix; every result goes to stdout with its code.
func reviewRoundVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed := cli.ParseReviewRoundCliArgs(args, cwd)
	if parsed.Error != "" {
		fmt.Fprintln(stderr, "review-round: "+parsed.Error)
		return 1
	}
	result, err := cli.RunReviewRoundCli(*parsed.Args, nil)
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

// metricVerb is the metric row (cli.ts:156-181). The oracle reads stdin only when the first
// argument after the verb is exactly "ingest", and before it resolves the working directory, so an
// oversized ingest in a deleted cwd still reports the overflow. The bound and the overflow rule
// are the repository's ReadStdin, the same one the hook envelope uses; every result goes to stdout
// with its code, and a library error answers "crw cli failed: ".
//
// The oracle's process dies on the first SIGINT and records nothing, so the ingest read runs as the
// hook leg's interrupted read does (Hook): in a goroutine, under the invocation's context. An
// interrupt that lands while the read still waits ends the row with Interrupted (130), with nothing
// written and nothing recorded; one that lands as the read finishes meets the context check below
// and gets the same answer, so a line that arrived just before the signal is not recorded either.
//
// The record window that follows is under the same context (CRW-627): the library ends its lock wait and its loop over the METRIC
// lines with it, and an ingest whose run ends by cancellation, or after which the context has ended, answers Interrupted with nothing
// printed too. The rows recorded before the signal stay, as the lines the oracle's process had appended do when it dies at the
// signal. Since CRW-632 the record and kind writers take the same context, so a run of theirs that ends by cancellation, or after
// which the context has ended, answers Interrupted the same way with nothing printed and nothing written; the reading verbs (show,
// parse-line, help) print their answer even under an ended context, which TestPabcdMetricRowsUnchangedWithoutAnInterrupt pins. Every
// uninterrupted run answers as before.
func metricVerb(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer) int {
	raw := ""
	ingest := len(args) > 0 && args[0] == "ingest"
	writer := metricWrites(args)
	if ingest {
		type stdinRead struct {
			raw      string
			overflow bool
		}
		done := make(chan stdinRead, 1)
		go func() {
			raw, overflow := ReadStdin(in)
			done <- stdinRead{raw: raw, overflow: overflow}
		}()
		var stdin stdinRead
		select {
		case stdin = <-done:
		case <-ctx.Done():
			return Interrupted
		}
		if ctx.Err() != nil {
			return Interrupted
		}
		if stdin.overflow {
			fmt.Fprintf(stderr, "metric: stdin exceeds %d bytes\n", MaxStdinBytes)
			return 1
		}
		raw = stdin.raw
	}
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	result, err := cli.RunMetricCLIContext(ctx, args, cwd, raw)
	if writer && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
		return Interrupted
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

// metricWrites reports whether the metric row's arguments can write the ledger or the kind file, which are the runs the
// invocation's context can cut short (CRW-632). metric kind writes only when it names a kind (cli.MetricKindRequested);
// without one it reads, as metric show does, and a reading run still prints its answer under an ended context.
func metricWrites(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "ingest", "record":
		return true
	case "kind":
		return cli.MetricKindRequested(args) != nil
	}
	return false
}

// loopVerb is the loop row (cli.ts:169-180): the parser and the runner this repository ports, driven exactly
// as the oracle's branch does. The oracle labels a parse refusal with the kind ("loop: "), which is why an
// unknown flag reads "loop: init: unknown flag '--help'"; a library error is the oracle's uncaught throw and
// answers the generic "crw cli failed: " prefix; every result goes to stdout with its own code.
//
// The row takes the invocation's context (CRW-1074): a steer whose batch read or goalplan lock wait ends with it, or which
// finds it ended with the lock held and before the transaction's first write, answers Interrupted (130) with nothing
// printed and nothing written, as the oracle's process dies at the signal. The other verbs do not read it.
func loopVerb(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed, err := cli.ParseLoopCliArgs(args, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "loop: "+err.Error())
		return 1
	}
	result, err := cli.RunLoopCliContext(ctx, parsed)
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return Interrupted
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

// divergenceVerb is the divergence row (cli.ts:183-189). The library's error is the oracle's one
// uncaught path (the mode write), reported as "crw cli failed: "; every result goes to stdout with
// its code. It takes the invocation's context like the metric row does (CRW-632): a writing run
// (mode on/off, candidate add) whose lock wait ends with the context, or whose context has ended
// after it, answers Interrupted (130) with nothing printed, as the oracle's process dies at the
// signal and prints nothing; the reading paths (mode with no on/off, candidate list) and the help
// token print their answer even under an ended context.
func divergenceVerb(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	result, err := cli.RunDivergenceCliContext(ctx, args, cwd)
	if divergenceWrites(args) && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
		return Interrupted
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}

// divergenceWrites reports whether the divergence row's arguments name one of the two writing verbs, which are the runs the
// invocation's context can cut short (CRW-632). It mirrors the library's own topic/verb split (cli/divergence.go:220-230).
func divergenceWrites(args []string) bool {
	topic, verb := "", ""
	if len(args) > 0 {
		topic = args[0]
	}
	if len(args) > 1 {
		verb = args[1]
	}
	return topic == "mode" && (verb == "on" || verb == "off") || topic == "candidate" && verb == "add"
}

// orchestrateVerb is the orchestrate row (cli.ts:143-152). The terminal entry is the only caller
// that supplies the native environment: the oracle hands runOrchestrateCli its process.env, which is
// what drives the implicit CODEX_THREAD_ID status selection and the homedir scan, while every other
// row keeps the library's empty env. The parse refusal is the one answer the oracle writes to
// STDERR (cli.ts:146-149) and it never reaches runOrchestrateCli, so it is rendered here instead of
// letting RunOrchestrateRead answer it on stdout. Everything else is the library's own stream, code
// and trailing newline, and a delegated mutation is RunOrchestrateTransition's answer the same way.
// The row takes the invocation's context (CRW-871): the oracle's process dies at the first SIGINT and
// records nothing, so a mutation whose lock wait or pre-write check ends with that context answers
// Interrupted (130) with nothing printed. Once the first write has started the command finishes and its
// own answer is printed, because a published change is never relabelled as interrupted.
func orchestrateVerb(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	parsed := cli.ParseOrchestrateCliArgs(args, cwd)
	if parsed.Error != nil {
		fmt.Fprintln(stderr, cli.RenderOrchestrateParseError(*parsed.Error))
		return 1
	}
	env := host.LookupEnv(os.LookupEnv)
	read, err := cli.RunOrchestrateRead(parsed, cli.ReadEnv{Native: env, Process: env})
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	if read.Result != nil {
		fmt.Fprintln(stdout, read.Result.Output)
		return read.Result.Code
	}
	result, err := cli.RunOrchestrateTransitionContext(ctx, *parsed.Args, read.SessionID)
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		// The mutation chose to cancel before its first write; a completed one returned a CliResult and
		// must print it, even if the context ended after the write started.
		return Interrupted
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}
