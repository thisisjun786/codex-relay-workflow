package harness

import (
	"context"
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
