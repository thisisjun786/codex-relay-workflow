package faults

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestF1ClaimCLIOracle(t *testing.T) {
	goldenParent(t)
	cases := [][][]string{
		{{"fault-claim", "--publication", "missing", "--owner", "operator"}},
		{{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"delivery_stalled","severity":"broken","signature":{"recipient":"a"},"occurrenceKey":"o","scope":{},"detail":"d","evidence":[],"cleared":false}`}, {"fault-claim", "--publication", "bad", "--owner", "operator"}},
	}
	id := publicationID(FaultID("crw", "delivery_stalled", map[string]any{"recipient": "a"}), openRecord, triggerOpen)
	cases = append(cases, [][]string{{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"delivery_stalled","severity":"broken","signature":{"recipient":"a"},"occurrenceKey":"o","scope":{},"detail":"d","evidence":[],"cleared":false}`}, {"fault-claim", "--publication", id, "--owner", "operator"}})
	cases = append(cases, [][]string{{"fault-operation", "--publication", "missing", "--claim-token", "bad"}, {"fault-reconcile", "--publication", "missing"}, {"fault-complete", "--publication", "missing"}, {"fault-sweep"}, {"fault-sweep", "--readings-after", "-1"}, {"fault-sweep", "--readings", `{"wrong":1}`}})
	cases = append(cases, [][]string{{"fault-target", "--product", "crw", "--team", "team", "--project-ref", "PROJECT"}, {"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"delivery_stalled","severity":"broken","signature":{"recipient":"a"},"occurrenceKey":"o","scope":{},"detail":"d","evidence":[],"cleared":false}`}, {"fault-claim", "--publication", id, "--owner", "operator"}, {"fault-operation", "--publication", id, "--claim-token", "stale"}, {"fault-reconcile", "--publication", id, "--searched"}, {"fault-complete", "--publication", id, "--claim-token", "stale"}})
	cases = append(cases, [][]string{{"fault-sweep", "--readings", `[{"schema":"reporting-observation/1","relationshipId":"r","selectors":{"turn":"t"},"reportingState":"in_progress"}]`}, {"fault-sweep", "--readings", `[{"schema":"bad"}]`}})
	cases = append(cases, [][]string{{"fault-sweep", "--readings", `[{"schema":"reporting-observation/1","relationshipId":"rel-0","selectors":{"turn":"turn-1"},"reportingState":"unreported"},{"schema":"reporting-observation/1","relationshipId":"rel-1","selectors":{"turn":"turn-1"},"reportingState":"unreported"},{"schema":"reporting-observation/1","relationshipId":"rel-2","selectors":{"turn":"turn-1"},"reportingState":"unreported"}]`, "--readings-after", "2"}})
	cases = append(cases, [][]string{{"fault-sweep", "--readings", `[{"schema":"reporting-observation/1","relationshipId":"rel-1","selectors":{"turn":"turn-1"},"reportingState":"unreported"}]`}})
	cases = append(cases, [][]string{{"fault-sweep", "--readings-after", "oops"}, {"fault-sweep", "--product", "bad:product"}, {"fault-sweep", "--readings", "not-json"}})
	cases = append(cases, [][]string{{"fault-target", "--product", "crw", "--team", "team", "--project-ref", "P"}, {"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"delivery_stalled","severity":"broken","signature":{"recipient":"a"},"occurrenceKey":"o","scope":{},"detail":"d","evidence":[],"cleared":false}`}, {"fault-claim", "--publication", id, "--owner", "operator"}, {"fault-reconcile", "--publication", id, "--observed-fields", "not-json"}, {"fault-complete", "--publication", id, "--observed-fields", "not-json"}})
	for n, steps := range cases {
		t.Run(string(rune('a'+n)), func(t *testing.T) {
			home, err := os.MkdirTemp("/dev/shm", "f1-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			for key, value := range map[string]string{"HOME": home, "XDG_STATE_HOME": home + "/xs", "XDG_CONFIG_HOME": home + "/xc", "CODEX_HOME": home + "/ch"} {
				t.Setenv(key, value)
			}
			for _, args := range steps {
				var got, stderr bytes.Buffer
				ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{clock: &testClock{now: 100000}, entropy: bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7})})
				exit, handled := executeAsCLI(ctx, append([]string{"--state", home + "/go", "--json"}, args...), &got, &stderr)
				if !handled {
					t.Fatal("unhandled")
				}
				checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: exit, Stdout: got.String()})
			}
		})
	}
}
