package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// TestOracleCorpus replays testdata/cases.json against the answers the CXC v0.2.40 oracle recorded in testdata/oracle-gate.json
// (record-oracle.mjs; no Node at test time), with the port's names ({S} is the state directory). Text from Node or V8 was recorded as
// <JSON> or <ERR> after its fixed words and is matched up to those.

type oracleCase struct {
	Kind    ReceiptKind         `json:"kind"`
	Epoch   *string             `json:"epoch"`
	Session string              `json:"session"`
	Setup   []map[string]string `json:"setup"` // {file, text}, {dir} or {symlink, to}
	Claim   string              `json:"claim"`
}

// sameError is got against the recorded message, which may end in a Node text mask.
func sameError(want, got string) bool {
	for _, mask := range []string{"<JSON>", "<ERR>"} {
		if prefix, rest, masked := strings.Cut(want, mask); masked {
			return strings.HasPrefix(got, prefix) && strings.HasSuffix(got, rest)
		}
	}
	return got == want
}

func TestOracleCorpus(t *testing.T) {
	base := hermetic(t)
	var cases, golden struct{ Receipt, Gate []json.RawMessage }
	for name, into := range map[string]any{"cases.json": &cases, "oracle-gate.json": &golden} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		must(t, err)
		must(t, json.Unmarshal(data, into))
	}
	// replay runs each case in a fresh workspace and hands check the case and its recorded answer, read with the workspace's names.
	replay := func(group string, raws, answers []json.RawMessage, check func(*testing.T, oracleCase, map[string]any, string)) {
		for i, raw := range raws {
			var named struct{ ID string }
			must(t, json.Unmarshal(raw, &named))
			t.Run(group+"/"+named.ID, func(t *testing.T) {
				ws, err := os.MkdirTemp(base, "c-")
				must(t, err)
				names := strings.NewReplacer("{S}", stateDir, "<CWD>", ws, "cxc receipt test", "crw pabcd receipt test", "cxc orchestrate", "crw pabcd orchestrate")
				var c oracleCase
				var want map[string]any
				must(t, json.Unmarshal([]byte(names.Replace(string(raw))), &c))
				must(t, json.Unmarshal([]byte(names.Replace(string(answers[i]))), &want))
				must(t, os.MkdirAll(filepath.Join(ws, stateDir, "evidence"), 0o755))
				for _, s := range c.Setup {
					switch {
					case s["dir"] != "":
						must(t, os.MkdirAll(filepath.Join(ws, s["dir"]), 0o755))
					case s["symlink"] != "":
						must(t, os.MkdirAll(filepath.Dir(filepath.Join(ws, s["symlink"])), 0o755))
						must(t, os.Symlink(s["to"], filepath.Join(ws, s["symlink"])))
					default:
						writeFile(t, filepath.Join(ws, s["file"]), s["text"])
					}
				}
				check(t, c, want, ws)
			})
		}
	}
	replay("receipt", cases.Receipt, golden.Receipt, func(t *testing.T, c oracleCase, want map[string]any, ws string) {
		got, err := ParseSourceBoundReceipt(c.Claim, ws, c.Kind)
		if message, refused := want["error"].(string); refused {
			if err == nil || !sameError(message, err.Error()) {
				t.Fatalf("got %+v, %v; the oracle refused with %q", got, err, message)
			}
			return
		}
		encoded, marshalErr := json.Marshal(got)
		var decoded any
		must(t, marshalErr)
		must(t, json.Unmarshal(encoded, &decoded))
		if err != nil || !reflect.DeepEqual(decoded, want["receipt"]) {
			t.Fatalf("got %s, %v; the oracle answered %v", encoded, err, want["receipt"])
		}
	})
	replay("gate", cases.Gate, golden.Gate, func(t *testing.T, c oracleCase, want map[string]any, ws string) {
		got := ValidateCheckReceipt(state.State{Phase: state.PhaseC, CheckEpoch: c.Epoch}, c.Session, c.Claim, ws)
		wantOK, _ := want["ok"].(bool)
		wantReason, _ := want["reason"].(string)
		if got.OK != wantOK || got.Reason != wantReason {
			t.Fatalf("got %+v; the oracle answered %v", got, want)
		}
	})
}
