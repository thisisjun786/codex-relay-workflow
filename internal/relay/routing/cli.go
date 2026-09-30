package routing

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var CommandNames = []string{"product-register", "product-bind", "product-show", "route-policy", "route-intake", "route-classify", "route-reconcile", "route-show", "route-digest", "route-projects", "completion-check"}

type wallClock struct{}

func (wallClock) Now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }
func (wallClock) ISO() string  { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

type clockContextKey struct{}
type ledgerMissingKey struct{}

func WithClock(ctx context.Context, clock interface {
	Now() float64
	ISO() string
}) context.Context {
	return context.WithValue(ctx, clockContextKey{}, clock)
}
func init() {
	for _, name := range CommandNames {
		name := name
		spec := argparse.Specs[name]
		cli.Commands = append(cli.Commands, cli.Command{Name: name, Flags: func(f *flag.FlagSet) {
			for _, action := range spec.Actions {
				if len(action.Flags) == 0 || action.Kind == "_HelpAction" {
					continue
				}
				key := strings.TrimPrefix(action.Flags[len(action.Flags)-1], "--")
				if action.Kind == "_StoreTrueAction" {
					f.Bool(key, false, "")
				} else if action.Type == "int" {
					fallback := int64(0)
					if key == "limit" {
						fallback = map[string]int64{"route-reconcile": 50, "route-show": 20, "route-digest": 500}[name]
					}
					f.Int64(key, fallback, "")
				} else {
					f.String(key, "", "")
				}
			}
		}, Run: func(ctx context.Context, services cli.Services, args cli.Args) (any, error) {
			return runCommand(ctx, services, args, name)
		}})
	}
}
func routeJSON(value, what string) (any, error) {
	raw := value
	if path, ok := strings.CutPrefix(value, "@"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			message := store.PythonOSError(err)
			if _, detail, ok := strings.Cut(message, ": "); ok {
				message = detail
			}
			return nil, malformed("the " + what + " file cannot be read: " + message)
		}
		raw = strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		return nil, malformed("the " + what + " is not readable JSON: " + store.PythonJSONError(raw))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, malformed("the " + what + " is not readable JSON: " + store.PythonJSONError(raw))
	}
	return out, nil
}
func runCommand(ctx context.Context, services cli.Services, args cli.Args, name string) (any, error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	clock := interface {
		Now() float64
		ISO() string
	}(wallClock{})
	if injected, ok := ctx.Value(clockContextKey{}).(interface {
		Now() float64
		ISO() string
	}); ok {
		clock = injected
	}
	r := New(s, clock)
	if missing, ok := ctx.Value(ledgerMissingKey{}).([]string); ok {
		r.Missing = missing
	}
	get := func(key string) string { v, _ := args.String(key); return v }
	optional := func(key string) any {
		v, present := args.String(key)
		if !present {
			return nil
		}
		return v
	}
	numeric := func(key string) any {
		v, present := args.String(key)
		if key == "after" && !present {
			return nil
		}
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil {
			return json.Number(v)
		}
		return n
	}
	var answer any
	switch name {
	case "product-register", "product-bind", "route-policy", "route-intake", "completion-check":
		key, what := "record", "registry record"
		switch name {
		case "product-bind":
			what = "binding"
		case "route-policy":
			what = "routing policy"
		case "route-intake":
			key, what = "incident", "incident"
		case "completion-check":
			key, what = "reading", "completion reading"
		}
		var value any
		value, err = routeJSON(get(key), what)
		if err == nil {
			switch name {
			case "product-register":
				answer, err = r.RegisterProduct(ctx, value)
			case "product-bind":
				answer, err = r.Bind(ctx, value)
			case "route-policy":
				answer, err = r.SetPolicy(ctx, value)
			case "route-intake":
				answer, err = r.Intake(ctx, value)
			case "completion-check":
				answer, err = r.CheckCompletion(ctx, value)
			}
		}
	case "product-show":
		answer, err = r.ShowProducts(ctx, optional("product"))
	case "route-classify":
		var value any
		value, err = routeJSON(get("classification"), "classification")
		if err == nil {
			answer, err = r.Classify(ctx, get("fault"), value)
		}
	case "route-reconcile":
		answer, err = r.Reconcile(ctx, optional("product"), numeric("limit"), numeric("after"))
	case "route-show":
		answer, err = r.Show(ctx, optional("product"), args.Bool("attention"), numeric("limit"), numeric("after"))
	case "route-digest":
		answer, err = r.Digest(ctx, numeric("limit"), numeric("after"))
	case "route-projects":
		answer, err = r.EvaluateProjects(ctx, get("product"))
	}
	if err != nil {
		detail := cleanError(err)
		reason, message, _ := strings.Cut(detail, ":")
		if strings.HasPrefix(reason, "route_") || strings.HasPrefix(reason, "fault_") {
			return nil, &cli.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: reason}, {Key: "detail", Value: strings.TrimPrefix(message, " ")}}, Code: 2}
		}
		return nil, err
	}
	return CommandRecord(name, answer), nil
}
