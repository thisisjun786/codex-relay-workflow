package spawn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// testdata/hook/oracle.json holds, besides the cases of hook_test.go, what the CXC v0.2.40 hook printed end to end ("route") and its
// denyEnvelope for reasons with lone surrogates ("deny"), recorded by testdata/hook/record-hook.mjs. A route step stores the CRW-named
// stdin text and the oracle's raw answer, renamed, with the guard blocks, scratch paths and grant nonces as placeholders. A large answer
// is stored as the sha256 and size of that text with the blocks written out. {{FILL:unit:n}} stands for unit repeated n times.

type spawnRouteStep struct {
	Note         string `json:"note"`
	Stdin        string `json:"stdin"`
	Expect       string `json:"expect"`
	ExpectSha256 string `json:"expectSha256"`
	ExpectBytes  int    `json:"expectBytes"`
	Class        string `json:"classification"`
	Reason       string `json:"reason"`
	Twin         *struct {
		Project      json.RawMessage `json:"project"`
		OracleWarned string          `json:"oracleWarned"`
	} `json:"twin"`
}

type spawnRouteFile struct {
	Route []struct {
		Steps []spawnRouteStep `json:"steps"`
	} `json:"route"`
	Deny []struct {
		Reason string `json:"reason"`
		Expect string `json:"expect"`
	} `json:"deny"`
	ItemsCap struct {
		MaxUnits int `json:"maxUnits"`
		Cases    []struct {
			Delta    int  `json:"delta"`
			Appended bool `json:"appended"`
		} `json:"cases"`
	} `json:"itemsCap"`
}

// spawnRouteRead reads the route and deny sections, and the environment of each route case through hook_test.go's case type.
func spawnRouteRead(t *testing.T) (spawnRouteFile, []spawnHookCase) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "hook", "oracle.json"))
	spawnHookMust(t, err)
	var steps spawnRouteFile
	var envs struct {
		Route []spawnHookCase `json:"route"`
	}
	spawnHookMust(t, json.Unmarshal(data, &steps))
	spawnHookMust(t, json.Unmarshal(data, &envs))
	if len(steps.Route) == 0 || len(steps.Route) != len(envs.Route) || len(steps.Deny) == 0 {
		t.Fatal("no recorded route cases")
	}
	return steps, envs.Route
}

// expandRaw fills a recorded text in: the guard blocks as JSON.stringify writes them, the scratch paths, the nonces minted so far and
// the {{FILL}} repeats.
func (r *spawnHookRig) expandRaw(s string) string {
	for token, block := range r.blocks {
		quoted := spawnHookRouteStringify(block)
		s = strings.ReplaceAll(s, token, quoted[1:len(quoted)-1])
	}
	s = strings.NewReplacer("{SKILLS}", r.skills, "{WS}", r.ws).Replace(s)
	for i, nonce := range r.nonces {
		s = strings.ReplaceAll(s, "{N"+strconv.Itoa(i+1)+"}", nonce)
	}
	fill := regexp.MustCompile(`\{\{FILL:([^:}]*):(\d+)\}\}`)
	return fill.ReplaceAllStringFunc(s, func(m string) string {
		parts := fill.FindStringSubmatch(m)
		n, _ := strconv.Atoi(parts[2])
		return strings.Repeat(parts[1], n)
	})
}

// plain is an answer as it was hashed: scratch paths and nonces back to placeholders.
func (r *spawnHookRig) plain(s string) string {
	s = strings.NewReplacer(r.skills, "{SKILLS}", r.ws, "{WS}").Replace(s)
	for i, nonce := range r.nonces {
		s = strings.ReplaceAll(s, nonce, "{N"+strconv.Itoa(i+1)+"}")
	}
	return s
}

