package spawn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// spawnLegEnv adds the plugin root and its manifest that harness.RecordInvocation needs to the rig's
// environment. Without PLUGIN_ROOT the record is refused (internal/harness/observation.go:105), and an
// assertion about the observation would then be vacuous; internal/harness/observation_test.go:35-44
// builds the same pair.
func spawnLegEnv(t *testing.T, r *spawnHookRig) (codexHome string) {
	t.Helper()
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	spawnHookMust(t, os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"), []byte(`{"name":"crw","version":"1.2.3"}`), 0o644))
	base := r.env
	r.env = func(key string) (string, bool) {
		if key == "PLUGIN_ROOT" {
			return plugin, true
		}
		return base(key)
	}
	codexHome, _ = base("CODEX_HOME")
	return codexHome
}

// spawnLegAnswer runs one payload through the leg, the way the hook entry does: the raw stdin bytes in,
// the answer out.
func spawnLegAnswer(t *testing.T, r *spawnHookRig, raw string) string {
	t.Helper()
	var out bytes.Buffer
	if code := RunHook(context.Background(), strings.NewReader(raw), &out, r.env); code != 0 {
		t.Fatalf("leg exit %d, want 0", code)
	}
	return out.String()
}

// spawnLegRecords lists the observation records under a CODEX_HOME.
func spawnLegRecords(t *testing.T, codexHome string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(codexHome, "crw", "hook-observations"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// TestSpawnLegReplaysTheRecordedRouteSteps drives the recorded CXC v0.2.40 route steps through the leg
// (readStdin + main) rather than calling RunSpawnAttachHook directly, so the bytes that reach stdout are
// compared with what the oracle printed: the recursion denials, the allows, the managed and final-gate
// answers and the exactly-4-MiB case are all in this recording.
func TestSpawnLegReplaysTheRecordedRouteSteps(t *testing.T) {
	steps, envs := spawnRouteRead(t)
	fixture := spawnHookReadFixture(t)
	nonce := regexp.MustCompile(`\[CRW-SUBSPAWN-GRANT:([a-f0-9]{64})\]`)
	total, denied := 0, 0
	for ci, c := range steps.Route {
		t.Run(envs[ci].Name, func(t *testing.T) {
			rig := spawnHookNewRig(t, fixture.Skills, envs[ci])
			spawnLegEnv(t, rig)
			for i, step := range c.Steps {
				total++
				at := "step " + strconv.Itoa(i+1)
				got := spawnLegAnswer(t, rig, rig.expandRaw(step.Stdin))
				for _, m := range nonce.FindAllStringSubmatch(got, -1) {
					if !slices.Contains(rig.nonces, m[1]) {
						rig.nonces = append(rig.nonces, m[1])
					}
				}
				if strings.Contains(got, RecurseDenyReason) {
					denied++
				}
				if step.ExpectSha256 != "" {
					plain := rig.plain(got)
					sum := sha256.Sum256([]byte(plain))
					if hex.EncodeToString(sum[:]) != step.ExpectSha256 || len(plain) != step.ExpectBytes {
						t.Fatalf("%s: answer sha256 %x (%d bytes), want %s (%d bytes)", at, sum, len(plain), step.ExpectSha256, step.ExpectBytes)
					}
					continue
				}
				if want := rig.expandRaw(step.Expect); got != want {
					t.Fatalf("%s:\n got %q\nwant %q", at, got, want)
				}
			}
		})
	}
	if denied == 0 {
		t.Fatal("no recorded step reached the recursion deny: the leg would not be exercised on it")
	}
	t.Logf("replayed %d recorded route steps through the leg, %d of them a recursion denial", total, denied)
}

// TestSpawnLegReplaysTheRecordedManagedSteps is the same through the managed dispatch recording: the
// allow envelope that carries the issued candidate, and its refusals.
func TestSpawnLegReplaysTheRecordedManagedSteps(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "hook", "oracle.json"))
	spawnHookMust(t, err)
	var managed spawnManagedFile
	spawnHookMust(t, json.Unmarshal(data, &managed))
	fixture := spawnHookReadFixture(t)
	if len(managed.Managed) == 0 {
		t.Fatal("no recorded managed cases")
	}
	total := 0
	for _, c := range managed.Managed {
		t.Run(c.Name, func(t *testing.T) {
			rig := spawnHookNewRig(t, fixture.Skills, spawnHookCase{})
			spawnLegEnv(t, rig)
			for i, step := range c.Steps {
				total++
				at := "step " + strconv.Itoa(i+1) + " (" + step.Note + ")"
				for rel, body := range step.Files {
					path := filepath.Join(rig.ws, filepath.FromSlash(rel))
					spawnHookMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
					spawnHookMust(t, os.WriteFile(path, []byte(body), 0o644))
				}
				got := spawnLegAnswer(t, rig, rig.expandRaw(step.Stdin))
				if step.ExpectSha256 != "" {
					plain := rig.plain(got)
					sum := sha256.Sum256([]byte(plain))
					if hex.EncodeToString(sum[:]) != step.ExpectSha256 || len(plain) != step.ExpectBytes {
						t.Fatalf("%s: answer sha256 %x (%d bytes), want %s (%d bytes)", at, sum, len(plain), step.ExpectSha256, step.ExpectBytes)
					}
					continue
				}
				if want := rig.expandRaw(step.Expect); got != want {
					t.Fatalf("%s:\n got %q\nwant %q", at, got, want)
				}
			}
		})
	}
	t.Logf("replayed %d recorded managed steps through the leg", total)
}

// TestSpawnLegFinalGateDeny drives a final-gate-marked spawn through the leg. The expected bytes are the
// recorded oracle answer of contract/fixtures/cxc/hook__pre-tool-use-attaching-skills__final_gate_missing_test_receipt_denied.json
// with the name substitution applied (R23 [codexclaw -> [crw, R26 .codexclaw -> .crw).
func TestSpawnLegFinalGateDeny(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contract", "fixtures", "cxc",
		"hook__pre-tool-use-attaching-skills__final_gate_missing_test_receipt_denied.json"))
	spawnHookMust(t, err)
	var fixture struct {
		Expect struct {
			Steps []struct {
				StdoutJSON struct {
					HookSpecificOutput struct {
						PermissionDecisionReason string `json:"permissionDecisionReason"`
					} `json:"hookSpecificOutput"`
				} `json:"stdout_json"`
			} `json:"steps"`
		} `json:"expect"`
	}
	spawnHookMust(t, json.Unmarshal(raw, &fixture))
	if len(fixture.Expect.Steps) == 0 {
		t.Fatal("the recorded fixture has no step")
	}
	oracle := fixture.Expect.Steps[0].StdoutJSON.HookSpecificOutput.PermissionDecisionReason
	renamed := strings.NewReplacer("[codexclaw ", "[crw ", ".codexclaw/", ".crw/").Replace(oracle)
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	spawnHookMust(t, os.MkdirAll(filepath.Join(rig.ws, ".crw", "goalplans", "demo"), 0o755))
	spawnHookMust(t, os.MkdirAll(filepath.Join(rig.ws, ".crw", "sessions"), 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(rig.ws, ".crw", "goalplans", "demo", "goalplan.json"),
		[]byte(`{"finalGate":{"testReceiptPath":".crw/evidence/test.json"},"criteria":[{"id":"c1","surface":"logic"}]}`), 0o644))
	spawnHookMust(t, os.WriteFile(filepath.Join(rig.ws, ".crw", "sessions", "rec-s1.json"),
		[]byte(`{"sessionId":"rec-s1","slug":"demo"}`), 0o644))
	payload := `{"hook_event_name":"PreToolUse","session_id":"rec-s1","cwd":` + strconv.Quote(rig.ws) +
		`,"turn_id":"rec-t1","tool_name":"spawn_agent","tool_input":{"message":"[CRW-FINAL-GATE] please review the final gate"},"tool_use_id":"rec-call-1"}`
	got := spawnLegAnswer(t, rig, payload)
	if want := DenyEnvelope(renamed); got != want {
		t.Fatalf("final gate:\n got %q\nwant %q", got, want)
	}
}

