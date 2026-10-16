package lint

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

const lintUsage = `usage: crw flow lint [--args JSON | --args-file FILE] [--no-dry-run] [--json] <script.js>...`

// Command is the first word of the command this package serves.
const Command = "lint"

// RunCLI is `crw flow`: `lint` here, every other command in the engine. It exits 0 when every script has no failing finding
// (same-thread review warnings are reported), 1 when a script has another finding or does not load, and 2 for a command line it cannot use.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == Command {
		return lintCmd(ctx, args[1:], stdout, stderr)
	}
	return engine.RunCLI(ctx, args, stdout, stderr)
}

func lintCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs_ := flag.NewFlagSet("crw flow lint", flag.ContinueOnError)
	fs_.SetOutput(stderr)
	argsJSON := fs_.String("args", "", "the script's args for the dry run, as JSON")
	argsFile := fs_.String("args-file", "", "a file holding the script's args as JSON")
	noDry := fs_.Bool("no-dry-run", false, "check the text only; do not run the script on the fake backend")
	asJSON := fs_.Bool("json", false, "print the reports as JSON")
	fs_.Usage = func() { fmt.Fprintln(stderr, lintUsage) }
	var scripts []string
	rest := args
	for {
		if err := fs_.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs_.NArg() == 0 {
			break
		}
		scripts = append(scripts, fs_.Arg(0))
		rest = fs_.Args()[1:]
	}
	if len(scripts) == 0 {
		fmt.Fprintln(stderr, lintUsage)
		fmt.Fprintln(stderr, "crw flow lint: error: at least one script file is required")
		return 2
	}
	var argsRaw json.RawMessage
	switch {
	case *argsJSON != "" && *argsFile != "":
		fmt.Fprintln(stderr, "crw flow lint: error: --args and --args-file exclude each other")
		return 2
	case *argsJSON != "":
		argsRaw = json.RawMessage(*argsJSON)
	case *argsFile != "":
		data, err := os.ReadFile(*argsFile)
		if err != nil {
			fmt.Fprintf(stderr, "crw flow lint: error: %v\n", err)
			return 2
		}
		argsRaw = data
	}
	if len(argsRaw) > 0 && !json.Valid(argsRaw) {
		fmt.Fprintln(stderr, "crw flow lint: error: args is not valid JSON")
		return 2
	}
	policy, _, err := engine.LoadPolicy()
	if err != nil {
		fmt.Fprintf(stderr, "crw flow lint: error: %v\n", err)
		return 2
	}
	var reports []Report
	for _, file := range scripts {
		text, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(stderr, "crw flow lint: error: %v\n", err)
			return 2
		}
		root, err := os.OpenRoot(filepath.Dir(file))
		if err != nil {
			fmt.Fprintf(stderr, "crw flow lint: error: %v\n", err)
			return 2
		}
		reports = append(reports, Check(ctx, script.Source{Name: filepath.Base(file), Text: text, Dir: root.FS()}, Options{Policy: policy, DryRun: !*noDry, Args: argsRaw}))
		root.Close()
		reports[len(reports)-1].Script = file
	}
	failed := false
	for _, r := range reports {
		failed = failed || !r.OK()
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			fmt.Fprintf(stderr, "crw flow lint: error: %v\n", err)
			return 2
		}
	} else {
		for _, r := range reports {
			if r.OK() {
				for _, d := range r.Diagnostics {
					fmt.Fprintf(stdout, "%s: %s\n", r.Script, d)
				}
				fmt.Fprintf(stdout, "%s: ok (dry run: %s)\n", r.Script, r.DryRun)
				continue
			}
			for _, d := range r.Diagnostics {
				fmt.Fprintf(stdout, "%s: %s\n", r.Script, d)
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}
