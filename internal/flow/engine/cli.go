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

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

const flowUsage = `usage: crw flow run --backend fake [--args JSON | --args-file FILE] [--pool N] [--token-budget N] [--journal] [--summary-document D] (<script.js> | --script JS [--plan-only=false])
       crw flow roles [--json]
       crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)
       crw flow resume [--state DIR] [--socket PATH] [--repo DIR] [--args JSON | --args-file FILE] RUN SCRIPT
       crw flow lint [--args JSON | --args-file FILE] [--no-dry-run] [--json] <script.js>...`

// Commands are the first words of the commands RunCLI serves itself; lint is internal/flow/lint's, and the rest of `crw flow` is
// internal/flow/cli's and internal/flow/shadow's.
var Commands = []string{"run", "roles", "compile", "recover", "resume"}

// PrintUsage writes the usage lines of the commands this package serves, with compile's.
func PrintUsage(w io.Writer) {
	fmt.Fprintln(w, flowUsage)
	fmt.Fprintln(w, compile.Usage)
}

// lookupEnv reads the environment for the policy file; tests replace it.
var lookupEnv = os.LookupEnv

// RunCLI is `crw flow`: the run command over the fake backend and the compile command (the App Server backend and the rest of the flow
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
		fmt.Fprintln(stdout, compile.Usage)
		return 0
	case "run":
		return runCmd(ctx, args[1:], stdout, stderr)
	case "roles":
		return rolesCmd(args[1:], stdout, stderr)
	case "compile":
		return compile.RunCLI(ctx, args[1:], os.Stdin, stdout, stderr)
	case "recover":
		return recoverCmd(ctx, args[1:], stdout, stderr)
	case "resume":
		return resumeCmd(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, flowUsage)
	fmt.Fprintln(stderr, compile.Usage)
	fmt.Fprintf(stderr, "crw flow: error: argument command: invalid choice: %q (choose from '%s')\n", args[0], strings.Join(Commands, "', '"))
	return 2
}

// isolationRootEnv is testsupport.IsolationRootEnv: the isolation root a test binary exports to itself and to every process it
// starts. The engine names it here so it does not import testsupport.
const isolationRootEnv = "CRW_TEST_ISOLATION_ROOT"

// LoadPolicy is the role table of every production flow command (CRW-1330, CRW-1316 design section 8): the helper role
// store's snapshot of the six roles (PolicyFromRoleHelper), whose path is the table's Source and whose canonical digest of the
// six pairs is its Digest. The execution policy file CODEX_THREAD_BRIDGE_EXECUTION_POLICY names is still loaded and returned
// (its mode, its loader warnings); a file that does not load is an error, as for the bridge. Its agent-roles and agent-aliases
// sections are not read: a file that still declares them adds a warning to the table.
//
// The fake backend's placeholder table (FakePolicy) is the table of a test only: a process under the test isolation
// (isolationRootEnv) whose role store has no file. A test that writes a store reads it like production.
func LoadPolicy() (*Policy, execution.Policy, error) {
	env := map[string]string{}
	for _, name := range []string{execution.EnvPolicy, execution.EnvDigest} {
		if v, ok := lookupEnv(name); ok {
			env[name] = v
		}
	}
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		return nil, execution.Policy{}, err
	}
	snap := role.ReadFlowSnapshot(host.LookupEnv(lookupEnv))
	if root, _ := lookupEnv(isolationRootEnv); root != "" && !snap.Present {
		return FakePolicy(), policy, nil
	}
	table := PolicyFromRoleHelper(snap)
	if policy.DeclaresAgentRoles() {
		table.Warnings = append(table.Warnings, fmt.Sprintf("the execution policy %s declares agent-roles: the flow no longer reads it (its pairs come from the helper role store %s); compare its pairs with crw flow roles, then remove the section", strings.TrimSpace(env[execution.EnvPolicy]), snap.Path))
	}
	return table, policy, nil
}

