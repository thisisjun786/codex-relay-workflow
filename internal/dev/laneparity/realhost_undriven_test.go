//go:build dev

package laneparity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeCodexEnv makes the test binary a stand-in Codex (TestMain): its value is the way the stand-in
// behaves. A real-host cell runs it through a wrapper script, since the cells give the host an
// environment of their own.
const fakeCodexEnv = "LANEPARITY_FAKE_CODEX"

// The ways the stand-in Codex behaves.
const (
	fakeUnsupported  = "unsupported"   // exec ends at once with no model request: the stub's API is not one it speaks
	fakeNoFeatures   = "no-features"   // the same, and `codex features list` fails as well
	fakePluginBreaks = "plugin-breaks" // exec reaches the provider in a home without a plugin, and fails in one with
)

// fakeCodex is the stand-in Codex: --version, features list, and exec as mode has it.
func fakeCodex(mode string) int {
	args := os.Args[1:]
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "--version":
		fmt.Println("codex-cli 0.0.0-fake")
		return 0
	case "features":
		if mode == fakeNoFeatures {
			fmt.Fprintln(os.Stderr, "error: unrecognized subcommand 'features'")
			return 2
		}
		fmt.Println("hooks stable true")
		return 0
	case "exec":
	default:
		return 2
	}
	config, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if mode != fakePluginBreaks {
		fmt.Fprintln(os.Stderr, "error: model provider stub: wire_api \"responses\" is not supported by this build")
		return 2
	}
	if bytes.Contains(config, []byte("[plugins.")) {
		fmt.Fprintln(os.Stderr, "error: a plugin failed to load")
		return 1
	}
	var base string
	sc := bufio.NewScanner(bytes.NewReader(config))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "base_url = "); ok {
			base, _ = strconv.Unquote(v)
		}
	}
	resp, err := http.Post(base+"/responses", "application/json", strings.NewReader(`{"input":[]}`))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	fmt.Println(`{"type":"thread.started","thread_id":"thread-fake"}`)
	fmt.Println(`{"type":"turn.completed"}`)
	return 0
}

// fakeHost writes the stand-in Codex (behaving as mode) and a stand-in crw whose doctor retrust
// trusts every declared hook of the shipped plugin, or fails when retrustFails.
func fakeHost(t *testing.T, root, mode string, retrustFails bool) (codex, crw string) {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	codex = filepath.Join(dir, "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\n"+fakeCodexEnv+"="+mode+" exec "+q(self)+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, registered, err := ReadRegistered(filepath.Join(root, "plugins", "crw"))
	if err != nil {
		t.Fatal(err)
	}
	retrust := fmt.Sprintf("echo updated=0 appended=%d\nexit 0", len(registered))
	if retrustFails {
		retrust = "echo 'codex features list failed' >&2\nexit 1"
	}
	crw = filepath.Join(dir, "crw")
	script := "#!/bin/sh\nif [ \"$1 $2\" = 'doctor retrust' ]; then\n" + retrust + "\nfi\nexit 0\n"
	if err := os.WriteFile(crw, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return codex, crw
}

// runRealHostCommand runs `parity realhost` for the cells only matches and returns the exit status,
// the report and the text.
func runRealHostCommand(t *testing.T, root, codex, crw, only string) (int, Report, string) {
	t.Helper()
	report := filepath.Join(t.TempDir(), "report.json")
	var out, errs bytes.Buffer
	code := Run([]string{"realhost", "--repo", root, "--crw", crw, "--plugin", filepath.Join(root, "plugins", "crw"), "--codex", codex,
		"--scratch", t.TempDir(), "--only", only, "--json", report}, &out, &errs)
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("exit %d, no report: %v\n%s%s", code, err, out.String(), errs.String())
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.RealHost == nil {
		t.Fatalf("no real-host part: %s", out.String())
	}
	return code, rep, out.String() + errs.String()
}

func notVerifiedCell(rep Report, cell string) (NotVerified, bool) {
	for _, n := range rep.NotVerified {
		if n.Cell == "real host: "+cell {
			return n, true
		}
	}
	return NotVerified{}, false
}

// A host the stub provider cannot drive (exec ends with no model request, and so does a control
// turn in a home without the plugin, its trust or the switch) leaves its cells not verified with the
// reason, does not fail the run, and the scope claims no turn: what the real host would cover stays
// on the unverified list.
func TestRealHost_aHostTheStubCannotDriveIsNotVerified(t *testing.T) {
	root := repoRoot(t)
	codex, crw := fakeHost(t, root, fakeUnsupported, false)
	code, rep, text := runRealHostCommand(t, root, codex, crw, "^(turn/crw|untrusted/crw)$")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, text)
	}
	if len(rep.RealHost.Cells) != 2 {
		t.Fatalf("%+v", rep.RealHost.Cells)
	}
	for _, c := range rep.RealHost.Cells {
		if c.Unverified == "" || c.Driven || len(c.Problems) != 0 || c.ModelRequest != 0 {
			t.Errorf("%s: %+v", c.Name, c)
		}
		if !strings.Contains(c.Unverified, "control turn") || !strings.Contains(c.Unverified, "not supported") {
			t.Errorf("%s: the reason does not say what was measured: %s", c.Name, c.Unverified)
		}
		if n, ok := notVerifiedCell(rep, c.Name); !ok || n.Reason != c.Unverified {
			t.Errorf("%s is not on the unverified list with its reason: %+v", c.Name, rep.NotVerified)
		}
	}
	if strings.Contains(rep.Scope, "ran whole turns") || !strings.Contains(rep.Scope, "no real Codex turn ran") {
		t.Errorf("scope: %s", rep.Scope)
	}
	for _, covered := range realHostCovers {
		found := false
		for _, n := range rep.NotVerified {
			found = found || strings.HasPrefix(n.Cell, covered.prefix)
		}
		if !found {
			t.Errorf("%q left the unverified list although no turn ran", covered.prefix)
		}
	}
}

