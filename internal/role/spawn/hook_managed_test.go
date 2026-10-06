package spawn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// testdata/hook/oracle.json's "managed" section holds what the CXC v0.2.40 hook printed for the managed dispatch leg, recorded
// by testdata/hook/record-hook.mjs. A step carries the CRW-named stdin, the oracle's answer renamed and tokenized, and the ledger
// files to write under the rig workspace before the step runs; the ledger is declared on the first step only, so the oracle's own
// mutation (spawnIssued, toolUseId) carries across the steps, as it does here.
type spawnManagedStep struct {
	Note         string            `json:"note"`
	Stdin        string            `json:"stdin"`
	Expect       string            `json:"expect"`
	ExpectSha256 string            `json:"expectSha256"`
	ExpectBytes  int               `json:"expectBytes"`
	Class        string            `json:"classification"`
	Reason       string            `json:"reason"`
	Files        map[string]string `json:"files"`
}

type spawnManagedFile struct {
	Managed []struct {
		Name  string             `json:"name"`
		Steps []spawnManagedStep `json:"steps"`
	} `json:"managed"`
}

// TestSpawnHookManagedOracleReplay replays the recorded managed route steps byte for byte, writing each case's ledger files under
// the rig workspace first.
func TestSpawnHookManagedOracleReplay(t *testing.T) {
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
			if len(c.Steps) == 0 {
				t.Fatal("a recorded managed case has no steps")
			}
			rig := spawnHookNewRig(t, fixture.Skills, spawnHookCase{})
			for i, step := range c.Steps {
				total++
				at := "step " + strconv.Itoa(i+1) + " (" + step.Note + ")"
				if step.Class != "identical" && (step.Class != "intentionally-changed" || step.Reason == "") {
					t.Fatalf("%s is unclassified", at)
				}
				for rel, body := range step.Files {
					path := filepath.Join(rig.ws, filepath.FromSlash(rel))
					spawnHookMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
					spawnHookMust(t, os.WriteFile(path, []byte(body), 0o644))
				}
				got := RunSpawnAttachHook(rig.expandRaw(step.Stdin), rig.env)
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
	t.Logf("replayed %d recorded managed steps", total)
}

// spawnManagedEnv is a CRW home holding one store document, and the environment that reads it.
func spawnManagedEnv(t *testing.T, store string) host.LookupEnv {
	t.Helper()
	home := t.TempDir()
	if store != "" {
		spawnHookMust(t, os.WriteFile(filepath.Join(home, role.StoreFile), []byte(store), 0o644))
	}
	vars := map[string]string{"CRW_HOME": home, "HOME": t.TempDir()}
	return func(key string) (string, bool) { v, ok := vars[key]; return v, ok }
}

// TestSpawnHookDispatchSources covers the branches the recorder cannot reach without a full oracle: the direct marker, the guard
// unwrap, the coordinator grant-instruction strip and its near-match refusal, the prompt ordering, and the role each source carries.
func TestSpawnHookDispatchSources(t *testing.T) {
	marker := "[CRW-DISPATCH:one:att-1]"
	env := spawnManagedEnv(t, "")
	if sources, err := spawnDispatchSources(marker+"\nTASK: x", env); err != nil || len(sources) != 1 || sources[0].Source != marker || sources[0].Role != nil {
		t.Fatalf("direct marker: %+v err %v", sources, err)
	}
	if sources, err := spawnDispatchSources("plain task", env); err != nil || sources != nil {
		t.Fatalf("no marker: %+v err %v", sources, err)
	}
	// An unwrapped guarded message with no prompt overrides yields one source per role, in Roles order.
	guarded := V1ScopeBlock + "\n\n" + marker + "\nTASK: x"
	sources, err := spawnDispatchSources(guarded, env)
	spawnHookMust(t, err)
	if len(sources) != len(role.Roles()) {
		t.Fatalf("guarded: %d sources, want %d", len(sources), len(role.Roles()))
	}
	for i, r := range role.Roles() {
		if sources[i].Role == nil || *sources[i].Role != r || sources[i].Source != marker {
			t.Fatalf("guarded source %d: %+v, want role %q", i, sources[i], r)
		}
	}
	// A role promptOverride leads the remaining text and keeps only that role.
	store := "{\"roles\":{\"reviewer\":{\"mode\":\"default\",\"promptOverride\":\"Review carefully.\"}}}"
	env = spawnManagedEnv(t, store)
	withPrompt := V1ScopeBlock + "\n\nReview carefully.\n\n" + marker + "\nTASK: x"
	sources, err = spawnDispatchSources(withPrompt, env)
	spawnHookMust(t, err)
	if len(sources) != 1 || sources[0].Role == nil || *sources[0].Role != role.Reviewer || sources[0].Source != marker {
		t.Fatalf("prompt override: %+v", sources)
	}
	// A coordinator guard strips its grant instruction before the marker is read.
	env = spawnManagedEnv(t, "")
	grant := V1ScopeBlockCoordinator + spawnGrantInstruction + "[CRW-SUBSPAWN-GRANT:" + strings.Repeat("a", 64) + "]"
	sources, err = spawnDispatchSources(grant+"\n\n"+marker+"\nTASK: x", env)
	spawnHookMust(t, err)
	if len(sources) != len(role.Roles()) {
		t.Fatalf("coordinator guard: %d sources, want %d", len(sources), len(role.Roles()))
	}
	// A near-match grant instruction is not stripped, so the tail does not open with a blank line and nothing unwraps.
	near := V1ScopeBlockCoordinator + strings.Replace(spawnGrantInstruction, "One child", "ONE child", 1) + "[CRW-SUBSPAWN-GRANT:" + strings.Repeat("a", 64) + "]"
	if sources, err = spawnDispatchSources(near+"\n\n"+marker+"\nTASK: x", env); err != nil || sources != nil {
		t.Fatalf("near-match grant: %+v err %v", sources, err)
	}
}

// TestSpawnHookWithout covers the key filter the managed issuance uses to delete a null candidate model or effort.
func TestSpawnHookWithout(t *testing.T) {
	o := spawnTestObject("model", "x", "message", "m", "reasoning_effort", "low")
	if got := spawnHookWithout(o, "model"); got.Get("message") != "m" || got.Get("reasoning_effort") != "low" || got.Get("model") != nil || len(got) != 2 {
		t.Fatalf("without model: %v", got)
	}
	if got := spawnHookWithout(o, "absent"); len(got) != len(o) || got[0].Key != "model" {
		t.Fatalf("absent key: %v", got)
	}
}

// spawnTestObject builds a small ordered object for the filter test.
func spawnTestObject(pairs ...string) pyjson.Object {
	var o pyjson.Object
	for i := 0; i+1 < len(pairs); i += 2 {
		o = o.Set(pairs[i], pairs[i+1])
	}
	return o
}