// TestSpawnLegOverflowRefusesAndRecordsNothing is the 4 MiB bound: an input one byte over is refused with
// the oracle's envelope and leaves no observation, an input of exactly the bound is processed.
func TestSpawnLegOverflowRefusesAndRecordsNothing(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	codexHome := spawnLegEnv(t, rig)
	payload := `{"hook_event_name":"PreToolUse","session_id":"leg-s1","cwd":` + strconv.Quote(rig.ws) +
		`,"tool_name":"spawn_agent","tool_input":{"message":"big","agent_type":"explorer"}}`
	over := payload + strings.Repeat(" ", harness.MaxStdinBytes+1-len(payload))
	if got, want := spawnLegAnswer(t, rig, over), DenyEnvelope(spawnHookOversizedInputReason); got != want {
		t.Fatalf("overflow:\n got %q\nwant %q", got, want)
	}
	if records := spawnLegRecords(t, codexHome); len(records) != 0 {
		t.Fatalf("an overflow is not recorded, got %v", records)
	}
	exact := payload + strings.Repeat(" ", harness.MaxStdinBytes-len(payload))
	if got := spawnLegAnswer(t, rig, exact); got == DenyEnvelope(spawnHookOversizedInputReason) {
		t.Fatal("exactly 4 MiB is processed, not refused")
	}
}

