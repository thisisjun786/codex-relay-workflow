package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
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

func memoryVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
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
	output, code := cli.RunMemoryCLI(*parsed.Args)
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
func scanVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
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
	result := cli.RunScanCli(*parsed.Args)
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
// signal. Every other metric row and every uninterrupted run answers as before.
func metricVerb(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer) int {
	raw := ""
	ingest := len(args) > 0 && args[0] == "ingest"
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
	if ingest && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
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
// its code.
func divergenceVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	cwd, err := syscall.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	result, err := cli.RunDivergenceCli(args, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, result.Output)
	return result.Code
}
