package cli_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The forge the merge-evidence tests collect from is a scripted gh on PATH. testdata/gh was the
// scripted forge the Python oracle ran against (a Python script, deleted with the oracle drivers
// in todo 44; the goldens began as what Python said against it); fakeGH is the same script in
// Go, which those tests run: this test binary, invoked through a link named gh (TestMain
// dispatches on the name). The environment scripts it as it scripted testdata/gh:
// CRW_FORGE_SCENARIO, CRW_FORGE_RULES, CRW_FORGE_RULES_JSON, CRW_FORGE_PATCH and CRW_FORGE_LOG.

var (
	ghOnce sync.Once
	ghDir  string
	ghErr  error
)

// goForgePath is PATH with the Go scripted gh first.
func goForgePath(t testing.TB) string {
	t.Helper()
	ghOnce.Do(func() {
		var executable string
		if executable, ghErr = os.Executable(); ghErr != nil {
			return
		}
		if ghDir, ghErr = os.MkdirTemp("", "crw-cli-gh-"); ghErr != nil {
			return
		}
		ghErr = os.Symlink(executable, filepath.Join(ghDir, "gh"))
	})
	if ghErr != nil {
		t.Fatal(ghErr)
	}
	return "PATH=" + ghDir + ":" + os.Getenv("PATH")
}

func ghObject(fields ...any) contract.OrderedObject {
	o := contract.OrderedObject{}
	for i := 0; i+1 < len(fields); i += 2 {
		o = append(o, contract.Field{Key: fields[i].(string), Value: fields[i+1]})
	}
	return o
}

