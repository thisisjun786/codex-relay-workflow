package harness

import (
	"fmt"
	"io"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
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

func receiptVerb(args []string, in io.Reader, stdout, stderr io.Writer) int {
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
	result, err := cli.RunReceiptCLI(parsed, cli.ReceiptRunOptions{Stdin: in, Stdout: stdout, Stderr: stderr})
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
