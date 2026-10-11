package engine

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

const flowUsage = `usage: crw flow run --backend fake [--args JSON | --args-file FILE] [--pool N] [--token-budget N] [--journal] <script.js>`

// RunCLI is `crw flow`: the run command over the fake backend (the App Server backend and the rest of the flow
// commands arrive with their own issues). It prints the run's result as JSON on stdout and exits 0 when the script
// completed, 1 when it ended in script_error, and 2 for a command line it cannot use.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, flowUsage)
		fmt.Fprintln(stderr, "crw flow: error: the following arguments are required: command")
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, flowUsage)
		return 0
	case "run":
		return runCmd(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, flowUsage)
	fmt.Fprintf(stderr, "crw flow: error: argument command: invalid choice: %q (choose from 'run')\n", args[0])
	return 2
}

func runCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs_ := flag.NewFlagSet("crw flow run", flag.ContinueOnError)
	fs_.SetOutput(stderr)
	backend := fs_.String("backend", "", "backend to run on (fake)")
	argsJSON := fs_.String("args", "", "the script's args, as JSON")
	argsFile := fs_.String("args-file", "", "a file holding the script's args as JSON")
	pool := fs_.Int("pool", 0, "agent calls in flight at once (default 6)")
	budget := fs_.Int64("token-budget", 0, "the run's token ceiling (0: none)")
	withJournal := fs_.Bool("journal", false, "include the journal rows in the output")
	fs_.Usage = func() { fmt.Fprintln(stderr, flowUsage) }
	// flags may follow the script name
	var positional []string
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
		positional = append(positional, fs_.Arg(0))
		rest = fs_.Args()[1:]
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, flowUsage)
		fmt.Fprintln(stderr, "crw flow run: error: exactly one script file is required")
		return 2
	}
	switch *backend {
	case "fake":
	case "":
		fmt.Fprintln(stderr, "crw flow run: error: --backend is required (this build has: fake)")
		return 2
	default:
		fmt.Fprintf(stderr, "crw flow run: error: backend %q is not available in this build (this build has: fake)\n", *backend)
		return 2
	}
	var argsRaw json.RawMessage
	switch {
	case *argsJSON != "" && *argsFile != "":
		fmt.Fprintln(stderr, "crw flow run: error: --args and --args-file exclude each other")
		return 2
	case *argsJSON != "":
		argsRaw = json.RawMessage(*argsJSON)
	case *argsFile != "":
		data, err := os.ReadFile(*argsFile)
		if err != nil {
			fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
			return 2
		}
		argsRaw = data
	}
	if len(argsRaw) > 0 && !json.Valid(argsRaw) {
		fmt.Fprintln(stderr, "crw flow run: error: args is not valid JSON")
		return 2
	}

	file := positional[0]
	text, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 2
	}
	root, err := os.OpenRoot(filepath.Dir(file))
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 2
	}
	defer root.Close()
	dir := root.FS()
	prog, err := script.Load(script.Source{Name: filepath.Base(file), Text: text, Dir: dir})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 1
	}
	journal := NewMemoryJournal()
	res, err := Run(ctx, prog, Config{
		Backend: NewFake(), Policy: FakePolicy(), Journal: journal, Args: argsRaw, Pool: *pool, TokenBudget: *budget,
		// The journal sequence numbers of a call say in which order the host finished concurrent calls, which differs from
		// run to run; the script and the printed result do not hold them, so two runs of one script print the same bytes
		// (the journal rows, with --journal, keep them).
		OmitSeq:   true,
		Workflows: func(name, scriptPath string) (*script.Program, error) { return loadSibling(dir, name, scriptPath) },
	})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 2
	}
	out := struct {
		*Result
		Journal []Event `json:"journal,omitempty"`
	}{Result: res}
	if *withJournal {
		out.Journal = journal.Events()
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 2
	}
	if res.Status != RunCompleted {
		return 1
	}
	return 0
}

// loadSibling resolves workflow(name) to <name>.js and workflow({scriptPath}) to that relative path, both inside the
// calling script's workflows directory.
func loadSibling(dir fs.FS, name, scriptPath string) (*script.Program, error) {
	rel := scriptPath
	if rel == "" {
		rel = name
		if !strings.HasSuffix(rel, ".js") {
			rel += ".js"
		}
	}
	rel = path.Clean(rel)
	if !fs.ValidPath(rel) {
		return nil, fmt.Errorf("%q is outside the workflows directory", rel)
	}
	text, err := fs.ReadFile(dir, rel)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(dir, path.Dir(rel))
	if err != nil {
		return nil, err
	}
	return script.Load(script.Source{Name: rel, Text: text, Dir: sub})
}
