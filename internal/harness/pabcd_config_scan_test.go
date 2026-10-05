package harness

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// configInterviewUsageForTest is the oracle's usage text (pabcd-state/src/cli.ts:224-228) in CRW
// names, pinned independently of the adapter's own constant.
const configInterviewUsageForTest = "Usage:\n  crw pabcd config interview [off|new-unit|always]\n\n" +
	"  off       only an explicit 'interview' / '인터뷰' request opens the Interview\n" +
	"  new-unit  also open it on the first plan request of a new unit (default)\n" +
	"  always    open it on every plan request\n\n" +
	"The Interview is advisory: it never changes the PABCD phase, and goal mode always suppresses it.\n"

// TestPabcdCLIConfigScanVerbs drives the two verb rows this issue adds (config, scan) through the
// dispatcher. The fixed table shares one fresh root (nothing in it writes); every other subtest
// takes its own pabcdCLITestHome root, so no case observes another case's crw.json.
func TestPabcdCLIConfigScanVerbs(t *testing.T) {
	t.Run("arguments_and_streams", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		help := cli.RunScanCli(*cli.ParseScanCliArgs([]string{"help"}, root).Args).Output + "\n"
		for _, tc := range []struct {
			name        string
			args        []string
			code        int
			out, errOut string
		}{
			{"config_bare", []string{"config"}, 2, "", "config: this component handles 'config interview' only\n" + configInterviewUsageForTest},
			{"config_interview", []string{"config", "interview"}, 0, configInterviewUsageForTest + "current: new-unit\n", ""},
			{"config_interview_help", []string{"config", "interview", "--help"}, 0, configInterviewUsageForTest + "current: new-unit\n", ""},
			{"config_interview_h", []string{"config", "interview", "-h"}, 0, configInterviewUsageForTest + "current: new-unit\n", ""},
			{"config_interview_unknown", []string{"config", "interview", "sometimes"}, 2, "", "config interview: unknown policy 'sometimes'\n" + configInterviewUsageForTest},
			{"config_other_subcommand", []string{"config", "list"}, 2, "", "config: this component handles 'config interview' only\n" + configInterviewUsageForTest},
			{"scan_help_word", []string{"scan", "help"}, 0, help, ""},
			{"scan_help_flag", []string{"scan", "--help"}, 0, help, ""},
			{"scan_h", []string{"scan", "-h"}, 0, help, ""},
			{"scan_bare", []string{"scan"}, 1, "", "scan: unknown scan action ''; run crw pabcd scan --help\n"},
			{"scan_show", []string{"scan", "show"}, 1, "", "scan: unknown scan action 'show'; run crw pabcd scan --help\n"},
			{"scan_record_missing_session", []string{"scan", "record"}, 1, "", "scan: scan record: --session <id> is required (mutating command, no latest-session fallback)\n"},
			{"scan_record_help", []string{"scan", "record", "--help"}, 1, "", "scan: unknown argument '--help'\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, out, errOut := pabcdCLITestRun(tc.args, "")
				if code != tc.code || out != tc.out || errOut != tc.errOut {
					t.Fatalf("got %d %q %q; want %d %q %q", code, out, errOut, tc.code, tc.out, tc.errOut)
				}
			})
		}
	})

	t.Run("interview_write_and_read", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		cwd, err := syscall.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		code, out, errOut := pabcdCLITestRun([]string{"config", "interview", "always"}, "")
		want := "interview policy: always (" + filepath.Join(cwd, "crw.json") + ")\n"
		if code != 0 || out != want || errOut != "" {
			t.Fatalf("set: %d %q %q", code, out, errOut)
		}
		if got := projectcfg.ReadPolicy(cwd); got != projectcfg.PolicyAlways {
			t.Fatalf("policy not written: %q", got)
		}
		if _, err := os.Stat(filepath.Join(root, "crw.json")); err != nil {
			t.Fatal(err)
		}
		if code, out, errOut := pabcdCLITestRun([]string{"config", "interview"}, ""); code != 0 || out != configInterviewUsageForTest+"current: always\n" || errOut != "" {
			t.Fatalf("read: %d %q %q", code, out, errOut)
		}
	})

	t.Run("interview_replaces_malformed", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		cwd, err := syscall.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(cwd, "crw.json")
		if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := pabcdCLITestRun([]string{"config", "interview", "off"}, "")
		want := "interview policy: off (" + path + ") — the previous file was not valid JSON and was replaced\n"
		if code != 0 || out != want || errOut != "" {
			t.Fatalf("replace: %d %q %q", code, out, errOut)
		}
		if got := projectcfg.ReadPolicy(cwd); got != projectcfg.PolicyOff {
			t.Fatalf("policy after replace: %q", got)
		}
		if _, err := os.Stat(filepath.Join(root, "crw.json")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("interview_write_failure", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		dir := filepath.Join(root, "crw.json")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := pabcdCLITestRun([]string{"config", "interview", "always"}, "")
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "config interview: could not read ") || !strings.Contains(errOut, "(left unchanged)") || !strings.HasSuffix(errOut, "\n") {
			t.Fatalf("failure: %d %q %q", code, out, errOut)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("directory touched: %v %v", info, err)
		}
	})

	t.Run("deleted_cwd", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		missing := filepath.Join(root, "removed")
		if err := os.Mkdir(missing, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(missing)
		if err := os.Remove(missing); err != nil {
			t.Fatal(err)
		}
		if code, out, errOut := pabcdCLITestRun([]string{"config", "interview", "sometimes"}, ""); code != 2 || out != "" || errOut != "config interview: unknown policy 'sometimes'\n"+configInterviewUsageForTest {
			t.Fatalf("unknown policy needs no cwd: %d %q %q", code, out, errOut)
		}
		for _, args := range [][]string{{"config", "interview"}, {"scan", "--help"}} {
			code, out, errOut := pabcdCLITestRun(args, "")
			if code != 1 || out != "" || !strings.HasPrefix(errOut, "crw cli failed: ") || !strings.HasSuffix(errOut, "\n") {
				t.Fatalf("%v: %d %q %q", args, code, out, errOut)
			}
		}
	})

	t.Run("scan_record_round", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		code, out, errOut := pabcdCLITestRun([]string{"scan", "record", "--session", "s1", "--known", "goal=export CSV"}, "")
		if code != 0 || errOut != "" || !strings.HasPrefix(out, "scan record: round 1 recorded for session s1 (") || !strings.HasSuffix(out, "\n") {
			t.Fatalf("record: %d %q %q", code, out, errOut)
		}
		s := state.ReadState(root, "s1")
		if s.Interview == nil || s.Interview.ScanRounds != 1 {
			t.Fatalf("tracker: %+v", s.Interview)
		}
	})

	t.Run("scan_record_refusal_on_stdout", func(t *testing.T) {
		root := pabcdCLITestHome(t)
		path := state.StatePath(root, "s1")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("unreadable"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := pabcdCLITestRun([]string{"scan", "record", "--session", "s1"}, "")
		want := "scan record failed: session state is unreadable; refusing to overwrite it\n"
		if code != 1 || out != want || errOut != "" {
			t.Fatalf("refusal: %d %q %q", code, out, errOut)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != "unreadable" {
			t.Fatalf("state overwritten: %q %v", b, err)
		}
	})
}
