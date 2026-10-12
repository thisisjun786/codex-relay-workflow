package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
)

// Usage is the usage of the commands this package serves.
const Usage = `usage: crw flow trace export --store-copy PATH [--run RUN] --out FILE
       crw flow shadow --trace FILE --script W [--run RUN] [--args JSON | --args-file FILE] [--out FILE]
       crw flow measure --trace FILE --out FILE [--run RUN] [--oversized-tokens N]`

// Handles reports whether `crw flow args...` is one of the commands this package serves.
func Handles(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "trace", "shadow", "measure":
		return true
	}
	return false
}

// RunCLI serves `crw flow trace export`, `crw flow shadow` and `crw flow measure`. It exits 0 when the command did what it was
// asked, 1 when the script of a shadow comparison does not load or ends in script_error, and 2 for a command line it cannot use
// or a path it refuses (a live store). None of them calls an agent or writes to a store.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case "trace":
		if len(args) < 2 || args[1] != "export" {
			fmt.Fprintln(stderr, Usage)
			fmt.Fprintln(stderr, "crw flow trace: error: the command is `export`")
			return 2
		}
		return exportCmd(ctx, args[2:], stdout, stderr)
	case "shadow":
		return shadowCmd(ctx, args[1:], stdout, stderr)
	case "measure":
		return measureCmd(args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, Usage)
	return 2
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, Usage) }
	return fs
}

// parse reads a command line that takes no positional argument. It returns -1 to continue, or the exit status.
func parse(fs *flag.FlagSet, args []string, name string, stderr io.Writer) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: error: takes no positional argument\n", name)
		return 2
	}
	return -1
}

// sameFile reports whether two paths name one existing file; a path that does not exist names none.
func sameFile(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

func fail(stderr io.Writer, name string, err error) int {
	fmt.Fprintf(stderr, "%s: error: %v\n", name, err)
	return 2
}

func exportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const name = "crw flow trace export"
	fs := newFlags(name, stderr)
	copyPath := fs.String("store-copy", "", "a copy of a relay store, made in a temporary directory (never the live store)")
	run := fs.String("run", "", "export only this run")
	out := fs.String("out", "", "the trace file to write")
	if code := parse(fs, args, name, stderr); code >= 0 {
		return code
	}
	if *copyPath == "" || *out == "" {
		fmt.Fprintln(stderr, Usage)
		fmt.Fprintf(stderr, "%s: error: --store-copy and --out are required\n", name)
		return 2
	}
	if err := trace.RefuseLive(*out); err != nil {
		return fail(stderr, name, err)
	}
	if sameFile(*out, *copyPath) {
		return fail(stderr, name, errors.New("--out is the store copy: the copy is never written"))
	}
	t, err := trace.Export(ctx, trace.Options{Path: *copyPath, Run: *run})
	if err != nil {
		return fail(stderr, name, err)
	}
	if err := trace.Write(*out, t); err != nil {
		return fail(stderr, name, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%d runs)\n", *out, len(t.Runs))
	return 0
}

func shadowCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const name = "crw flow shadow"
	fs := newFlags(name, stderr)
	tracePath := fs.String("trace", "", "the crw-flow-trace/1 file")
	scriptPath := fs.String("script", "", "the workflow script W to replay")
	run := fs.String("run", "", "compare only this run")
	argsJSON := fs.String("args", "", "the script's args, as JSON")
	argsFile := fs.String("args-file", "", "a file holding the script's args as JSON")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	pool := fs.Int("pool", 0, "agent calls in flight at once (default 6)")
	if code := parse(fs, args, name, stderr); code >= 0 {
		return code
	}
	if *tracePath == "" || *scriptPath == "" {
		fmt.Fprintln(stderr, Usage)
		fmt.Fprintf(stderr, "%s: error: --trace and --script are required\n", name)
		return 2
	}
	var argsRaw json.RawMessage
	switch {
	case *argsJSON != "" && *argsFile != "":
		return fail(stderr, name, errors.New("--args and --args-file exclude each other"))
	case *argsJSON != "":
		argsRaw = json.RawMessage(*argsJSON)
	case *argsFile != "":
		data, err := os.ReadFile(*argsFile)
		if err != nil {
			return fail(stderr, name, err)
		}
		argsRaw = data
	}
	if len(argsRaw) > 0 && !json.Valid(argsRaw) {
		return fail(stderr, name, errors.New("args is not valid JSON"))
	}
	if *out != "" {
		if err := trace.RefuseLive(*out); err != nil {
			return fail(stderr, name, err)
		}
		if sameFile(*out, *tracePath) || sameFile(*out, *scriptPath) {
			return fail(stderr, name, errors.New("--out is an input file"))
		}
	}
	if err := trace.RefuseLive(*scriptPath); err != nil {
		return fail(stderr, name, err)
	}
	tr, err := trace.ReadFile(*tracePath)
	if err != nil {
		return fail(stderr, name, err)
	}
	if tr, err = tr.Select(*run); err != nil {
		return fail(stderr, name, err)
	}
	text, err := os.ReadFile(*scriptPath)
	if err != nil {
		return fail(stderr, name, err)
	}
	root, err := os.OpenRoot(filepath.Dir(*scriptPath))
	if err != nil {
		return fail(stderr, name, err)
	}
	defer root.Close()
	policy, _, err := engine.LoadPolicy()
	if err != nil {
		return fail(stderr, name, err)
	}
	rep, err := Compare(ctx, tr, Config{Script: text, Name: filepath.Base(*scriptPath), Dir: root.FS(), Args: argsRaw, Policy: policy, Pool: *pool})
	if err != nil {
		fmt.Fprintf(stderr, "%s: error: %v\n", name, err)
		return 1
	}
	if code := emit(rep, *out, name, stdout, stderr); code != 0 {
		return code
	}
	for _, r := range rep.Runs {
		if r.Replay.Status != engine.RunCompleted {
			return 1
		}
	}
	return 0
}