func TestSpawnHookRouteOracleReplay(t *testing.T) {
	steps, envs := spawnRouteRead(t)
	fixture := spawnHookReadFixture(t)
	nonce := regexp.MustCompile(`\[CRW-SUBSPAWN-GRANT:([a-f0-9]{64})\]`)
	total := 0
	for ci, c := range steps.Route {
		t.Run(envs[ci].Name, func(t *testing.T) {
			rig := spawnHookNewRig(t, fixture.Skills, envs[ci])
			for i, step := range c.Steps {
				total++
				at := "step " + strconv.Itoa(i+1) + " (" + step.Note + ")"
				if step.Class != "identical" && (step.Class != "intentionally-changed" || step.Reason == "") {
					t.Fatalf("%s is unclassified", at)
				}
				if step.Twin != nil { // the oracle warned about this project config; the port has no project layer and must ignore it
					spawnHookMust(t, os.MkdirAll(filepath.Join(rig.ws, ".crw"), 0o755))
					spawnHookMust(t, os.WriteFile(filepath.Join(rig.ws, ".crw", "subagents.json"), []byte(`{"roles":`+string(step.Twin.Project)+`}`), 0o644))
				}
				got := RunSpawnAttachHook(rig.expandRaw(step.Stdin), rig.env)
				for _, m := range nonce.FindAllStringSubmatch(got, -1) {
					if !slices.Contains(rig.nonces, m[1]) {
						rig.nonces = append(rig.nonces, m[1])
					}
				}
				if step.ExpectSha256 != "" {
					plain := rig.plain(got)
					sum := sha256.Sum256([]byte(plain))
					if hex.EncodeToString(sum[:]) != step.ExpectSha256 || len(plain) != step.ExpectBytes {
						t.Fatalf("%s: answer sha256 %x (%d bytes), want %s (%d bytes)", at, sum, len(plain), step.ExpectSha256, step.ExpectBytes)
					}
				} else if want := rig.expandRaw(step.Expect); got != want {
					t.Fatalf("%s:\n got %q\nwant %q", at, got, want)
				}
			}
		})
	}
	t.Logf("replayed %d recorded route steps", total)
}

// A deny envelope keeps a lone surrogate as its escape, as JSON.stringify writes it, never as U+FFFD (c9).
func TestSpawnHookDenyEnvelopeSurrogates(t *testing.T) {
	steps, _ := spawnRouteRead(t)
	for _, d := range steps.Deny {
		reason, err := pyjson.Loads(d.Reason, pyjson.LoadOptions{Surrogates: true})
		spawnHookMust(t, err)
		if got := DenyEnvelope(reason.(string)); got != d.Expect {
			t.Errorf("deny of %s: %q, want %q", d.Reason, got, d.Expect)
		}
	}
}

