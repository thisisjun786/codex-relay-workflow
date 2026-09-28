package cli

import (
	"context"
	"flag"
	"io"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var guardEvaluateCommand = Command{Name: "guard-evaluate", Exempt: true, Flags: func(f *flag.FlagSet) {
	f.String("marker-root", "", "")
	f.String("stop-input", "-", "")
	f.String("mode", "observe", "")
	f.String("db-path", "", "")
	f.String("now", "", "")
	f.Bool("no-record", false, "")
}, Run: func(ctx context.Context, s Services, a Args) (any, error) {
	path, _ := a.String("stop-input")
	var raw []byte
	var err error
	if path != "" && path != "-" {
		path, err = store.ExpandUser(path)
		if err == nil {
			raw, err = os.ReadFile(path)
		}
		if err == nil {
			_, err = store.DecodeUTF8(raw)
		}
		if err != nil {
			return nil, &UsageError{Detail: "the Stop payload file could not be read: " + store.PythonOSErrorText(err), Code: contract.ExitUsage}
		}
	} else {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		if _, err := store.DecodeUTF8(raw); err != nil {
			return nil, &HostError{Class: "UnicodeDecodeError", Detail: err.Error()}
		}
	}
	v, err := hook.Decode(raw)
	if err != nil {
		return nil, &UsageError{Detail: "the Stop payload is not JSON: " + err.Error(), Code: contract.ExitUsage}
	}
	stop, ok := evidence.Object(v)
	if !ok {
		return nil, &UsageError{Detail: "the Stop payload must be a JSON object", Code: contract.ExitUsage}
	}
	mode, _ := a.String("mode")
	if mode == hook.Hold && a.Bool("no-record") {
		return nil, &UsageError{Detail: "--mode hold cannot be combined with --no-record: a hold that publishes no observation cannot be released, counted against the bounds, or audited", Code: contract.ExitUsage}
	}
	rootArg, _ := a.String("marker-root")
	root, err := delivery.ResolveMarkerRoot(rootArg)
	if err != nil {
		return nil, err
	}
	now, _ := a.String("now")
	db, _ := a.String("db-path")
	return hook.Evaluate(ctx, stop, hook.GuardOptions{Root: root.Path, Now: now, Mode: mode, DBPath: db, NoRecord: a.Bool("no-record"), DefaultDBPath: func() (string, error) { return guardFallback(s) }})
}}
