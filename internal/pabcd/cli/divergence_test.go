package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
)

// The divergence CLI port's tests. TestDivergenceCliMatchesTheRecordedOracle replays the answers the
// CXC v0.2.40 oracle gave runDivergenceCli (testdata/divergence/oracle.json, recorded by
// record-oracle.mjs; no Node runs here); the three tests under it port the B-class cases of
// divergence.test.ts:153-225 and the divergence block of help-verbs.test.ts:120-125 literally; and
// TestDivergenceCliQuoteSpellsAsTheMetricEncoderDoes pins the one string the wrapper spells itself.
// Every test runs in a temporary workspace with HOME, CODEX_HOME and CRW_HOME pointed at a temporary
// tree (the real ~/.codex and ~/.crw are not observed, CRW-1170).

type divergenceCliOracleCase struct {
	ID    string            `json:"id"`
	Cwds  int               `json:"cwds"`
	Setup map[string]string `json:"setup"`
	Steps []struct {
		Argv []string `json:"argv"`
	} `json:"steps"`
	Want struct {
		Steps []struct {
			Code   *int    `json:"code"`
			Output *string `json:"output"`
			Threw  bool    `json:"threw"`
		} `json:"steps"`
		Files struct {
			Cwd0 map[string]string `json:"cwd0"`
			Cwd1 map[string]string `json:"cwd1"`
		} `json:"files"`
	} `json:"want"`
}

func TestDivergenceCliMatchesTheRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/divergence/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string                    `json:"oracle"`
		Cases  []divergenceCliOracleCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("the recorded oracle contains no cases")
	}
	// The timestamp pattern and the name substitution are function-local (no package-level init).
	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`)
	names := strings.NewReplacer("cxc divergence", "crw pabcd divergence", "cxc-loop", "crw-loop")
	for _, c := range fixture.Cases {
		t.Run(c.ID, func(t *testing.T) {
			root := divergenceCliSandbox(t)
			cwd0, cwd1 := filepath.Join(root, "cwd0"), filepath.Join(root, "cwd1")
			for _, dir := range []string{cwd0, cwd1} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for rel, text := range c.Setup {
				path := filepath.Join(cwd0, strings.Replace(rel, ".codexclaw", crwdir.DirName, 1))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The recorder ran its steps with the process cwd set to its cwd0, so a "--cwd ''" case
			// resolves a relative .crw against it; the replay does the same.
			t.Chdir(cwd0)
			for i, step := range c.Steps {
				argv := make([]string, len(step.Argv))
				for j, arg := range step.Argv {
					argv[j] = strings.ReplaceAll(strings.ReplaceAll(arg, "<CWD0>", cwd0), "<CWD1>", cwd1)
				}
				result, err := RunDivergenceCli(argv, cwd0)
				want := c.Want.Steps[i]
				if want.Threw {
					if err == nil {
						t.Errorf("step %d %q: the oracle threw for this input; got code %d %q", i, step.Argv, result.Code, result.Output)
					}
					continue
				}
				if err != nil {
					t.Errorf("step %d %q: %v", i, step.Argv, err)
					continue
				}
				if want.Code == nil || result.Code != *want.Code {
					t.Errorf("step %d %q: code %d, want %v", i, step.Argv, result.Code, want.Code)
				}
				if want.Output != nil {
					if got, expected := stamp.ReplaceAllString(result.Output, "<TS>"), names.Replace(*want.Output); got != expected {
						t.Errorf("step %d %q:\n got %q\nwant %q", i, step.Argv, got, expected)
					}
				}
			}
			if got, want := divergenceCliTree(t, cwd0, stamp), divergenceCliFiles(c.Want.Files.Cwd0); !reflect.DeepEqual(got, want) {
				t.Errorf("cwd0 files: got %v, want %v", got, want)
			}
			if c.Cwds == 2 {
				if got, want := divergenceCliTree(t, cwd1, stamp), divergenceCliFiles(c.Want.Files.Cwd1); !reflect.DeepEqual(got, want) {
					t.Errorf("cwd1 files: got %v, want %v", got, want)
				}
			}
		})
	}
}

// The two runDivergenceCli cases of divergence.test.ts:153-225.
func TestDivergenceCliModeAndCandidateAddList(t *testing.T) {
	cwd := divergenceCliSandbox(t)
	mode, err := RunDivergenceCli([]string{"mode", "on", "--session", "cli", "--collapse", "D", "--reason", "plateau", "--json"}, cwd)
	if err != nil || mode.Code != 0 {
		t.Fatalf("mode on: %v %+v", err, mode)
	}
	var modeJSON struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal([]byte(mode.Output), &modeJSON); err != nil {
		t.Fatal(err)
	}
	if !modeJSON.Active {
		t.Errorf("mode --json: %s", mode.Output)
	}
	add, err := RunDivergenceCli([]string{
		"candidate", "add", "--session", "cli", "--kind", "strong-1", "--title", "Main path",
		"--rationale", "best grounded option", "--source", "https://example.com/source",
		"--change-class", "parameter-tweak", "--killed-at-phase", "D", "--json",
	}, cwd)
	if err != nil || add.Code != 0 {
		t.Fatalf("candidate add: %v %+v", err, add)
	}
	var candidate struct {
		Kind          string `json:"kind"`
		ChangeClass   string `json:"changeClass"`
		KilledAtPhase string `json:"killedAtPhase"`
	}
	if err := json.Unmarshal([]byte(add.Output), &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate.Kind != "strong-1" || candidate.ChangeClass != "parameter-tweak" || candidate.KilledAtPhase != "D" {
		t.Errorf("candidate --json: %s", add.Output)
	}
	list, err := RunDivergenceCli([]string{"candidate", "list", "--session", "cli", "--json"}, cwd)
	if err != nil || list.Code != 0 {
		t.Fatalf("candidate list: %v %+v", err, list)
	}
	var listed struct {
		Candidates []json.RawMessage `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(list.Output), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Candidates) != 1 {
		t.Errorf("candidate list --json: %s", list.Output)
	}
}

