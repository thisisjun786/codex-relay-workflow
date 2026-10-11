package compile

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// Usage is the one-line usage of the command.
const Usage = `usage: crw flow compile [--dry-run] [--base LOG.json]... <handoff.json | ->`

const usageHelp = Usage + `

Compile the handoff of crw-plan (crw-flow-compile-input/1: the issues of each project with their prerequisites, edit regions,
size_fit, size_reason, issue-size answer, group and milestone) into the plan revision each project takes. It reads and folds in
process, through the rules of dag-plan-put, and writes nothing: the revision documents are printed, one per project, to be put
with dag-plan-put.

  --dry-run     print the previews only (changes, parallel width, critical path, size_unreviewed), without the revision documents
  --base FILE   the whole log (crw relay dag-plan-log --after 0, every page) of a plan that exists, so that the changes are
                update_node, add_node and retire_node against it and no retired id is used again; repeat for each plan

Exit 0 compiled; 1 the handoff is refused (the reasons are printed on stderr and as JSON on stdout); 2 an input that cannot be used.
`

type baseFiles []string

func (b *baseFiles) String() string     { return strings.Join(*b, ",") }
func (b *baseFiles) Set(v string) error { *b = append(*b, v); return nil }

// RunCLI is `crw flow compile`.
func RunCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crw flow compile", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	dry := fs.Bool("dry-run", false, "print the previews only")
	var bases baseFiles
	fs.Var(&bases, "base", "the whole log of an existing plan")
	// flags may follow the file
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(stdout, usageHelp)
				return 0
			}
			fmt.Fprintln(stderr, Usage)
			fmt.Fprintf(stderr, "crw flow compile: error: %v\n", err)
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "crw flow compile: error: "+format+"\n", a...)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, Usage)
		return fail("exactly one handoff file (or - for stdin) is required")
	}
	var raw []byte
	var err error
	if positional[0] == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(positional[0])
	}
	if err != nil {
		return fail("%v", err)
	}
	in, err := Decode(raw)
	if err != nil {
		return fail("the handoff is not readable: %v", err)
	}
	keyOf := map[string]string{}
	for _, p := range in.Projects {
		keyOf[planID(p)] = p.Key
	}
	opts := Options{DryRun: *dry, Bases: map[string]dag.State{}}
	for _, file := range bases {
		log, err := os.ReadFile(file)
		if err != nil {
			return fail("%v", err)
		}
		var head struct {
			PlanID string `json:"plan_id"`
		}
		if err := json.Unmarshal(log, &head); err != nil {
			return fail("--base %s is not readable: %v", file, err)
		}
		key, ok := keyOf[head.PlanID]
		if !ok {
			return fail("--base %s is the log of plan %q, which no project of the handoff writes", file, head.PlanID)
		}
		if _, twice := opts.Bases[head.PlanID]; twice {
			return fail("two --base files are the log of plan %q", head.PlanID)
		}
		state, err := BaseFromLog(log, key)
		if err != nil {
			return fail("--base %s: %v", file, err)
		}
		opts.Bases[head.PlanID] = state
	}
	res, err := Compile(in, opts)
	var refused *Refused
	switch {
	case errors.As(err, &refused):
		for _, r := range refused.Refusals {
			where := strings.TrimSpace(r.Project + " " + r.Path)
			fmt.Fprintf(stderr, "crw flow compile: refused: [%s] %s: %s\n", r.Rule, where, r.Detail)
		}
		out, _ := json.MarshalIndent(map[string]any{"ok": false, "schema": OutputSchema, "refusals": refused.Refusals}, "", "  ")
		fmt.Fprintf(stdout, "%s\n", out)
		return 1
	case err != nil:
		return fail("%v", err)
	}
	out, err := Marshal(res)
	if err != nil {
		return fail("%v", err)
	}
	_, _ = stdout.Write(out)
	return 0
}
