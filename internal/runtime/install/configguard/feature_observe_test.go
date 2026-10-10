package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1143: a Codex CLI that exits 0 is not proof that a flag changed. The commands read the flags back from the same
// config and mark ownership, Changed and success only for the state they observe.

func TestMultiAgentV2ExitZeroIsNotAVerifiedChange(t *testing.T) {
	for _, mode := range []string{"does nothing", "deletes the config", "writes the opposite"} {
		t.Run(mode, func(t *testing.T) {
			home, path := multiAgentHome(t)
			activationWrite(t, path, "[features]\nmulti_agent_v2 = false\n")
			deps := MultiAgentV2Deps{CodexHome: home, Run: func(args []string) CodexRunResult {
				switch mode {
				case "deletes the config":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "writes the opposite":
					activationWrite(t, path, "[features]\nmulti_agent_v2 = false\n# rewritten\n")
				}
				return CodexRunResult{}
			}}
			got, err := SetMultiAgentV2State(deps, MultiAgentV2)
			if err == nil || got != nil {
				t.Fatalf("an exit-0 runner that did not enable v2 was reported as %+v, %v", got, err)
			}
		})
	}
	home, path := multiAgentHome(t)
	activationWrite(t, path, "[features]\nmulti_agent_v2 = false\n")
	deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
		activationWrite(t, path, "[features]\nmulti_agent_v2 = true\n")
		return CodexRunResult{}
	}}
	if got, err := SetMultiAgentV2State(deps, MultiAgentV2); err != nil || !got.Changed || !got.V2Enabled {
		t.Fatalf("a real change: %+v %v", got, err)
	}
}

func TestActivateOwnsOnlyAFlagItObservedTurnOn(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[features]\nhooks = true\n")
	state := map[string]bool{"hooks": true}
	var calls [][]string
	deps := activationDeps(t, home, state, &calls)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "enable" && args[2] == "goals" {
			return CodexRunResult{} // exit 0, nothing changed
		}
		return base(args)
	}
	m, err := Activate(deps)
	if err == nil || m != nil || !strings.Contains(err.Error(), "goals") {
		t.Fatalf("a hard flag that stayed off was reported as %+v, %v", m, err)
	}
	rec := parseInstallManifest(activationRead(t, manifestPath(home)))
	if rec == nil || rec.Flags["goals"].EnabledByCodexclaw || !rec.Flags["multi_agent"].EnabledByCodexclaw || rec.Flags["hooks"].EnabledByCodexclaw {
		t.Fatalf("ownership not taken from the observed state: %+v", rec)
	}
}

func TestActivateSoftFlagThatStaysOffIsASoftFailure(t *testing.T) {
	home := activationHome(t)
	activationWrite(t, filepath.Join(home, "config.toml"), "")
	state := map[string]bool{}
	var calls [][]string
	deps := activationDeps(t, home, state, &calls)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "enable" && args[2] == string(FeatureDefaultModeRequestUserInput) {
			return CodexRunResult{}
		}
		return base(args)
	}
	m, err := Activate(deps)
	if err != nil {
		t.Fatalf("a soft flag that stayed off failed the activation: %v", err)
	}
	f := m.Flags[string(FeatureDefaultModeRequestUserInput)]
	if f.EnabledByCodexclaw || !f.EnableFailed || f.Failure == nil {
		t.Fatalf("soft flag record %+v", f)
	}
}

func TestDeactivateExitZeroIsNotAVerifiedDisable(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "")
	state := map[string]bool{}
	var calls [][]string
	deps := activationDeps(t, home, state, &calls)
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	// The CLI answers 0 to every disable but changes nothing.
	r, err := Deactivate(deactivationDeps(home, func(args []string) CodexRunResult {
		if args[1] == "disable" {
			return CodexRunResult{}
		}
		return deps.Run(args)
	}))
	if err != nil || len(r.Disabled) != 0 || len(r.Failed) != 4 {
		t.Fatalf("an exit-0 disable that changed nothing was reported as %+v, %v", r, err)
	}
	if parseInstallManifest(activationRead(t, manifestPath(home))).ReleasedAt != nil {
		t.Fatal("the ownership was released although the flags are still on")
	}
}

func TestParseFeatureStates(t *testing.T) {
	got, err := ParseFeatureStates("multi_agent   stable            true\ngoals  under development  false\nhooks stable true\nhooks stable true\nunrelated x y\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]FeatureState{"multi_agent": FeatureEnabled, "goals": FeatureDisabled, "hooks": FeatureEnabled, "default_mode_request_user_input": FeatureUnsupported}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %s, want %s (%v)", k, got[k], v, got)
		}
	}
	for name, stdout := range map[string]string{
		"truncated row":           "hooks\ngoals stable true\n",
		"row without a boolean":   "hooks stable yes\n",
		"conflicting rows":        "hooks stable true\nhooks stable false\n",
		"invalid duplicate row":   "hooks stable true\nhooks unknown\n",
		"comment after the value": "hooks stable true # note\n",
	} {
		if states, err := ParseFeatureStates(stdout); err == nil {
			t.Fatalf("%s was read as %v", name, states)
		}
	}
}

// A soft flag this Codex does not list is unsupported: the activation tries it, records the soft failure and goes on.
func TestActivateSoftFlagUnsupportedUpstreamStaysSoft(t *testing.T) {
	home := activationHome(t)
	activationWrite(t, filepath.Join(home, "config.toml"), "")
	state := map[string]bool{}
	var calls [][]string
	deps := activationDeps(t, home, state, &calls)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "list" {
			out := base(args)
			var rows []string
			for _, row := range strings.Split(out.Stdout, "\n") {
				if !strings.HasPrefix(row, string(FeatureDefaultModeRequestUserInput)+" ") {
					rows = append(rows, row)
				}
			}
			return CodexRunResult{Stdout: strings.Join(rows, "\n")}
		}
		if args[2] == string(FeatureDefaultModeRequestUserInput) {
			return CodexRunResult{ExitCode: 2, Stderr: "unknown feature"}
		}
		return base(args)
	}
	m, err := Activate(deps)
	if err != nil || !m.Flags["hooks"].EnabledByCodexclaw || !m.Flags[string(FeatureDefaultModeRequestUserInput)].EnableFailed {
		t.Fatalf("%+v %v", m, err)
	}
}
