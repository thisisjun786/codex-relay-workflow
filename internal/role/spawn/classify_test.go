package spawn

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// testdata/classify/oracle.json holds what the CXC v0.2.40 source answered, recorded under Node
// 24 by testdata/classify/record-classify.mjs; no Node runs here. For each case, input is the
// CRW-named case this test replays, oracle is the raw answer the CXC-named case received, and
// expected is the renamed answer the Go port must produce. Every case is classified identical or
// intentionally-changed with the rename reason, exactly like the other oracle fixtures of this
// package.

type spawnClassifyCase struct {
	Input          json.RawMessage `json:"input"`
	Oracle         json.RawMessage `json:"oracle"`
	Expected       json.RawMessage `json:"expected"`
	Classification string          `json:"classification"`
	Reason         string          `json:"reason"`
	Test           string          `json:"test"`
}

type spawnClassifySection struct {
	Fn    string              `json:"fn"`
	Cases []spawnClassifyCase `json:"cases"`
}

type spawnClassifyFixture struct {
	Oracle   string                 `json:"oracle"`
	Source   string                 `json:"source"`
	Tests    string                 `json:"tests"`
	Sections []spawnClassifySection `json:"sections"`
}

func TestSpawnClassifyOracleReplay(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "classify", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture spawnClassifyFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Sections) != 10 {
		t.Fatalf("fixture has %d sections, want 10", len(fixture.Sections))
	}
	seen := map[string]bool{}
	total := 0
	for _, section := range fixture.Sections {
		if seen[section.Fn] {
			t.Fatalf("duplicate section %s", section.Fn)
		}
		seen[section.Fn] = true
		if len(section.Cases) == 0 {
			t.Fatalf("section %s has no cases", section.Fn)
		}
		t.Run(section.Fn, func(t *testing.T) {
			for i, c := range section.Cases {
				if c.Classification != "identical" && (c.Classification != "intentionally-changed" || c.Reason == "") {
					t.Fatalf("case %d (%s) is unclassified", i+1, c.Test)
				}
				got, want := spawnClassifyReplay(t, section.Fn, c)
				if got != want {
					t.Errorf("case %d (%s): got %q, want %q", i+1, c.Test, got, want)
				}
				total++
			}
		})
	}
	if total == 0 {
		t.Fatal("no cases replayed")
	}
}

// spawnClassifyScalar reads an expected value as the text the replay compares: a string as it
// is, a boolean as true or false, and null as null.
func spawnClassifyScalar(raw json.RawMessage) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	switch v := v.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}

