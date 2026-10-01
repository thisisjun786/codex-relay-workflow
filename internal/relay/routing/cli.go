package routing

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var CommandNames = []string{"product-register", "product-bind", "product-show", "route-policy", "route-intake", "route-classify", "route-reconcile", "route-show", "route-digest", "route-projects", "completion-check"}

type wallClock struct{}

func (wallClock) Now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }
func (wallClock) ISO() string  { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

type clockContextKey struct{}

func WithClock(ctx context.Context, clock interface {
	Now() float64
	ISO() string
}) context.Context {
	return context.WithValue(ctx, clockContextKey{}, clock)
}
func init() {
	limits := map[string]int64{"route-reconcile": 50, "route-show": 20, "route-digest": 500}
	var commands []dispatch.Command
	for _, name := range CommandNames {
		var defaults map[string]any
		if limit, ok := limits[name]; ok {
			defaults = map[string]any{"limit": limit}
		}
		commands = append(commands, dispatch.Command{Name: name, Defaults: defaults,
			ReadOnly: name == "product-show" || name == "route-show",
			Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
				return runCommand(ctx, services, args, name)
			}})
	}
	dispatch.Register(nil, commands...)
}
func routeJSON(value, what string) (any, error) {
	raw := value
	if path, ok := strings.CutPrefix(value, "@"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, malformed("the " + what + " file cannot be read: " + err.Error())
		}
		raw = strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	}
	out, err := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers})
	if err != nil {
		return nil, malformed("the " + what + " is not readable JSON: " + err.Error())
	}
	return out, nil
}
func runCommand(ctx context.Context, services dispatch.Services, args dispatch.Args, name string) (any, error) {
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
			return nil, &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: reason}, {Key: "detail", Value: strings.TrimPrefix(message, " ")}}, Code: 2}
		}
		return nil, err
	}
	return CommandRecord(answer), nil
}