// A host that runs a turn in a home without the plugin but none in the cell's home is driven into
// failure by what the cell added: the cell fails, it is not excused as not verified, and the scope
// does not count it as a turn that ran.
func TestRealHost_aTurnTheControlRunsButTheCellDoesNotFails(t *testing.T) {
	root := repoRoot(t)
	codex, crw := fakeHost(t, root, fakePluginBreaks, false)
	code, rep, text := runRealHostCommand(t, root, codex, crw, "^untrusted/crw$")
	if code == 0 {
		t.Fatalf("exit 0: %s", text)
	}
	c := rep.RealHost.Cells[0]
	if c.OK || c.Unverified != "" || c.Driven {
		t.Errorf("%+v", c)
	}
	if !strings.Contains(strings.Join(c.Problems, "\n"), "control turn") {
		t.Errorf("the problems do not name the control turn: %q", c.Problems)
	}
	if _, ok := notVerifiedCell(rep, CellUntrusted); ok {
		t.Errorf("a failed cell is listed as not verified: %+v", rep.NotVerified)
	}
	if strings.Contains(rep.Scope, "ran whole turns") {
		t.Errorf("scope: %s", rep.Scope)
	}
}

// crw doctor retrust failing is a failure of the build under test, unless the host cannot be
// prepared at all: when `codex features list`, the check retrust runs, fails in a home without the
// plugin too, the cell is not verified with both measurements. Neither cell ran a turn, and the
// scope says none ran.
func TestRealHost_aFailedRetrust(t *testing.T) {
	root := repoRoot(t)
	t.Run("the host cannot be prepared", func(t *testing.T) {
		codex, crw := fakeHost(t, root, fakeNoFeatures, true)
		code, rep, text := runRealHostCommand(t, root, codex, crw, "^turn/crw$")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, text)
		}
		c := rep.RealHost.Cells[0]
		if c.Unverified == "" || c.Driven || len(c.Problems) != 0 {
			t.Fatalf("%+v", c)
		}
		if !strings.Contains(c.Unverified, "crw doctor retrust exited 1") || !strings.Contains(c.Unverified, "features list") {
			t.Errorf("reason: %s", c.Unverified)
		}
		if _, ok := notVerifiedCell(rep, CellTurnCRW); !ok {
			t.Errorf("not listed: %+v", rep.NotVerified)
		}
		if strings.Contains(rep.Scope, "ran whole turns") {
			t.Errorf("scope: %s", rep.Scope)
		}
	})
	t.Run("the build fails on a host that can be prepared", func(t *testing.T) {
		codex, crw := fakeHost(t, root, fakePluginBreaks, true)
		code, rep, text := runRealHostCommand(t, root, codex, crw, "^turn/crw$")
		if code == 0 {
			t.Fatalf("exit 0: %s", text)
		}
		c := rep.RealHost.Cells[0]
		if c.OK || c.Unverified != "" || c.Driven || !strings.Contains(strings.Join(c.Problems, "\n"), "crw doctor retrust exited 1") {
			t.Fatalf("%+v", c)
		}
		if strings.Contains(rep.Scope, "ran whole turns") {
			t.Errorf("scope: %s", rep.Scope)
		}
	})
}