// fakeGH answers one gh invocation as testdata/gh did and returns its exit status.
func fakeGH(args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(stderr, "Traceback (most recent call last):\n  fake gh: %v\n", recovered)
			code = 1
		}
	}()
	scenario := os.Getenv("CRW_FORGE_SCENARIO")
	if scenario == "" {
		scenario = "ready"
	}
	ruleShape := os.Getenv("CRW_FORGE_RULES")
	if ruleShape == "" {
		ruleShape = "one"
	}
	if strings.HasPrefix(scenario, "multi-") {
		parts := strings.SplitN(scenario, "-", 3)
		ruleShape, scenario = parts[1], parts[2]
	}
	target := args[len(args)-1]
	if log := os.Getenv("CRW_FORGE_LOG"); log != "" {
		items := make([]any, len(args))
		for i, a := range args {
			items[i] = a
		}
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			panic(err)
		}
		_, _ = io.WriteString(f, pyjson.Dumps(items, pyjson.Options{})+"\n")
		_ = f.Close()
	}
	graphql := len(args) >= 2 && args[0] == "api" && args[1] == "graphql"
	query := func(fallback bool) string {
		for _, a := range args {
			if strings.HasPrefix(a, "query=") {
				return a[6:]
			}
		}
		if fallback {
			return ""
		}
		panic("StopIteration")
	}
	out := func(v any) {
		if raw := os.Getenv("CRW_FORGE_PATCH"); raw != "" {
			patch := loadsForge(raw)
			if object, ok := patch.(contract.OrderedObject); ok && len(object) > 0 {
				var endpoint string
				if graphql {
					q := query(false)
					switch {
					case strings.Contains(q, "\n    reviews("):
						endpoint = "reviews"
					case strings.Contains(q, "\n    comments("):
						endpoint = "comments"
					default:
						endpoint = "reviewThreads"
					}
				} else {
					switch {
					case strings.Contains(target, "/pulls/"):
						endpoint = "pull"
					case strings.Contains(target, "/jobs"):
						endpoint = "jobs"
					case strings.Contains(target, "/actions/runs"):
						endpoint = "runs"
					case strings.Contains(target, "/check-runs"):
						endpoint = "checks"
					case strings.Contains(target, "/status"):
						endpoint = "statuses"
					default:
						endpoint = "other"
					}
				}
				if evidence.Get(object, "endpoint") == endpoint {
					v = applyForgePatch(v, object)
				}
			}
		}
		fmt.Fprintln(stdout, pyjson.Dumps(v, pyjson.Options{Compact: true}))
	}
	forbidden := func() int {
		fmt.Fprintln(stderr, "gh: forbidden (HTTP 403)")
		return 1
	}
	if graphql {
		if ruleShape != "one" && scenario == "unknown" {
			return forbidden()
		}
		q := query(true)
		field, nodes := "reviewThreads", []any{}
		switch {
		case strings.Contains(q, "\n    reviews("):
			field = "reviews"
			if scenario == "rich" {
				nodes = []any{ghObject("id", "R1", "state", "APPROVED", "url", "u/R1", "body", "review text", "submittedAt", "t", "author", ghObject("login", "r"))}
			}
		case strings.Contains(q, "\n    comments("):
			field = "comments"
			if scenario == "rich" {
				nodes = []any{ghObject("id", "C1", "url", "u/C1", "body", "comment text", "createdAt", "t", "author", ghObject("login", "c"))}
			}
		default:
			count := 0
			if scenario == "unresolved" || scenario == "late" {
				count = 1
			}
			switch scenario {
			case "late":
				count = 2
			case "seven":
				count = 7
			case "rich":
				count = 1
			}
			for i := 1; i <= count; i++ {
				nodes = append(nodes, ghObject("id", fmt.Sprintf("T%d", i), "isResolved", scenario != "unresolved", "isOutdated", false, "path", "a", "line", i, "originalLine", nil,
					"comments", ghObject("nodes", []any{ghObject("url", fmt.Sprintf("u/T%d", i), "author", ghObject("login", "r"), "body", "x", "createdAt", "t")})))
			}
		}
		out(ghObject("data", ghObject("repository", ghObject("pullRequest", ghObject(field, ghObject("totalCount", len(nodes), "pageInfo", ghObject("hasNextPage", false, "endCursor", nil), "nodes", nodes))))))
		return 0
	}
	path, rawQuery, _ := strings.Cut(target, "?")
	a40, b40 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	switch {
	case strings.Contains(path, "/pulls/") && scenario == "unreadable":
		return forbidden()
	case strings.Contains(path, "/pulls/"):
		out(ghObject("number", 7, "html_url", "u", "state", "open", "merged", false, "draft", false, "head", ghObject("sha", a40), "base", ghObject("sha", b40, "ref", "dev"), "mergeable", true, "mergeable_state", "clean"))
	case strings.Contains(path, "/git/ref/"):
		out(ghObject("ref", "refs/heads/dev"))
	case strings.Contains(path, "/rules/branches/"):
		if ruleShape == "one" && scenario == "unknown" {
			return forbidden()
		}
		required := []any{ghObject("context", "dev-gate")}
		if scenario == "rich" {
			required = []any{ghObject("context", "external", "integration_id", 43), ghObject("context", "dev-gate", "integration_id", 42)}
		}
		rules := []any{ghObject("type", "required_status_checks", "parameters", ghObject("strict_required_status_checks_policy", false, "required_status_checks", required))}
		if ruleShape == "two" || ruleShape == "three" || ruleShape == "duplicate" {
			rules = append(rules, ghObject("type", "pull_request", "parameters", ghObject("required_review_thread_resolution", true)))
		}
		if ruleShape == "three" {
			rules = append(rules, ghObject("type", "non_fast_forward"))
		}
		if ruleShape == "duplicate" {
			rules = append(rules, rules[0])
		}
		if raw, ok := os.LookupEnv("CRW_FORGE_RULES_JSON"); ok {
			decoded := loadsForge(raw)
			list, isList := decoded.([]any)
			if !isList {
				out(decoded)
				return 0
			}
			rules, ruleShape = list, "custom"
		}
		if ruleShape == "one" {
			out(rules)
			return 0
		}
		params := map[string]string{}
		for _, pair := range strings.Split(rawQuery, "&") {
			if pair == "" {
				continue
			}
			key, value, _ := strings.Cut(pair, "=")
			k, _ := url.QueryUnescape(strings.ReplaceAll(key, "+", " "))
			v, _ := url.QueryUnescape(strings.ReplaceAll(value, "+", " "))
			if value != "" {
				params[k] = v
			}
		}
		page, size := pyInt(params, "page", 1), pyInt(params, "per_page", 100)
		if size > 0 {
			out(pySlice(rules, (page-1)*size, page*size))
		} else {
			out(rules)
		}
	case strings.Contains(path, "/actions/runs/1/jobs"):
		out(ghObject("total_count", 1, "jobs", []any{ghObject("id", 11, "name", "dev-gate", "run_attempt", 1, "status", "completed", "conclusion", "success", "html_url", "job", "started_at", nil, "completed_at", nil)}))
	case strings.Contains(path, "/actions/runs/2/jobs"):
		out(ghObject("total_count", 1, "jobs", []any{ghObject("id", 12, "name", "old-gate", "run_attempt", 1, "status", "completed", "conclusion", "cancelled", "html_url", "old-job", "started_at", nil, "completed_at", nil)}))
	case strings.Contains(path, "/actions/runs"):
		runs := []any{ghObject("id", 1, "name", "CI", "head_sha", a40, "workflow_id", 100, "event", "pull_request", "html_url", "run", "run_started_at", "2026-01-02")}
		if scenario == "rich" {
			runs = append(runs, ghObject("id", 2, "name", "CI", "head_sha", a40, "workflow_id", 100, "event", "pull_request", "html_url", "old-run", "conclusion", "cancelled", "run_started_at", "2026-01-01"))
		}
		out(ghObject("total_count", len(runs), "workflow_runs", runs))
	case strings.Contains(path, "/check-runs"):
		checks := []any{ghObject("id", 11, "name", "dev-gate", "head_sha", a40, "status", "completed", "conclusion", "success", "app", ghObject("id", 42, "slug", "actions"))}
		if scenario == "rich" {
			checks = append(checks, ghObject("id", 13, "name", "external", "head_sha", a40, "status", "completed", "conclusion", "success", "app", ghObject("id", 43, "slug", "external"), "html_url", "check", "started_at", "t", "completed_at", "t"))
		}
		out(ghObject("total_count", len(checks), "check_runs", checks))
	case strings.HasSuffix(path, "/status"):
		statuses := []any{}
		if scenario == "rich" {
			statuses = []any{ghObject("context", "status", "state", "success", "target_url", "status-url", "updated_at", "t")}
		}
		out(ghObject("total_count", len(statuses), "statuses", statuses))
	default:
		fmt.Fprintln(stderr, "unexpected "+target)
		return 1
	}
	return 0
}