// TestSpawnLegRecordsTheInvocation is the other half of the oracle's main: everything that is not an
// overflow leaves the metadata-only observation the oracle records for the component and the event.
func TestSpawnLegRecordsTheInvocation(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	codexHome := spawnLegEnv(t, rig)
	payload := `{"hook_event_name":"PreToolUse","session_id":"leg-s1","cwd":` + strconv.Quote(rig.ws) +
		`,"tool_name":"spawn_agent","tool_input":{"message":"x","agent_type":"explorer"}}`
	spawnLegAnswer(t, rig, payload)
	records := spawnLegRecords(t, codexHome)
	if len(records) != 1 {
		t.Fatalf("want one observation, got %v", records)
	}
	body, err := os.ReadFile(records[0])
	spawnHookMust(t, err)
	var record map[string]any
	spawnHookMust(t, json.Unmarshal(body, &record))
	if record["component"] != "subagent-config" || record["event"] != "pre-tool-use" || record["sessionId"] != "leg-s1" {
		t.Fatalf("record %s", body)
	}
}

// spawnLegBrokenInput is a read that fails: the oracle's readStdin catch reads it as empty input.
type spawnLegBrokenInput struct{}

func (spawnLegBrokenInput) Read(p []byte) (int, error) {
	return copy(p, []byte(`{"hook_event_name":"PreToolUse"}`)), errors.New("read failure")
}

func TestSpawnLegReadFailureAnswersNothing(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	var out bytes.Buffer
	if code := RunHook(context.Background(), spawnLegBrokenInput{}, &out, rig.env); code != 0 || out.Len() != 0 {
		t.Fatalf("a failed read: exit %d, output %q", code, out.String())
	}
}

func TestSpawnLegInterrupted(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if code := RunHook(ctx, strings.NewReader(`{}`), &out, rig.env); code != harness.Interrupted || out.Len() != 0 {
		t.Fatalf("interrupted: exit %d, output %q", code, out.String())
	}
}

// TestSpawnLegEndsOnAnInterruptWhileItsInputIsStillOpen is this leg half of
// cmd/crw TestAnInterruptEndsAHookLegWaitingForItsInput: the first interrupt ends the leg at once, while the read is
// still waiting for input the caller never sends. A leg that reads inline would hang here until the pipe closes.
func TestSpawnLegEndsOnAnInterruptWhileItsInputIsStillOpen(t *testing.T) {
	rig := spawnHookNewRig(t, spawnHookReadFixture(t).Skills, spawnHookCase{})
	spawnLegEnv(t, rig)
	in, hold, err := os.Pipe()
	spawnHookMust(t, err)
	defer hold.Close()
	defer in.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	type result struct {
		code int
		out  string
	}
	got := make(chan result, 1)
	go func() {
		var out bytes.Buffer
		code := RunHook(ctx, in, &out, rig.env)
		got <- result{code, out.String()}
	}()
	select {
	case r := <-got:
		if r.code != harness.Interrupted || r.out != "" {
			t.Fatalf("interrupted leg: exit %d, output %q", r.code, r.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an interrupted leg is still waiting for its input")
	}
}