func measureCmd(args []string, stdout, stderr io.Writer) int {
	const name = "crw flow measure"
	fs := newFlags(name, stderr)
	tracePath := fs.String("trace", "", "the crw-flow-trace/1 file")
	out := fs.String("out", "", "the measurement table to write (JSON)")
	run := fs.String("run", "", "measure only this run")
	oversized := fs.Int64("oversized-tokens", 0, "the token total above which a node counts as oversized (0: none)")
	if code := parse(fs, args, name, stderr); code >= 0 {
		return code
	}
	if *tracePath == "" || *out == "" {
		fmt.Fprintln(stderr, Usage)
		fmt.Fprintf(stderr, "%s: error: --trace and --out are required\n", name)
		return 2
	}
	if *oversized < 0 {
		return fail(stderr, name, errors.New("--oversized-tokens is 0 or more"))
	}
	if err := trace.RefuseLive(*out); err != nil {
		return fail(stderr, name, err)
	}
	if sameFile(*out, *tracePath) {
		return fail(stderr, name, errors.New("--out is the trace: it is never overwritten"))
	}
	tr, err := trace.ReadFile(*tracePath)
	if err != nil {
		return fail(stderr, name, err)
	}
	if tr, err = tr.Select(*run); err != nil {
		return fail(stderr, name, err)
	}
	return emit(BuildMeasure(tr, MeasureOptions{OversizedTokens: *oversized}), *out, name, stdout, stderr)
}

// emit writes a report as indented JSON to out, or to stdout when out is empty.
func emit(v any, out, name string, stdout, stderr io.Writer) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(stderr, name, err)
	}
	if out == "" {
		_, _ = stdout.Write(buf.Bytes())
		return 0
	}
	if err := trace.WriteBytes(out, buf.Bytes()); err != nil {
		return fail(stderr, name, err)
	}
	fmt.Fprintf(stdout, "wrote %s\n", out)
	return 0
}