func loadsForge(raw string) any {
	v, err := store.LoadsJSON([]byte(raw))
	if err != nil {
		panic(err)
	}
	return v
}

func pyInt(params map[string]string, key string, fallback int) int {
	text, ok := params[key]
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		panic(fmt.Sprintf("ValueError: invalid literal for int() with base 10: %q", text))
	}
	return n
}

// pySlice is list[start:stop].
func pySlice(list []any, start, stop int) []any {
	clamp := func(i int) int {
		if i < 0 {
			i += len(list)
		}
		return min(max(i, 0), len(list))
	}
	start, stop = clamp(start), clamp(stop)
	if start >= stop {
		return []any{}
	}
	return list[start:stop]
}

// applyForgePatch was testdata/gh's CRW_FORGE_PATCH: set or delete the value at path.
func applyForgePatch(v any, patch contract.OrderedObject) any {
	path, _ := evidence.Get(patch, "path").([]any)
	remove := pyvalue.Truthy(evidence.Get(patch, "delete"))
	value := func() any {
		value, ok := evidence.Lookup(patch, "value")
		if !ok {
			panic("KeyError: 'value'")
		}
		return value
	}
	if len(path) == 0 {
		if remove {
			return nil
		}
		return value()
	}
	return patchAt(v, path, remove, value)
}

// patchAt returns node with the value at path set (or deleted), as node[k1][k2]... = value.
func patchAt(node any, path []any, remove bool, value func() any) any {
	key := path[0]
	if len(path) > 1 {
		child := patchAt(forgeIndex(node, key), path[1:], remove, value)
		switch n := node.(type) {
		case contract.OrderedObject:
			for i := range n {
				if n[i].Key == key.(string) {
					n[i].Value = child
				}
			}
		case []any:
			n[forgeListIndex(n, key)] = child
		}
		return node
	}
	switch n := node.(type) {
	case contract.OrderedObject:
		name, ok := key.(string)
		if !ok {
			panic("KeyError")
		}
		index := -1
		for i := range n {
			if n[i].Key == name {
				index = i
			}
		}
		if remove {
			if index < 0 {
				return n
			}
			return append(n[:index:index], n[index+1:]...)
		}
		if index >= 0 {
			n[index].Value = value()
			return n
		}
		return append(n, contract.Field{Key: name, Value: value()})
	case []any:
		i := forgeListIndex(n, key)
		if remove {
			return append(n[:i:i], n[i+1:]...)
		}
		n[i] = value()
		return n
	}
	panic("TypeError: object does not support item assignment")
}

func forgeIndex(node, key any) any {
	switch n := node.(type) {
	case contract.OrderedObject:
		name, ok := key.(string)
		if !ok {
			panic("KeyError")
		}
		value, found := evidence.Lookup(n, name)
		if !found {
			panic("KeyError: " + name)
		}
		return value
	case []any:
		return n[forgeListIndex(n, key)]
	}
	panic("TypeError: object is not subscriptable")
}

func forgeListIndex(list []any, key any) int {
	number, ok := key.(json.Number)
	if !ok {
		panic("TypeError: list indices must be integers or slices")
	}
	i, err := strconv.Atoi(number.String())
	if err != nil {
		panic(err)
	}
	if i < 0 {
		i += len(list)
	}
	if i < 0 || i >= len(list) {
		panic("IndexError: list index out of range")
	}
	return i
}
