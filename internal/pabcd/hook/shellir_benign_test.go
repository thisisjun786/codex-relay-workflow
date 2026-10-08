package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// shellBenignVerdicts is one command's verdict from each command gate: refused (the reader cannot read it), denied, or
// allowed, with the reason of a refusal or a denial.
type shellBenignVerdicts struct {
	memory, github, worktree string
	reasons                  []string
}

// shellBenignRecords reads the benign corpus: NUL-separated command texts.
func shellBenignRecords(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range strings.Split(string(b), "\x00") {
		if strings.TrimSpace(r) != "" {
			out = append(out, r)
		}
	}
	return out
}

// shellBenignJudge runs one command through the three gates. Each gate reads the command with the shared reader, so a
// command the reader refuses is refused by every gate.
func shellBenignJudge(t *testing.T, rig delRig, cmd string) shellBenignVerdicts {
	t.Helper()
	v := shellBenignVerdicts{memory: "allowed", github: "allowed", worktree: "allowed"}
	if _, err := shellir.AnalyzeEnv(cmd, "/work", nil); err != nil {
		v.memory, v.github, v.worktree = "unreadable", "unreadable", "unreadable"
		v.reasons = []string{err.Error()}
		return v
	}
	if dests, ok := shellIRWriteDests(cmd, "/work", nil); !ok {
		v.memory = "unreadable"
	} else if slicesContainsUnknown(dests) {
		v.memory = "needs grant"
		v.reasons = append(v.reasons, "memory: unknown destination")
	} else if len(dests) > 0 {
		v.memory = "named"
	}
	if reason := githubPostReason(t, githubPostShell(t, rig.checkout, cmd)); reason != "" {
		v.github = "denied"
		v.reasons = append(v.reasons, "github: "+reason)
	}
	if got := rig.verdict(cmd); got.Deny {
		v.worktree = "denied"
		v.reasons = append(v.reasons, "worktree: "+got.Reason)
	}
	return v
}

// TestShellWriteBenignAccounting runs the benign corpus through every gate and reports the counts. The corpus is
// secret-filtered by the parent and read from CRW_BENIGN_COMMANDS (skipped when unset). CRW_BENIGN_REPORT, when set,
// names a file that receives one line per refusal or denial with its reason.
func TestShellWriteBenignAccounting(t *testing.T) {
	file := os.Getenv("CRW_BENIGN_COMMANDS")
	if file == "" {
		t.Skip("CRW_BENIGN_COMMANDS is not set")
	}
	githubPostTempHome(t)
	rig := newDelRig(t)
	records := shellBenignRecords(t, file)
	counts := map[string]map[string]int{"memory": {}, "github": {}, "worktree": {}}
	var report strings.Builder
	for i, cmd := range records {
		v := shellBenignJudge(t, rig, cmd)
		counts["memory"][v.memory]++
		counts["github"][v.github]++
		counts["worktree"][v.worktree]++
		for _, r := range v.reasons {
			fmt.Fprintf(&report, "%d\t%s\t%s\n", i, r, strings.ReplaceAll(cmd, "\n", "\\n"))
		}
	}
	for _, gate := range []string{"memory", "github", "worktree"} {
		keys := make([]string, 0, len(counts[gate]))
		for k := range counts[gate] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, counts[gate][k]))
		}
		t.Logf("%s gate over %d commands: %s", gate, len(records), strings.Join(parts, " "))
	}
	if out := os.Getenv("CRW_BENIGN_REPORT"); out != "" {
		if err := os.WriteFile(out, []byte(report.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// shellBenignHostText is a value the scrubbed fixture never holds: a host path, a state or session identifier, or a
// long hexadecimal run.
var shellBenignHostText = regexp.MustCompile(`/home/|/state/|/scratch|/var/tmp|/data/|/Users/|/tmp/|[0-9a-f]{20,}|[0-9a-f]{8}-[0-9a-f]{4}-`)

// TestShellWriteBenignScrubbedStaysAllowed holds the checked-in scrubbed subset: at most 300 real commands without host
// paths or identifiers, every one of which every gate must allow. CRW_BENIGN_SCRUBBED_OUT, when set, writes the subset
// (chosen from the corpus in file order) as the fixture before the check.
func TestShellWriteBenignScrubbedStaysAllowed(t *testing.T) {
	fixture := filepath.Join("testdata", "shellir", "benign-scrubbed.json")
	if file := os.Getenv("CRW_BENIGN_COMMANDS"); file != "" && os.Getenv("CRW_BENIGN_SCRUBBED_OUT") != "" {
		githubPostTempHome(t)
		rig := newDelRig(t)
		var keep []string
		for _, cmd := range shellBenignRecords(t, file) {
			if len(keep) == 300 {
				break
			}
			if len(cmd) > 400 || shellBenignHostText.MatchString(cmd) || strings.ContainsAny(cmd, "\x00") {
				continue
			}
			if v := shellBenignJudge(t, rig, cmd); v.memory != "named" && v.memory != "allowed" || v.github != "allowed" || v.worktree != "allowed" {
				continue
			}
			keep = append(keep, cmd)
		}
		b, err := json.MarshalIndent(keep, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("CRW_BENIGN_SCRUBBED_OUT"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) > 300 {
		t.Fatalf("fixture holds %d rows, want at most 300", len(rows))
	}
	githubPostTempHome(t)
	rig := newDelRig(t)
	for i, row := range rows {
		cmd := row
		if shellBenignHostText.MatchString(cmd) {
			t.Errorf("row %d holds a host path or identifier", i)
		}
		if v := shellBenignJudge(t, rig, cmd); v.memory == "unreadable" || v.memory == "needs grant" || v.github != "allowed" || v.worktree != "allowed" {
			t.Errorf("row %d %q: memory=%s github=%s worktree=%s %v", i, cmd, v.memory, v.github, v.worktree, v.reasons)
		}
	}
}