// The oracle's empty-guard branches (:1030-1046) cannot be reached through the hook, and String.replace expands $ patterns and finds
// nothing in a message that lacks the guard: these expectations are read off the oracle's code, not recorded.
func TestSpawnHookRoutePromptBranches(t *testing.T) {
	for _, tc := range []struct {
		name, message, guard, prompt string
		items, v2                    bool
		want                         string
	}{
		{"items guard only", "G", "G", "P", true, false, "G\n\nP"},
		{"replace inserts at the first guard only", "G\n\nT\n\nG\n\nU", "G", "P", false, false, "G\n\nP\n\nT\n\nG\n\nU"},
		{"a message that is only the guard finds nothing", "G", "G", "P", false, false, "G"},
		{"empty guard: after the marker's block", "x [CRW-SUBAGENT-SCOPE] a\n\nT", "", "P", false, false, "x [CRW-SUBAGENT-SCOPE] a\n\nP\n\nT"},
		{"empty guard: v2 marker, no blank line after it", "[CRW-LEAF-GUARD] a", "", "P", false, true, "[CRW-LEAF-GUARD] a\n\nP"},
		{"empty guard: no marker", "T", "", "P", false, false, "P\n\nT"},
		{"empty guard: a v1 marker does not serve v2", "[CRW-SUBAGENT-SCOPE] a\n\nT", "", "P", false, true, "P\n\n[CRW-SUBAGENT-SCOPE] a\n\nT"},
		{"$ patterns and a trailing $", "G\n\nT", "G", "$$ $& $' $` $1 $", false, false, "G\n\n$ G\n\n T  $1 $\n\nT"},
	} {
		if got := spawnHookRoutePrompt(tc.message, tc.guard, tc.prompt, tc.items, tc.v2); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The trust prefix cannot arise through the hook (no project layer: hook.go leaves it empty). The rule is the oracle's :1049, and the
// expected answer is the oracle's own with-warning answer, recorded beside the global-only one (the ciphertext notice also carries the
// warning, which SpawnResolution has no field for, so only the plain message is replayed this way).
func TestSpawnHookRouteTrustPrefix(t *testing.T) {
	steps, envs := spawnRouteRead(t)
	fixture := spawnHookReadFixture(t)
	for ci, c := range steps.Route {
		if envs[ci].Name != "route: trust warning" {
			continue
		}
		rig := spawnHookNewRig(t, fixture.Skills, envs[ci])
		warned := rig.expandRaw(c.Steps[0].Twin.OracleWarned)
		output := spawnHookLoad(t, []byte(warned)).(pyjson.Object).Get("hookSpecificOutput").(pyjson.Object)
		prefix, _, found := strings.Cut(pyjson.Text(output.Get("updatedInput").(pyjson.Object).Get("message")), "\n\n")
		payload, ok := spawnHookRouteLoad(rig.expandRaw(c.Steps[0].Stdin))
		if !found || !ok || !strings.HasPrefix(prefix, "[CRW-CONFIG-IGNORED] ") {
			t.Fatalf("recorded warning %q, payload ok %v", prefix, ok)
		}
		a, _, stop := spawnHookAssemble(spawnHookView(payload), rig.env)
		a.trustPrefix = prefix + "\n\n"
		if got := spawnHookRoute(a, rig.env); stop || got != warned {
			t.Fatalf("with the prefix set:\n%q\nwant the oracle's\n%q", got, warned)
		}
		return
	}
	t.Fatal("no trust warning case")
}

// The answer is total: a panic (a nil environment) prints nothing.
func TestSpawnHookRouteTotal(t *testing.T) {
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	valid := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","tool_input":{"message":"x"}}`
	if got := RunSpawnAttachHook(valid, nil); got != "" {
		t.Errorf("a panic prints nothing, got %q", got)
	}
	if got := RunSpawnAttachHook(valid, rig.env); got == "" {
		t.Error("a valid payload is answered")
	}
}

// The skill blocks follow the last text item only while the text items total at most 256 KiB of UTF-16 units. The edge moves with the
// length of the renamed text, so the port finds its own edge and checks the outcomes the oracle gave at it (the recorded itemsCap).
func TestSpawnHookRouteItemsCap(t *testing.T) {
	steps, _ := spawnRouteRead(t)
	fixture := spawnHookReadFixture(t)
	rig := spawnHookNewRig(t, fixture.Skills, spawnHookCase{})
	look := func(n int) (appended bool, total int) {
		items := `[{"type":"text","text":"$crw-dev ` + strings.Repeat("a", n) + `"},{"type":"attachment","ref":"a"},{"type":"text","text":"tail"}]`
		out := RunSpawnAttachHook(`{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"s","cwd":`+strconv.Quote(rig.ws)+`,"tool_input":{"agent_type":"explorer","items":`+items+`}}`, rig.env)
		var texts []string
		for _, item := range spawnHookLoad(t, []byte(out)).(pyjson.Object).Get("hookSpecificOutput").(pyjson.Object).Get("updatedInput").(pyjson.Object).Get("items").([]any) {
			if o := item.(pyjson.Object); o.Get("type") == "text" {
				texts = append(texts, o.Get("text").(string))
			}
		}
		for _, text := range texts {
			total += spawnInlineUTF16Units(text)
		}
		return strings.Contains(texts[len(texts)-1], "<skill name=\"crw-dev\">"), total + (len(texts)-1)*2
	}
	appended, total := look(0)
	if !appended {
		t.Fatal("a small items spawn gets the skill block")
	}
	edge := steps.ItemsCap.MaxUnits - total
	for _, c := range steps.ItemsCap.Cases {
		got, units := look(edge + c.Delta)
		if got != c.Appended || (got && units != steps.ItemsCap.MaxUnits) {
			t.Errorf("edge %+d: appended %v with %d units, want appended %v within %d", c.Delta, got, units, c.Appended, steps.ItemsCap.MaxUnits)
		}
	}
}

// A value nested past the limit is never walked: a recursive walk or writer would overflow the goroutine stack, which ends the process
// where recover cannot help. The value here is 2.1 million levels deep, built without the parser.
func TestSpawnHookRouteDeepValuesAreNotWalked(t *testing.T) {
	var deep any = "x"
	for range 2_100_000 {
		deep = []any{deep}
	}
	if got := spawnHookRoute(spawnHookAssembly{toolInput: pyjson.Object{{Key: "junk", Value: deep}}}, nil); got != "" {
		t.Errorf("a too deep tool_input prints nothing, got %q", got)
	}
}

// The deepest payload 4 MiB holds still parses, so a subagent's recursion is denied and a root spawn prints nothing.
func TestSpawnHookRouteMaximumNesting(t *testing.T) {
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	payload := func(stamp string) string {
		return `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"s",` + stamp + `"tool_input":{"message":"x","junk":` +
			strings.Repeat("[", 2_090_000) + strings.Repeat("]", 2_090_000) + "}}"
	}
	if got := RunSpawnAttachHook(payload(`"agent_id":"c","agent_type":"explorer",`), rig.env); got != DenyEnvelope(RecurseDenyReason) {
		t.Errorf("a subagent spawn is denied, got %.80q", got)
	}
	if got := RunSpawnAttachHook(payload(""), rig.env); got != "" {
		t.Errorf("a root spawn prints nothing, got %.80q", got)
	}
}
