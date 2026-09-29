package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Every reason rolepolicy.worker_readiness gives, against Python's answer for the same
// observation, requirement list and caller policy (the "readiness" cases of
// ../service/testdata/worker_reasons.json, captured once by worker_reasons_capture.py). The
// observation readers' reasons are pinned beside it in package service.
func TestWorkerReadiness_every_reason_is_pythons(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "service", "testdata", "worker_reasons.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Readiness map[string]*string `json:"readiness"`
		Policy    json.RawMessage    `json:"policy"`
	}
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	policyFile, parentOnly := filepath.Join(dir, "policy.json"), filepath.Join(dir, "parent-only.json")
	var policy map[string]map[string]any
	if err = json.Unmarshal(golden.Policy, &policy); err != nil {
		t.Fatal(err)
	}
	parent, _ := json.Marshal(map[string]any{"roles": map[string]any{"parent": policy["roles"]["parent"]}})
	for path, content := range map[string][]byte{policyFile: golden.Policy, parentOnly: parent} {
		if err = os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	summary := func(file string) contract.OrderedObject {
		return registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: file}).Summary()
	}
	observation := func(policy any) contract.OrderedObject {
		return contract.OrderedObject{{Key: "observed", Value: true}, {Key: "reason", Value: nil}, {Key: "policy", Value: policy}}
	}
	// withPolicy is the valid observation with one change to the worker's public policy.
	withPolicy := func(change func(contract.OrderedObject) contract.OrderedObject) contract.OrderedObject {
		return observation(change(summary(policyFile)))
	}
	set := func(o contract.OrderedObject, key string, value any) contract.OrderedObject {
		out := append(contract.OrderedObject{}, o...)
		for i := range out {
			if out[i].Key == key {
				out[i].Value = value
			}
		}
		return out
	}
	get := func(o contract.OrderedObject, key string) any {
		for _, field := range o {
			if field.Key == key {
				return field.Value
			}
		}
		return nil
	}
	requirement := func(text string) any {
		value, err := store.LoadsJSON([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	ready := `{"role": "parent", "model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}`
	child := `{"role": "child", "model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}`
	with := func(key, value string) string {
		return strings.Replace(ready, `}`, `, "`+key+`": `+value+`}`, 1)
	}
	valid := observation(summary(policyFile))
	type input struct {
		observation  contract.OrderedObject
		requirements string
		policyFile   string
	}
	cases := map[string]input{
		"ready":                    {valid, "[" + ready + "]", policyFile},
		"ready-both":               {valid, "[" + ready + ", " + child + "]", policyFile},
		"requirements-empty":       {valid, "[]", policyFile},
		"requirements-object":      {valid, `{"role": "parent"}`, policyFile},
		"requirements-null-item":   {valid, "[null]", policyFile},
		"requirements-model-false": {valid, `[` + strings.Replace(ready, `"anthropic/claude-opus-5-5"`, "false", 1) + `]`, policyFile},
		"requirements-model-blank": {valid, `[` + strings.Replace(ready, `"anthropic/claude-opus-5-5"`, `"  "`, 1) + `]`, policyFile},
		// str.strip() also strips the information separators U+001C..U+001F.
		"requirements-model-separator": {valid, `[` + strings.Replace(ready, `"anthropic/claude-opus-5-5"`, `"\u001f"`, 1) + `]`, policyFile},
		"requirements-extra-key":       {valid, "[" + with("exception", `"x"`) + "]", policyFile},
		"role-supervisor":              {valid, "[" + strings.Replace(ready, `"parent"`, `"supervisor"`, 1) + "]", policyFile},
		"role-undeclared":              {observation(summary(parentOnly)), "[" + child + "]", parentOnly},
		"unobserved":                   {contract.OrderedObject{{Key: "observed", Value: false}, {Key: "reason", Value: nil}, {Key: "policy", Value: nil}}, "[" + ready + "]", policyFile},
		"unobserved-reason":            {contract.OrderedObject{{Key: "observed", Value: false}, {Key: "reason", Value: "worker_policy_lock_unheld"}, {Key: "policy", Value: nil}}, "[" + ready + "]", policyFile},
		"worker-unresolved":            {withPolicy(func(p contract.OrderedObject) contract.OrderedObject { return set(p, "state", "unresolved") }), "[" + ready + "]", policyFile},
		"caller-unresolved":            {valid, "[" + ready + "]", ""},
		"digest-mismatch": {withPolicy(func(p contract.OrderedObject) contract.OrderedObject {
			return set(p, "digest", strings.Repeat("0", 64))
		}), "[" + ready + "]", policyFile},
		"summary-mismatch": {withPolicy(func(p contract.OrderedObject) contract.OrderedObject {
			roles := get(p, "roles").(contract.OrderedObject)
			parent := set(get(roles, "parent").(contract.OrderedObject), "model", "not-the-declared-model")
			return set(p, "roles", set(roles, "parent", parent))
		}), "[" + ready + "]", policyFile},
		"pair-mismatch-model":  {valid, "[" + strings.Replace(ready, `"anthropic/claude-opus-5-5"`, `"other-model"`, 1) + "]", policyFile},
		"pair-mismatch-effort": {valid, "[" + strings.Replace(ready, `"xhigh"`, `"low"`, 1) + "]", policyFile},
	}
	if len(cases) != len(golden.Readiness) {
		t.Fatalf("%d Go cases, %d Python cases", len(cases), len(golden.Readiness))
	}
	digest := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: policyFile}).Digest()
	for name, want := range golden.Readiness {
		c, ok := cases[name]
		if !ok {
			t.Fatalf("no Go case for Python's %q", name)
		}
		answer := cli.WorkerReadiness(c.observation, requirement(c.requirements), c.policyFile)
		reason, _ := get(answer, "reason").(string)
		switch {
		case want == nil && (reason != "" || get(answer, "ready") != true || get(answer, "digest") != digest):
			t.Errorf("%s: %v, Python ready", name, answer)
		case want != nil && (reason != *want || get(answer, "ready") != false || get(answer, "digest") != nil):
			t.Errorf("%s: %v, Python %s", name, answer, *want)
		}
	}
}