func spawnClassifyReplay(t *testing.T, fn string, c spawnClassifyCase) (string, string) {
	t.Helper()
	want, ok := spawnClassifyScalar(c.Expected)
	if !ok {
		t.Fatalf("case (%s): expected is not a scalar", c.Test)
	}
	switch fn {
	case "SubspawnToken":
		return SubspawnToken, want
	case "RecurseDenyReason":
		return RecurseDenyReason, want
	case "StripControlMarkers":
		var in struct {
			Message            string `json:"message"`
			PreserveWhitespace bool   `json:"preserveWhitespace"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			t.Fatal(err)
		}
		return StripControlMarkers(in.Message, in.PreserveWhitespace), want
	case "DenyEnvelope":
		var in struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			t.Fatal(err)
		}
		return DenyEnvelope(in.Reason), want
	case "InferRole":
		var in struct {
			AgentType any    `json:"agentType"`
			Message   string `json:"message"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			t.Fatal(err)
		}
		return string(InferRole(in.AgentType, in.Message)), want
	case "IsV2SpawnInput", "IsFullHistoryFork":
		var envelope struct {
			ToolInput json.RawMessage `json:"toolInput"`
		}
		if err := json.Unmarshal(c.Input, &envelope); err != nil {
			t.Fatal(err)
		}
		var toolInput map[string]any
		if err := json.Unmarshal(envelope.ToolInput, &toolInput); err != nil {
			t.Fatal(err)
		}
		if fn == "IsV2SpawnInput" {
			return strconv.FormatBool(IsV2SpawnInput(toolInput)), want
		}
		got := IsFullHistoryFork(toolInput)
		// CRW-613 may decode with or without UseNumber; the number branch must accept both.
		numbered := map[string]any{}
		dec := json.NewDecoder(bytes.NewReader(envelope.ToolInput))
		dec.UseNumber()
		if err := dec.Decode(&numbered); err != nil {
			t.Fatal(err)
		}
		if got != IsFullHistoryFork(numbered) {
			t.Errorf("case (%s): json.Number decode answers %v, float64 decode answers %v",
				c.Test, IsFullHistoryFork(numbered), got)
		}
		return strconv.FormatBool(got), want
	case "IsFernetTokenShape":
		var in struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			t.Fatal(err)
		}
		return strconv.FormatBool(IsFernetTokenShape(in.Token)), want
	case "IsSpawnToolName", "IsCollaborationToolName":
		var in struct {
			Name any `json:"name"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			t.Fatal(err)
		}
		if fn == "IsSpawnToolName" {
			return strconv.FormatBool(IsSpawnToolName(in.Name)), want
		}
		return strconv.FormatBool(IsCollaborationToolName(in.Name)), want
	}
	t.Fatalf("unknown fixture section %s", fn)
	return "", ""
}

// TestSpawnClassifyGrantPattern pins the exported pattern the sibling grant issue reuses: it
// matches the canonical, lowercase, mixed-case and uppercase-hex markers the oracle strips, and
// it must not match the U+017F spelling, which the oracle's /i leaves alone while Go's (?i)
// would fold with s.
func TestSpawnClassifyGrantPattern(t *testing.T) {
	re, err := regexp.Compile(SubspawnGrantPattern)
	if err != nil {
		t.Fatalf("SubspawnGrantPattern does not compile: %v", err)
	}
	hex := strings.Repeat("a1b2c3d4", 8)
	for _, marker := range []string{
		"[CRW-SUBSPAWN-GRANT:" + hex + "]",
		"[crw-subspawn-grant:" + hex + "]",
		"[CrW-SuBsPaWn-GrAnT:" + hex + "]",
		"[CRW-SUBSPAWN-GRANT:" + strings.ToUpper(hex) + "]",
	} {
		if !re.MatchString(marker) {
			t.Errorf("pattern does not match %q", marker)
		}
	}
	if re.MatchString("[CRW-ſUBSPAWN-GRANT:" + hex + "]") {
		t.Error("pattern folds U+017F with s, which the oracle's /i does not")
	}
	if re.MatchString("[CRW-SUBSPAWN-GRANT:" + hex[:63] + "]") {
		t.Error("pattern matches a 63-character nonce")
	}
}

// TestSpawnClassifyDenyReasonInvariants is the upstream assertion that the deny reason never
// spells out the literal token, kept here on the constant itself.
func TestSpawnClassifyDenyReasonInvariants(t *testing.T) {
	if strings.Contains(RecurseDenyReason, SubspawnToken) {
		t.Error("RecurseDenyReason contains the literal token name")
	}
	for _, want := range []string{"LEAF-TOPOLOGY-01", "recursion grant token"} {
		if !strings.Contains(RecurseDenyReason, want) {
			t.Errorf("RecurseDenyReason does not mention %q", want)
		}
	}
}

// TestSpawnClassifyDenyEnvelopeShape reads the envelope the way the host parser does, separate
// from the byte comparison of the recorded cases.
func TestSpawnClassifyDenyEnvelopeShape(t *testing.T) {
	out := DenyEnvelope("why")
	if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
		t.Fatalf("envelope must end with exactly one newline: %q", out)
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	h := parsed.HookSpecificOutput
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "deny" || h.PermissionDecisionReason != "why" {
		t.Fatalf("envelope fields are wrong: %+v", h)
	}
}
