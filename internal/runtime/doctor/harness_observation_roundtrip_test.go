package doctor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

func TestHookObservationHomeRoundTrip(t *testing.T) {
	for _, mode := range []string{"empty", "unset", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			plugin, explicit := harnessOptionsPlugin(t)
			home := t.TempDir()
			t.Chdir(t.TempDir())
			env := map[string]string{"HOME": home, "PLUGIN_ROOT": plugin, "CODEX_THREAD_ID": "rec-s1"}
			want := filepath.Join(home, ".codex")
			if mode == "empty" {
				env["CODEX_HOME"] = ""
			} else if mode == "explicit" {
				env["CODEX_HOME"] = explicit
				want = explicit
			}
			if !harness.RecordInvocation(`{"session_id":"rec-s1"}`, "cxc-ops", "session-start", harnessOptionsEnv(env)) {
				t.Fatal("write failed")
			}
			check := HarnessHookExecutionCheck(plugin, HarnessOptions{}, harnessOptionsEnv(env), time.Now())
			if check.Severity != HarnessPass {
				t.Fatalf("writer/doctor round trip: %+v", check)
			}
			if got, err := harnessObservationCodexHome(HarnessOptions{}, harnessOptionsEnv(env)); err != nil || got != want {
				t.Fatalf("home=%q err=%v want=%q", got, err, want)
			}
			// An explicit empty Go option remains a distinct relative store.
			check = HarnessHookExecutionCheck(plugin, HarnessOptions{CodexHome: harnessOptionsPtr("")}, harnessOptionsEnv(env), time.Now())
			if check.Severity == HarnessPass {
				t.Fatalf("explicit empty option adopted environment store: %+v", check)
			}
		})
	}
}
