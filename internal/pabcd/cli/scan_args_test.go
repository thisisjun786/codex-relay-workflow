package cli

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestScanArgsNodeOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/scan_args/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			ID           string
			Argv         []string
			Cwd          string
			Want         json.RawMessage
			NegativeZero map[string]bool
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("oracle contains no parser cases")
	}
	for _, c := range fixture.Cases {
		t.Run(c.ID, func(t *testing.T) {
			before := append([]string{}, c.Argv...)
			p := ParseScanCliArgs(c.Argv, c.Cwd)
			// CRW-871: a recorded case whose --session value is not canonical (the value flag consumes
			// the next token, even a flag, and the sanitiser would map the id to a different session's
			// file). The oracle accepted it; the port refuses it before the runner reads or writes.
			if want, changed := sessionAliasScanArgsChanged[c.ID]; changed {
				if p.Args != nil || p.Error != want {
					t.Fatalf("changed case %s: %+v, want error %q", c.ID, p, want)
				}
				if !reflect.DeepEqual(before, c.Argv) {
					t.Error("parser changed argv")
				}
				return
			}
			if (p.Args != nil) == (p.Error != "") {
				t.Fatalf("expected exactly one parser outcome: %+v", p)
			}
			var observed any = p.Args
			if p.Error != "" {
				observed = map[string]string{"error": p.Error}
			}
			gotJSON, err := json.Marshal(observed)
			if err != nil {
				t.Fatal(err)
			}
			// Compare complete JSON shapes, not just typed fields: omitted fields matter.
			// JSON.stringify emits 0 for -0; sign is checked separately below.
			var got, want any
			if err := json.Unmarshal(gotJSON, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Want, &want); err != nil {
				t.Fatal(err)
			}
			if obj, ok := want.(map[string]any); ok {
				if message, ok := obj["error"].(string); ok {
					obj["error"] = strings.NewReplacer("cxc scan", "crw pabcd scan", "cxc orchestrate", "crw pabcd orchestrate").Replace(message)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("argv %q: got %s, want %#v", c.Argv, gotJSON, want)
			}
			if p.Args != nil {
				a := p.Args
				values := map[string]float64{"contradictionCount": a.ContradictionCount, "highContradictionCount": a.HighContradictionCount}
				for dim, value := range a.Confidence {
					values["confidence."+string(dim)] = value
				}
				for key, value := range values {
					if negative := value == 0 && math.Signbit(value); negative != c.NegativeZero[key] {
						t.Errorf("%s negative zero = %v, want %v", key, negative, c.NegativeZero[key])
					}
				}
			}
			if !reflect.DeepEqual(before, c.Argv) {
				t.Error("parser changed argv")
			}
		})
	}
}

// Ported B-class assertions from scan-cli.test.ts:27-70,179-198,340-357.
// sessionAliasScanArgsChanged lists the recorded scan-args cases the port answers differently on purpose
// (CRW-871): the oracle's parser accepted a non-canonical --session value, which the runner then
// sanitised into a DIFFERENT session's key, so scan record rewrote another session's state file.
var sessionAliasScanArgsChanged = map[string]string{
	"session_consumes_flag": "scan record: " + sessionAliasRefusalText,
	"flag_value_session":    "scan record: " + sessionAliasRefusalText,
	"whitespace_session":    "scan record: " + sessionAliasRefusalText,
}

func TestScanArgsParserCases(t *testing.T) {
	for _, c := range []struct {
		argv []string
		text string
	}{
		{[]string{"record"}, "--session <id> is required"},
		{[]string{"evidence"}, "unknown scan action 'evidence'"},
		{nil, "unknown scan action"},
		{[]string{"record", "--session", "s", "--nope"}, "unknown argument '--nope'"},
		{[]string{"record", "--session", "s", "--contradictions", "-1"}, "--contradictions must be a non-negative integer"},
		{[]string{"record", "--session", "s", "--high", "-2"}, "--high must be a non-negative integer"},
		{[]string{"record", "--session", "s", "--contradictions", "abc"}, "--contradictions must be a non-negative integer"},
		{[]string{"record", "--session", "s", "--dim", "nope=high"}, "unknown dimension 'nope'"},
		{[]string{"record", "--session", "s", "--dim", "goal=enormous"}, "invalid level 'enormous'"},
		{[]string{"record", "--session", "s", "--dim", "goal"}, "--dim expects"},
		{[]string{"record", "--session", "s", "--confidence", "goal=7"}, "within [0,1]"},
		{[]string{"record", "--session", "s", "--known", "goal="}, "must not be empty"},
		{[]string{"record", "--session", "s", "--map", "q1=bogus"}, "unknown dimension 'bogus'"},
	} {
		p := ParseScanCliArgs(c.argv, "/ws")
		if p.Args != nil || !strings.Contains(p.Error, c.text) {
			t.Errorf("%q: %+v, want error containing %q", c.argv, p, c.text)
		}
	}
	p := ParseScanCliArgs([]string{"record", "--session", "s1"}, "/some/cwd")
	want := ScanCliArgs{Action: ScanActionRecord, SessionID: "s1", Cwd: "/some/cwd"}
	if p.Error != "" || p.Args == nil || !reflect.DeepEqual(*p.Args, want) {
		t.Fatalf("defaults: %+v, want %+v", p, want)
	}
	for _, level := range []string{"low", "mid", "high"} {
		if p := ParseScanCliArgs([]string{"record", "--session", "s", "--dim", "goal=" + level}, "/ws"); p.Args == nil || p.Error != "" {
			t.Errorf("level %s rejected: %+v", level, p)
		}
	}
	p = ParseScanCliArgs([]string{"record", "--session", "s", "--dim", "goal=max"}, "/ws")
	if !strings.Contains(p.Error, "cannot set 'max'") || !strings.Contains(p.Error, `"override":true`) {
		t.Errorf("max must name the attested override: %+v", p)
	}
	for _, value := range []string{"0.5abc", "abc", "", " ", "1.5", "-0.1"} {
		p := ParseScanCliArgs([]string{"record", "--session", "s", "--confidence", "goal=" + value}, "/ws")
		if p.Args != nil || p.Error == "" {
			t.Errorf("confidence %q accepted: %+v", value, p)
		}
	}
}
