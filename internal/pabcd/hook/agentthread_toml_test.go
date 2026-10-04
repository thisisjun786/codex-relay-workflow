package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

type tomlOracleOp struct {
	Op, Scope, Save string
	Parts           []string
	Array           bool
}

type tomlOracleCase struct {
	Unit, Input, Error string
	Start              int
	Ops                []tomlOracleOp
	Answer             json.RawMessage
}

func TestAgentThreadTomlOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/agentthread-toml-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []tomlOracleCase
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("empty oracle corpus")
	}
	for i, c := range corpus.Cases {
		t.Run(fmt.Sprintf("%s/%03d", c.Unit, i), func(t *testing.T) {
			var got any
			switch c.Unit {
			case "finiteNumber":
				value, err := finiteTomlNumber(c.Input)
				if (err != nil) != (c.Error != "") {
					t.Fatalf("%q: error %v, oracle error %q", c.Input, err, c.Error)
				}
				if c.Error != "" {
					return // Go error text replaces V8's syntax diagnostic.
				}
				got = value
			case "validDateTime":
				got = validTomlDateTime(c.Input)
			case "validValue":
				got = validTomlValue(c.Input)
			case "keyPath":
				p := tomlKeyPath(c.Input, c.Start)
				if p != nil {
					got = map[string]any{"parts": p.parts, "end": p.end}
				}
			case "structure":
				root := newTomlTable()
				current := root
				scopes := map[string]*tomlEntry{"root": root}
				answers := []any{tomlStepSnapshot(true, root, current)}
				for _, op := range c.Ops {
					var ok bool
					if op.Op == "assign" {
						scope := current
						if op.Scope != "" {
							scope = scopes[op.Scope]
						}
						ok = tomlAssignKey(scope, op.Parts)
					} else {
						next := tomlEnterTable(root, op.Parts, op.Array)
						ok = next != nil
						if ok {
							current = next
							if op.Save != "" {
								scopes[op.Save] = next
							}
						}
					}
					answers = append(answers, tomlStepSnapshot(ok, root, current))
				}
				got = answers
			default:
				t.Fatalf("unknown oracle unit %q", c.Unit)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if err := json.Unmarshal(encoded, &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Answer, &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Errorf("%q: got %s, oracle %s", c.Input, encoded, c.Answer)
			}
		})
	}
}

func tomlStepSnapshot(ok bool, root, current *tomlEntry) any {
	return map[string]any{"ok": ok, "root": tomlSnapshot(root), "current": tomlSnapshot(current)}
}

func tomlSnapshot(entry *tomlEntry) any {
	out := map[string]any{"kind": entry.kind}
	if entry.children != nil {
		children := make(map[string]any, len(entry.children))
		for key, child := range entry.children {
			children[key] = tomlSnapshot(child)
		}
		out["children"] = children
	}
	if entry.declared {
		out["declared"] = true
	}
	if entry.latest != nil {
		out["latest"] = tomlSnapshot(entry.latest)
	}
	return out
}
