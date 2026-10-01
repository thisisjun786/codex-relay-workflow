package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var guardEvaluateCommand = dispatch.Command{Name: "guard-evaluate", Exempt: true, ReadOnly: true, Defaults: map[string]any{"stop-input": "-", "mode": "observe"}, Run: func(ctx context.Context, s dispatch.Services, a dispatch.Args) (any, error) {
	path, _ := a.String("stop-input")
	var raw []byte
	var err error
	if path != "" && path != "-" {
		path, err = store.ExpandUser(path)
		if err == nil {
			raw, err = os.ReadFile(path)
		}
		if err == nil && !utf8.Valid(raw) {
			err = errors.New(path + " is not UTF-8 text")
		}
		if err != nil {
			return nil, &dispatch.UsageError{Detail: "the Stop payload file could not be read: " + err.Error(), Code: contract.ExitUsage}
		}
	} else {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(raw) {
			return nil, dispatch.Host("the Stop payload on stdin is not UTF-8 text")
		}
	}
	v, err := hook.Decode(raw)
	if err != nil {
		return nil, &dispatch.UsageError{Detail: "the Stop payload is not JSON: " + err.Error(), Code: contract.ExitUsage}
	}
	stop, ok := evidence.Object(v)
	if !ok {
		return nil, &dispatch.UsageError{Detail: "the Stop payload must be a JSON object", Code: contract.ExitUsage}
	}
	mode, _ := a.String("mode")
	if mode == hook.Hold && a.Bool("no-record") {
		return nil, &dispatch.UsageError{Detail: "--mode hold cannot be combined with --no-record: a hold that publishes no observation cannot be released, counted against the bounds, or audited", Code: contract.ExitUsage}
	}
	rootArg, _ := a.String("marker-root")
	root, err := delivery.ResolveMarkerRoot(rootArg)
	if err != nil {
		return nil, err
	}
	now, _ := a.String("now")
	db, _ := a.String("db-path")
	// The selection's fallback answers once per process, so the local evaluation reuses the
	// answer the routing decision below read (cmd_guard_evaluate's _once).
	fallback := onceFallback(func() (string, error) { return guardFallback(s) })
	options := hook.GuardOptions{Root: root.Path, Now: now, Mode: mode, DBPath: db, NoRecord: a.Bool("no-record"), DefaultDBPath: fallback}
	answer, routed, err := routeGuard(ctx, s, stop, options)
	if err != nil || routed {
		return answer, err
	}
	return hook.Evaluate(ctx, stop, options)
}}

// routeGuard is cmd_guard_evaluate before its own evaluation. A Stop whose evaluation reads a
// receipt store (hook.SelectedStore, which also answers the selection's refusal) is asked of
// the owner of the store it would read: --db-path, else the dbPath the intent recorded, else
// the selected store. The owner's answer is this command's (routed): its error record with the
// exit status it carries, a host error for an owner that timed out or said nothing readable,
// or the fence's refusal when the owner could not be asked and another runtime owns the store.
// Where this runtime may evaluate it (no owner could be asked, and the store is unfenced or
// Go's), the store's ownership is checked on the read-only Stop path (store.CheckStop) before
// the caller evaluates. A Stop judged on its marker alone reads no store and is evaluated here
// whoever owns one.
func routeGuard(ctx context.Context, s dispatch.Services, stop hook.Object, options hook.GuardOptions) (any, bool, error) {
	receipt, err := hook.SelectedStore(ctx, stop, options)
	var refusal *dispatch.PayloadExit
	if errors.As(err, &refusal) {
		return nil, true, err
	}
	if err != nil || receipt == "" {
		return nil, false, nil
	}
	selected := options.DBPath
	if selected == "" {
		selected = s.Selection.DBPath()
		if workspace := evidence.Get(stop, "cwd"); pyvalue.Truthy(workspace) {
			path, _ := workspace.(string)
			_, facts, _, err := delivery.SelectAssignmentContext(ctx, options.Root, path, evidence.Get(stop, "session_id"))
			if err != nil {
				return nil, true, err
			}
			intent, _ := evidence.Object(evidence.Get(facts, "intent"))
			if recorded, ok := evidence.Get(intent, "dbPath").(string); ok {
				selected = recorded
			}
		}
	}
	routing := options
	routing.SocketPath, routing.Program = s.SocketPath, s.Program
	if options.DBPath != "" {
		// Absolute, as Path.expanduser().absolute() spells it: the owner resolves nothing
		// against this process's working directory.
		if routing.DBPath, err = store.ExpandUser(options.DBPath); err == nil && !filepath.IsAbs(routing.DBPath) {
			var cwd string
			cwd, err = os.Getwd()
			routing.DBPath = cwd + "/" + routing.DBPath
		}
		if err != nil {
			return nil, true, err
		}
		routing.DBPath = pathlibSpelling(routing.DBPath)
	}
	if selected == "" {
		selected = "."
	}
	answer, err := hook.RouteGuard(ctx, filepath.Dir(store.ResolveLoosely(selected)), stop, routing)
	switch {
	case err != nil:
		// Sent, then failed: the owner may hold part of the request, so it is neither refused
		// nor evaluated here.
		return nil, true, dispatch.Host(err.Error())
	case answer == nil:
		// The read-only Stop path verifies the durable owner without copying the store or
		// creating a sidecar, and writes nothing to it.
		if err := store.CheckStop(ctx, selected); err != nil {
			return nil, true, err
		}
		return nil, false, nil
	case answer.TimedOut:
		return nil, true, &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: answer.Detail}}, Code: contract.ExitHost}
	case !answer.Readable:
		return nil, true, &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: "the owner closed control.sock without a readable guard-evaluate answer"}}, Code: contract.ExitHost}
	case answer.Code != contract.ExitOk:
		payload, _ := evidence.Object(answer.Answer)
		return nil, true, &dispatch.PayloadExit{Payload: payload, Code: answer.Code}
	}
	return answer.Answer, true, nil
}

// onceFallback is resolve answered once: a later call returns the first call's answer.
func onceFallback(resolve func() (string, error)) func() (string, error) {
	var once sync.Once
	var path string
	var err error
	return func() (string, error) {
		once.Do(func() { path, err = resolve() })
		return path, err
	}
}

// pathlibSpelling is an absolute path as pathlib spells it: empty and "." components dropped,
// ".." kept.
func pathlibSpelling(path string) string {
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return "/" + strings.Join(parts, "/")
}