// rolesCmd is `crw flow roles`: the policy a flow run resolves calls against, with its digest and source.
func rolesCmd(args []string, stdout, stderr io.Writer) int {
	fs_ := flag.NewFlagSet("crw flow roles", flag.ContinueOnError)
	fs_.SetOutput(stderr)
	asJSON := fs_.Bool("json", false, "print the policy as JSON")
	fs_.Usage = func() { fmt.Fprintln(stderr, flowUsage) }
	if err := fs_.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs_.NArg() != 0 {
		fmt.Fprintln(stderr, "crw flow roles: error: takes no arguments")
		return 2
	}
	table, policy, err := LoadPolicy()
	if err != nil {
		fmt.Fprintf(stderr, "crw flow roles: error: %v\n", err)
		return 2
	}
	report := DescribePolicy(table, policy)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "crw flow roles: error: %v\n", err)
			return 2
		}
		return 0
	}
	fmt.Fprint(stdout, report.Text())
	return 0
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
	inline := fs_.String("script", "", "an ad-hoc script given as text instead of a file")
	summaryDoc := fs_.String("summary-document", "", "the document the run keeps the plan summary in; recorded in the run's journal for the upward hook")
	planOnly := fs_.Bool("plan-only", true, "run the script on the fake backend and print the plan; the default for --script")
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
	scriptGiven := *inline != ""
	// plan-only is the default of an ad-hoc script only: a script file runs on the backend it names.
	planned := scriptGiven && *planOnly
	explicitPlan := false
	fs_.Visit(func(f *flag.Flag) { explicitPlan = explicitPlan || f.Name == "plan-only" })
	if explicitPlan && !scriptGiven {
		fmt.Fprintln(stderr, "crw flow run: error: --plan-only applies to --script")
		return 2
	}
	switch {
	case scriptGiven && len(positional) != 0:
		fmt.Fprintln(stderr, "crw flow run: error: --script and a script file exclude each other")
		return 2
	case !scriptGiven && len(positional) != 1:
		fmt.Fprintln(stderr, flowUsage)
		fmt.Fprintln(stderr, "crw flow run: error: exactly one script file is required")
		return 2
	}
	switch *backend {
	case "fake":
	case "":
		if planned {
			break
		}
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

	var (
		text []byte
		name string
		dir  fs.FS
	)
	if scriptGiven {
		// An ad-hoc script has no directory: it imports crw:lib/<name> only and nests no workflow.
		text, name = []byte(*inline), "script.js"
	} else {
		file := positional[0]
		var err error
		if text, err = os.ReadFile(file); err != nil {
			fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
			return 2
		}
		root, err := os.OpenRoot(filepath.Dir(file))
		if err != nil {
			fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
			return 2
		}
		defer root.Close()
		dir, name = root.FS(), filepath.Base(file)
	}
	table, _, err := LoadPolicy()
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 2
	}
	prog, err := script.Load(script.Source{Name: name, Text: text, Dir: dir})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 1
	}
	journal := NewMemoryJournal()
	res, err := Run(ctx, prog, Config{
		Backend: NewFake(), Policy: table, Journal: journal, Args: argsRaw, Pool: *pool, TokenBudget: *budget, SummaryDocument: *summaryDoc,
		// The journal sequence numbers of a call say in which order the host finished concurrent calls, which differs from
		// run to run; the script and the printed result do not hold them, so two runs of one script print the same bytes
		// (the journal rows, with --journal, keep them).
		OmitSeq: true,
		Workflows: func(name, scriptPath string) (*script.Program, error) {
			if dir == nil {
				return nil, errors.New("an ad-hoc script has no workflows directory")
			}
			return loadSibling(dir, name, scriptPath)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow run: error: %v\n", err)
		return 2
	}
	out := struct {
		*Result
		PlanOnly bool    `json:"planOnly,omitempty"`
		Journal  []Event `json:"journal,omitempty"`
	}{Result: res, PlanOnly: planned}
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
