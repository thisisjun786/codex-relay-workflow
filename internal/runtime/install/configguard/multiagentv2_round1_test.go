package configguard

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-1143: a write command is verified against the file the runner left. A file the runner removed or left unreadable is a
// failed measurement, in both directions, never a disabled or enabled flag.
func TestMultiAgentV2RunnerThatLeavesNoReadableConfigIsNotAChange(t *testing.T) {
	for _, version := range []MultiAgentVersion{MultiAgentV1, MultiAgentV2} {
		for _, mode := range []string{"deleted", "invalid"} {
			t.Run(string(version)+"-"+mode, func(t *testing.T) {
				home, path := multiAgentHome(t)
				activationWrite(t, path, "[features]\nmulti_agent_v2 = "+strconvBool(version == MultiAgentV1)+"\n")
				got, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
					if mode == "deleted" {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					} else {
						activationWrite(t, path, "truncated {\n")
					}
					return CodexRunResult{}
				}}, version)
				if err == nil || got != nil {
					t.Fatalf("a missing or invalid file after the runner was accepted: %+v %v", got, err)
				}
			})
		}
	}
}

// CRW-1141: the multi_agent_v2 tuning is found and put back through the decoded document, whatever the header spelling.
func TestMultiAgentV2TuningRestoredFromEveryTableSpelling(t *testing.T) {
	long := "# note\nmax = 7\nlist = [\n  \"enabled = true\", # in an array\n  \"[features.multi_agent_v2]\",\n]\ntext = \"\"\"\nenabled = false\n[other]\n\"\"\"\n# trailing comment\n"
	for name, header := range map[string]string{
		"bare":   "[features.multi_agent_v2]",
		"quoted": "[\"features\".\"multi_agent_v2\"]",
		"spaced": "[ features . multi_agent_v2 ]  # mine",
		"mixed":  "[features.'multi_agent_v2']",
	} {
		t.Run(name, func(t *testing.T) {
			home, path := multiAgentHome(t)
			pre := "[model]\nname = \"keep\"\n\n" + header + "\nenabled = false # off\n" + long + "\n[after]\nx = 1\n"
			activationWrite(t, path, pre)
			var calls [][]string
			got, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: multiAgentFake(t, path, &calls)}, MultiAgentV2)
			if err != nil || !got.V2Enabled || !got.Changed {
				t.Fatalf("%+v %v", got, err)
			}
			content := activationRead(t, path)
			for _, kept := range []string{long[:len(long)-1], "[features.multi_agent_v2]\nenabled = true\n"} {
				if !strings.Contains(content, kept) {
					t.Fatalf("lost %q in %q", kept, content)
				}
			}
			// The runner's write replaced the whole fake file, so the other tables are the fake's; the table must be exact.
			if strings.Contains(content, "enabled = false") && !strings.Contains(content, "text = \"\"\"\nenabled = false") {
				t.Fatalf("the old enabled line came back: %q", content)
			}
		})
	}
}

// A tuning form crw cannot put back exactly is refused before the Codex CLI runs, and nothing changes.
func TestMultiAgentV2TuningInAFormCrwCannotRestoreIsRefusedBeforeTheRunner(t *testing.T) {
	for name, pre := range map[string]string{
		"inline table": "[features]\nmulti_agent_v2 = { enabled = false, max = 7 }\n",
		"dotted keys":  "[features]\nmulti_agent_v2.enabled = false\nmulti_agent_v2.max = 7\n",
		"sub-table":    "[features.multi_agent_v2]\nenabled = false\n[features.multi_agent_v2.limits]\nmax = 7\n",
	} {
		t.Run(name, func(t *testing.T) {
			home, path := multiAgentHome(t)
			activationWrite(t, path, pre)
			got, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
				t.Fatal("the runner ran on tuning it would drop")
				return CodexRunResult{}
			}}, MultiAgentV2)
			if err == nil || got != nil || activationRead(t, path) != pre {
				t.Fatalf("%+v %v %q", got, err, activationRead(t, path))
			}
		})
	}
}

// CRW-1153: a repair or a release whose directory sync failed is reported (the record stays in place), never a success.
func TestMultiAgentV2RepairWithAnUnsyncedDirectoryIsReported(t *testing.T) {
	home, path := multiAgentHome(t)
	activationWrite(t, path, "[features.multi_agent_v2]\nenabled = false\nmax = 7\n")
	saved := activationCrwdirPublish
	t.Cleanup(func() { activationCrwdirPublish = saved })
	activationCrwdirPublish = func(p string, b []byte) error {
		if err := saved(p, b); err != nil {
			return err
		}
		return &crwdir.PublishedError{Err: errors.New("injected directory sync")}
	}
	var calls [][]string
	got, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: multiAgentFake(t, path, &calls)}, MultiAgentV2)
	if got == nil || !got.V2Enabled || err == nil || !crwdir.Published(err) || !strings.Contains(activationRead(t, path), "max = 7") {
		t.Fatalf("an unsynced repair was reported as durable: %+v %v", got, err)
	}
}