// divergence.test.ts:195-225: --cwd writes to the owner archive, not the caller's worktree.
func TestDivergenceCliCwdWritesToTheOwnerArchive(t *testing.T) {
	owner := divergenceCliSandbox(t)
	child := divergenceCliSandbox(t)
	result, err := RunDivergenceCli([]string{
		"candidate", "add", "--session", "cli", "--cwd", owner, "--kind", "add-1", "--title", "Child idea",
		"--rationale", "recorded from an isolated worktree", "--source", "https://example.com/child",
	}, child)
	if err != nil || result.Code != 0 {
		t.Fatalf("candidate add --cwd: %v %+v", err, result)
	}
	if got := metric.ReadDivergenceCandidates(owner, "cli"); len(got) != 1 {
		t.Errorf("the owner archive holds %d candidates, want 1", len(got))
	}
	if got := metric.ReadDivergenceCandidates(child, "cli"); len(got) != 0 {
		t.Errorf("the caller worktree archive holds %d candidates, want 0", len(got))
	}
}

// The divergence block of help-verbs.test.ts:120-125: help prints usage and demands no session, and
// writes nothing (the filesystem assertion the freeze help case makes in the same file).
func TestDivergenceCliHelpTokensDoNotDemandSession(t *testing.T) {
	for _, token := range []string{"help", "--help", "-h"} {
		t.Run(token, func(t *testing.T) {
			cwd := divergenceCliSandbox(t)
			result, err := RunDivergenceCli([]string{token}, cwd)
			if err != nil {
				t.Fatal(err)
			}
			if result.Code != 0 {
				t.Errorf("%s: code %d, want 0", token, result.Code)
			}
			if !strings.Contains(result.Output, "Usage:") {
				t.Errorf("%s: no usage text: %q", token, result.Output)
			}
			if strings.Contains(result.Output, "--session <id> is required") {
				t.Errorf("%s must not demand a session: %q", token, result.Output)
			}
			if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName)); !os.IsNotExist(err) {
				t.Errorf("%s wrote into the workspace", token)
			}
		})
	}
}

// The wrapper's session id is the one string divergence.go spells itself; metric's rowQuote is
// unexported, so this pins the copy to the metric encoder's spelling (JSON.stringify).
func TestDivergenceCliQuoteSpellsAsTheMetricEncoderDoes(t *testing.T) {
	for _, value := range []string{"", "plain", "\"", "\\", "\b\f\n\r\t", "\u0001", "\u2028\u2029", "<>&'", "\U0001F600", "\xff"} {
		encoded := metric.EncodeMode(metric.DivergenceMode{Reason: value})
		start := strings.Index(encoded, `"reason":`)
		end := strings.Index(encoded, `,"updatedAt":`)
		if start < 0 || end < start {
			t.Fatalf("metric.EncodeMode spelled unexpectedly: %s", encoded)
		}
		want := encoded[start+len(`"reason":`) : end]
		if got := divergenceCliQuote(value); got != want {
			t.Errorf("divergenceCliQuote(%q) = %s, the metric encoder spells %s", value, got, want)
		}
	}
}

// divergenceCliTree is the files under <cwd>/.crw, by path relative to it, with ISO timestamps
// replaced; .crw/.gitignore is skipped (its text is renamed by the port and carries no behaviour).
func divergenceCliTree(t *testing.T, cwd string, stamp *regexp.Regexp) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join(cwd, crwdir.DirName)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || entry.Name() == ".gitignore" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = stamp.ReplaceAllString(string(raw), "<TS>")
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// divergenceCliFiles copies a recorded tree so a null one compares as empty.
func divergenceCliFiles(want map[string]string) map[string]string {
	out := map[string]string{}
	for path, text := range want {
		out[path] = text
	}
	return out
}

// divergenceCliSandbox points HOME, CODEX_HOME and CRW_HOME at a temporary tree (the operator rule
// after a self-heal write reached a real CODEX_HOME). It does not observe the real ~/.codex or ~/.crw:
// the host's own Codex sessions write there while the test runs, and with the three variables
// repointed nothing the run resolves from them can reach the real ones (CRW-1170).
func divergenceCliSandbox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	return root
}
