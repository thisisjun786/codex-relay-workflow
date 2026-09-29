package doctor_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	golden.Helper()
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// host is a temporary HOME laid out as the relay host lays out its runtime.
type host struct {
	home, dest, codex, state, record string
	env                              scope.Env
}

func newHost(t *testing.T) *host {
	t.Helper()
	home := t.TempDir()
	h := &host{home: home, dest: filepath.Join(home, ".local", "share", "crw-runtime"), codex: filepath.Join(home, ".codex"), state: filepath.Join(home, ".local", "state")}
	h.record = filepath.Join(h.state, "codex-relay-workflow", record.Name)
	h.env = scope.Env{"HOME=" + home, "XDG_STATE_HOME=" + h.state, "CODEX_HOME=" + h.codex, "PATH=" + os.Getenv("PATH")}
	for _, d := range []string{h.dest, h.codex, filepath.Dir(h.record)} {
		mkdir(t, d)
	}
	return h
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// relayScript answers the relay's doctor and service status the way a Python console script
// would, run by the venv's python3 (here a link to /bin/sh, so no Python runs).
const relayScript = `case "$*" in
  *"service status"*) echo '{"running": false}' ;;
  *doctor*) echo '{"stateSelection": {"path": "/s/scope", "socketScope": "scope-1"}, "actorReachability": {"socketConnect": "ok"}, "contents": {"available": true, "openAttempts": 0}}' ;;
  *) exit 2 ;;
esac
`

// pythonVenv lays out env-1-<hash> as runtime_install.py builds it: pyvenv.cfg, a python3
// interpreter link, console scripts whose #! names it, and a COMPLETE Python staging claim.
func (h *host) pythonVenv(t *testing.T) string {
	t.Helper()
	env := filepath.Join(h.dest, "env-1-0be23c258476")
	write(t, filepath.Join(env, "pyvenv.cfg"), "home = /usr/bin\nversion = 3.13.14\n", 0o644)
	link(t, "/bin/sh", filepath.Join(env, "bin", "python3"))
	for _, name := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
		write(t, filepath.Join(env, "bin", name), "#!"+filepath.Join(env, "bin", "python3")+"\n"+relayScript, 0o755)
	}
	for _, module := range []string{"codex_session_relay", "codex_thread_bridge"} {
		mkdir(t, filepath.Join(env, "lib", "python3.13", "site-packages", module))
	}
	if err := staging.WriteClaim(env, staging.Payload(staging.Complete, staging.WrittenByPython, "CRW-116", "1", 1, "host", "2026-09-25T00:40:21Z")); err != nil {
		t.Fatal(err)
	}
	return env
}

// fakeCrw is a file whose first bytes are an ELF header: what the doctor classifies, without a
// real binary having to be built.
const fakeCrw = "\x7fELF\x02\x01\x01\x00fake crw"

func (h *host) goRuntime(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := filepath.Join(h.dest, name)
	write(t, filepath.Join(dir, "bin", "crw"), fakeCrw+name, 0o755)
	for _, script := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
		link(t, "crw", filepath.Join(dir, "bin", script))
	}
	sum := sha256.Sum256([]byte(fakeCrw + name))
	return dir, hex.EncodeToString(sum[:])
}

// fixtureRecord installs the committed v1 record with its paths moved under this HOME.
func (h *host) fixtureRecord(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(golden.Dir(), "..", "record", "testdata", "host-record-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, h.record, strings.ReplaceAll(string(raw), "/home/user", h.home), 0o600)
}

func (h *host) settings(t *testing.T, completion, bridge map[string]string) {
	t.Helper()
	encode := func(values map[string]string) string {
		o := record.Object{}
		for k, v := range values {
			o = append(o, contract.Field{Key: k, Value: v})
		}
		return string(record.Encode(o))
	}
	if completion != nil {
		write(t, filepath.Join(h.codex, "crw-completion-hook.json"), encode(completion), 0o600)
	}
	if bridge != nil {
		write(t, filepath.Join(h.codex, "crw-bridge-mcp.json"), encode(bridge), 0o600)
	}
}

func (h *host) current() string { return pointer.Path(h.dest) }

func codex(context.Context) *string {
	v := "codex-cli 0.154.0"
	return &v
}

func (h *host) diagnose(t *testing.T) record.Object {
	t.Helper()
	return doctor.Diagnose(context.Background(), doctor.Options{Env: h.env, CodexVersion: codex, Now: func() time.Time { return time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC) }})
}

func at(o record.Object, path ...string) any {
	var v any = o
	for _, key := range path {
		v = record.Get(golden.Obj(v), key)
	}
	return v
}

// snapshot is every path under root with its bytes (or link target) and mode.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		line := path + " " + info.Mode().String() + " " + info.ModTime().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			line += " -> " + target
		case info.Mode().IsRegular():
			raw, _ := os.ReadFile(path)
			sum := sha256.Sum256(raw)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// On a host with only the Python install, the doctor reports selected: python-venv from the
// pointer target's pyvenv.cfg and the record's selected map - no interpreter, module or
// shebang probe - resolves every settings executable through the pointer to the Python it
// reaches, leaves the Python install to the Python classifier, and writes nothing.
func TestDoctorOnAPythonVenvOnlyHost(t *testing.T) {
	h := newHost(t)
	env := h.pythonVenv(t)
	link(t, env, h.current())
	h.fixtureRecord(t)
	h.settings(t, map[string]string{
		"relayExecutable":    filepath.Join(h.current(), "bin", "codex-session-relay"),
		"adapterEntryPoint":  filepath.Join(h.current(), "bin", "crw-completion-hook"),
		"adapterInterpreter": filepath.Join(h.current(), "bin", "python3"),
	}, map[string]string{"bridgeExecutable": filepath.Join(h.current(), "bin", "codex-thread-bridge")})
	before := snapshot(t, h.home)
	report := h.diagnose(t)
	if after := snapshot(t, h.home); after != before {
		t.Fatal("the doctor changed the host")
	}
	for path, want := range map[string]any{
		"selected": doctor.KindPythonVenv, "hostRecordState": "PRESENT",
		"runtime.kind": doctor.KindPythonVenv, "runtime.recordSelectsKind": doctor.KindPythonVenv, "runtime.agrees": true, "runtime.placementRecorded": true, "runtime.pointerFrom": "record",
		"promotionLock.state": record.NoFile, "promotionLock.held": false,
		"components.codex-session-relay.class": nil, "components.codex-thread-bridge.class": nil,
		"scope.socketConnect": "ok", "checks.results.connected.value": "verified", "checks.results.installed.value": "unknown",
		"checks.results.deliveryAccepted.value": "not_applicable", "checks.results.settingsPreserved.value": "verified",
	} {
		if got := at(report, strings.Split(path, ".")...); golden.Canon(got) != golden.Canon(want) {
			t.Errorf("%s = %s, want %s", path, golden.Canon(got), golden.Canon(want))
		}
	}
	executables := golden.Obj(at(report, "settings", "crw-completion-hook.json", "executables"))
	for key, kind := range map[string]string{"relayExecutable": doctor.KindPythonScript, "adapterEntryPoint": doctor.KindPythonScript, "adapterInterpreter": doctor.KindPythonInterpreter} {
		e := golden.Obj(record.Get(executables, key))
		if record.Get(e, "kind") != kind || record.Get(e, "python") != true || record.Get(e, "throughPointer") != true {
			t.Errorf("%s: %s", key, golden.Canon(e))
		}
	}
	if strings.Contains(golden.Canon(record.Get(executables, "relayExecutable")), `"value":"`+filepath.Join(h.current(), "bin", "codex-session-relay")) == false {
		t.Error("the reference names no Python in its text; it is Python only through the pointer")
	}
	var keys []string
	for _, f := range golden.Obj(at(report, "checks", "results")) {
		keys = append(keys, f.Key)
	}
	if strings.Join(keys, ",") != strings.Join(doctor.CheckFields, ",") {
		t.Errorf("check fields %v", keys)
	}
	if len(golden.List(record.Get(report, "residualPaths"))) != 0 {
		t.Errorf("residue on a settled host: %s", golden.Canon(record.Get(report, "residualPaths")))
	}
}

// goHost is a host whose pointer selects a Go runtime the record describes with its digest and
// a covering point for each component.
func goHost(t *testing.T, point bool) (*host, string, string) {
	t.Helper()
	h := newHost(t)
	h.pythonVenv(t)
	h.fixtureRecord(t)
	dir, digest := h.goRuntime(t, "bin-0.3.0-aaaaaaaaaaaa")
	link(t, dir, h.current())
	hostname, _ := os.Hostname()
	delta := record.Delta{Pointer: record.Object{{Key: "path", Value: h.current()}, {Key: "recordedAt", Value: "2026-09-29T00:00:00Z"}, {Key: "recordedBy", Value: "CRW-157"}}}
	for _, c := range []string{"codex-session-relay", "codex-thread-bridge"} {
		delta.Installs = append(delta.Installs, record.Named{Component: c, Entry: record.GoInstall(dir, c, digest, "crw install", true)})
		delta.Select = append(delta.Select, contract.Field{Key: c, Value: filepath.Join(dir, "bin")})
		if point {
			delta.Points = append(delta.Points, record.Named{Component: c, Entry: record.Object{{Key: "exercised", Value: true}, {Key: "install", Value: filepath.Join(dir, "bin")},
				{Key: "installDigest", Value: digest}, {Key: "codexCli", Value: "codex-cli 0.154.0"}, {Key: "host", Value: hostname}}})
		}
	}
	if _, err := record.Update(h.record, 1, delta); err != nil {
		t.Fatal(err)
	}
	h.settings(t, map[string]string{
		"relayExecutable":    filepath.Join(h.current(), "bin", "codex-session-relay"),
		"adapterEntryPoint":  filepath.Join(h.current(), "bin", "crw-completion-hook"),
		"adapterInterpreter": filepath.Join(h.current(), "bin", "crw-completion-hook"),
	}, map[string]string{"bridgeExecutable": filepath.Join(h.current(), "bin", "codex-thread-bridge")})
	return h, dir, digest
}

// A Go runtime nothing can launch is not owned, though its bytes still match: bin/crw without
// its execute bit (chmod 0644, digest unchanged) or a compatibility link that no longer resolves
// to it makes both components foreign, launchable false, with the problem as the reason, and
// installed is not verified.
func TestDoctorDoesNotOwnAGoRuntimeNothingCanLaunch(t *testing.T) {
	h, dir, digest := goHost(t, true)
	if first := h.diagnose(t); at(first, "components", "codex-session-relay", "launchable") != true || at(first, "components", "codex-session-relay", "class") != "own" {
		t.Fatalf("an executable runtime: %s", golden.Canon(at(first, "components", "codex-session-relay")))
	}
	crw := filepath.Join(dir, "bin", "crw")
	if err := os.Chmod(crw, 0o644); err != nil {
		t.Fatal(err)
	}
	report := h.diagnose(t)
	for _, c := range []string{"codex-session-relay", "codex-thread-bridge"} {
		reasons := golden.Canon(at(report, "components", c, "reasons"))
		if at(report, "components", c, "class") != "foreign" || at(report, "components", c, "launchable") != false ||
			at(report, "components", c, "digest") != digest || !strings.Contains(reasons, crw+" is not executable by this user") {
			t.Errorf("%s without an execute bit: %s", c, golden.Canon(at(report, "components", c)))
		}
	}
	if got := at(report, "checks", "results", "installed", "value"); got != "not_verified" {
		t.Errorf("installed = %v for a runtime nothing can launch", got)
	}
	if err := os.Chmod(crw, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(dir, "bin", "crw-completion-hook")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "bin", "stale-hook"), "#!/bin/sh\n", 0o755)
	link(t, "stale-hook", hook)
	report = h.diagnose(t)
	if got := golden.Canon(at(report, "components", "codex-session-relay", "reasons")); at(report, "components", "codex-session-relay", "class") != "foreign" || !strings.Contains(got, hook+" resolves to") {
		t.Errorf("a link that does not resolve to bin/crw: %s", golden.Canon(at(report, "components", "codex-session-relay")))
	}
}

// Every registration that starts a component is judged against the selected Go runtime: the
// Stop settings' relayExecutable (and, plugin-owned, the adapter the packaged launcher runs),
// the plugin-owned bridge record's bridgeExecutable and config.toml's codex-thread-bridge table.
// One naming another executable (an old runtime's bridge, a Python interpreter, the right binary
// under another name) makes its component conflict; one that cannot be read leaves it
// unreadable; one that starts the selected binary under the component's name (through the
// pointer, or crw with its mode argument) keeps it own.
func TestDoctorJudgesEveryRegistrationAgainstTheSelectedRuntime(t *testing.T) {
	h, dir, _ := goHost(t, true)
	stop := filepath.Join(h.codex, "crw-completion-hook.json")
	bridgeRecord := filepath.Join(h.codex, "crw-bridge-mcp.json")
	config := filepath.Join(h.codex, "config.toml")
	stale := filepath.Join(h.home, "opt", "old", "bin", "codex-thread-bridge")
	write(t, stale, fakeCrw+"old", 0o755)
	env := filepath.Join(h.dest, "env-1-0be23c258476")
	current := func(name string) string { return filepath.Join(h.current(), "bin", name) }
	goodStop := `{"relayExecutable": "` + current("codex-session-relay") + `", "owner": "plugin", "adapterInterpreter": "/usr/bin/env", "adapterEntryPoint": "` + current("crw-completion-hook") + `"}`
	goodBridge := `{"owner": "plugin", "bridgeExecutable": "` + current("codex-thread-bridge") + `", "args": []}`
	for _, c := range []struct {
		name, stop, bridge, config string
		relay, bridgeClass, want   string
	}{
		{"all agree", goodStop, goodBridge, "[mcp_servers.codex-thread-bridge]\ncommand = \"" + filepath.Join(dir, "bin", "crw") + "\"\nargs = [\"bridge\"]\n", "own", "own", ""},
		{"an old runtime's bridge", goodStop, `{"owner": "plugin", "bridgeExecutable": "` + stale + `"}`, "", "own", "conflict", "crw-bridge-mcp.json bridgeExecutable names " + stale},
		{"a Python bridge in config.toml", goodStop, goodBridge, "[mcp_servers.codex-thread-bridge]\ncommand = \"" + filepath.Join(env, "bin", "python3") + "\"\nargs = [\"-m\", \"codex_thread_bridge\"]\n", "own", "conflict", "config.toml mcp_servers.codex-thread-bridge.command names " + filepath.Join(env, "bin", "python3")},
		{"a Python-era adapter", `{"relayExecutable": "` + current("codex-session-relay") + `", "owner": "plugin", "adapterInterpreter": "` + current("python3") + `", "adapterEntryPoint": "` + current("crw-completion-hook") + `"}`, goodBridge, "", "conflict", "own", "adapterInterpreter " + current("python3") + " is a Python interpreter"},
		{"the relay under the bridge's name", `{"relayExecutable": "` + current("codex-thread-bridge") + `"}`, goodBridge, "", "conflict", "own", "as codex-thread-bridge rather than as codex-session-relay"},
		{"unreadable Stop settings", `{"relayExecutable": `, goodBridge, "", "unreadable", "own", "the Stop settings " + stop},
	} {
		t.Run(c.name, func(t *testing.T) {
			write(t, stop, c.stop, 0o600)
			write(t, bridgeRecord, c.bridge, 0o600)
			_ = os.Remove(config)
			if c.config != "" {
				write(t, config, c.config, 0o600)
			}
			report := h.diagnose(t)
			relay, bridge := at(report, "components", "codex-session-relay"), at(report, "components", "codex-thread-bridge")
			if record.Get(golden.Obj(relay), "class") != c.relay || record.Get(golden.Obj(bridge), "class") != c.bridgeClass {
				t.Fatalf("relay %s\nbridge %s", golden.Canon(relay), golden.Canon(bridge))
			}
			if c.want != "" && !strings.Contains(golden.Canon(relay)+golden.Canon(bridge), c.want) {
				t.Errorf("no reason names %q:\nrelay %s\nbridge %s", c.want, golden.Canon(relay), golden.Canon(bridge))
			}
			wantInstalled := "not_verified"
			if c.relay == "own" && c.bridgeClass == "own" {
				wantInstalled = "verified"
			}
			if got := at(report, "checks", "results", "installed", "value"); got != wantInstalled {
				t.Errorf("installed = %v", got)
			}
		})
	}
}

// Every component the definition requires must be selected by an absolute path: a selection
// that is missing, null, not a string, empty or relative leaves the pointer's agreement
// unestablished (agrees null, the selection listed) and the Go classification unreadable,
// rather than letting the pointer agree over the selections that remain.
func TestDoctorLeavesAGoRuntimeUnreadableWhenASelectionIsUnusable(t *testing.T) {
	h, _, _ := goHost(t, true)
	raw, err := os.ReadFile(h.record)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"missing": nil, "null": "null", "number": "42", "empty": `""`, "relative": `"runtime/bin"`} {
		t.Run(name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			selected := document["selected"].(map[string]any)
			if value == nil {
				delete(selected, "codex-thread-bridge")
			} else {
				selected["codex-thread-bridge"] = json.RawMessage(value.(string))
			}
			changed, err := json.MarshalIndent(document, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			write(t, h.record, string(changed)+"\n", 0o600)
			report := h.diagnose(t)
			unusable := golden.Canon(at(report, "runtime", "unusableSelections"))
			if at(report, "runtime", "agrees") != nil || !strings.Contains(unusable, "codex-thread-bridge") {
				t.Errorf("agrees %v, unusable %s", at(report, "runtime", "agrees"), unusable)
			}
			for _, c := range []string{"codex-session-relay", "codex-thread-bridge"} {
				if got := at(report, "components", c, "class"); got != "unreadable" {
					t.Errorf("%s: %v %s", c, got, golden.Canon(at(report, "components", c, "reasons")))
				}
			}
			if got := at(report, "checks", "results", "installed", "value"); got != "not_verified" {
				t.Errorf("installed = %v", got)
			}
		})
	}
}

// On a Go host the pointer selects go-binary, and each component is classified from its
// binary digest, the record's install entry and a covering point: own when all agree, and
// unmeasured, fork, foreign or conflict when one of them does not.
func TestDoctorOnAGoBinaryHost(t *testing.T) {
	h, dir, digest := goHost(t, true)
	before := snapshot(t, h.home)
	report := h.diagnose(t)
	if after := snapshot(t, h.home); after != before {
		t.Fatal("the doctor changed the host")
	}
	for path, want := range map[string]any{
		"selected": doctor.KindGoRuntime, "runtime.recordSelectsKind": doctor.KindGoRuntime, "runtime.agrees": true,
		"codexCli.observed": "codex-cli 0.154.0", "codexCli.pinnedAgainst": "codex-cli 0.154.0", "codexCli.agrees": true,
		"components.codex-session-relay.class": "own", "components.codex-thread-bridge.class": "own",
		"components.codex-session-relay.digest": digest, "components.codex-session-relay.location": filepath.Join(dir, "bin"),
		"checks.results.installed.value": "verified", "checks.results.connected.value": "unknown",
	} {
		if got := at(report, strings.Split(path, ".")...); golden.Canon(got) != golden.Canon(want) {
			t.Errorf("%s = %s, want %s", path, golden.Canon(got), golden.Canon(want))
		}
	}
	for key, e := range golden.Obj(at(report, "settings", "crw-completion-hook.json", "executables")) {
		if record.Get(golden.Obj(e.Value), "python") != false || record.Get(golden.Obj(e.Value), "kind") != doctor.KindNative {
			t.Errorf("%d: %s", key, golden.Canon(e.Value))
		}
	}

	unmeasured, _, _ := goHost(t, false)
	if got := at(unmeasured.diagnose(t), "components", "codex-session-relay", "class"); got != "unmeasured" {
		t.Errorf("no covering point: %v", got)
	}

	write(t, filepath.Join(dir, "bin", "crw"), fakeCrw+"changed", 0o755)
	if got := at(h.diagnose(t), "components", "codex-session-relay", "class"); got != "fork" {
		t.Errorf("bytes that differ from the recorded digest: %v", got)
	}

	other, _ := h.goRuntime(t, "bin-0.3.1-bbbbbbbbbbbb")
	if err := pointer.Place(h.current(), other); err != nil {
		t.Fatal(err)
	}
	report = h.diagnose(t)
	if got := at(report, "components", "codex-session-relay", "class"); got != "conflict" || at(report, "runtime", "agrees") != false {
		t.Errorf("a pointer moved off the recorded selection: %v", got)
	}
	if _, err := record.Update(h.record, 1, record.Delta{Select: []contract.Field{{Key: "codex-session-relay", Value: filepath.Join(other, "bin")}, {Key: "codex-thread-bridge", Value: filepath.Join(other, "bin")}}}); err != nil {
		t.Fatal(err)
	}
	if got := at(h.diagnose(t), "components", "codex-session-relay", "class"); got != "foreign" {
		t.Errorf("a runtime no install entry records: %v", got)
	}
}

// A promotion lock another process holds is reported by the doctor, and a promotion (what
// install runs under it) refuses as Busy instead of waiting for ever.
func TestDoctorReportsAHeldPromotionLockAndPromotionRefuses(t *testing.T) {
	h, _, _ := goHost(t, true)
	_, release := golden.Spawn(t, "", h.record+record.PromotionLockSuffix)
	report := h.diagnose(t)
	if at(report, "promotionLock", "held") != true || at(report, "promotionLock", "state") != record.Held {
		t.Fatalf("a held promotion lock: %s", golden.Canon(record.Get(report, "promotionLock")))
	}
	var busy *record.Busy
	if _, err := record.Promote(h.record, 100*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("a promotion ran while another held the lock: %v", err)
	}
	release()
	if at(h.diagnose(t), "promotionLock", "held") != false {
		t.Fatal("a released promotion lock is still reported held")
	}
}

// An unreadable host record is its own answer and stops Go classification rather than being
// read as a clean host.
func TestDoctorKeepsAnUnreadableRecordApart(t *testing.T) {
	h, _, _ := goHost(t, true)
	write(t, h.record, "{ not json", 0o600)
	report := h.diagnose(t)
	if record.Get(report, "hostRecordState") != "UNREADABLE" || at(report, "components", "codex-session-relay", "class") != "unreadable" || record.Get(report, "hostRecordReading") == nil {
		t.Fatalf("%s %v", record.Get(report, "hostRecordState"), at(report, "components", "codex-session-relay", "class"))
	}
}

// No pointer at all is "none", never a runtime.
func TestDoctorOnACleanHost(t *testing.T) {
	h := newHost(t)
	report := h.diagnose(t)
	if record.Get(report, "selected") != "none" || record.Get(report, "hostRecordState") != "ABSENT" || at(report, "runtime", "pointerFrom") != "default" {
		t.Fatalf("a clean host: %s", golden.Canon(report))
	}
}

// CheckRecordTests: every result must be stated with a known value, a result without a timed
// observation says "unknown" rather than a plausible time, and a temporary destination is
// recorded as one.
func TestCheckRecordStatesEveryResult(t *testing.T) {
	fields := map[string]record.Object{}
	for _, name := range doctor.CheckFields {
		fields[name] = record.Object{{Key: "value", Value: "unknown"}, {Key: "measuredAt", Value: "unknown"}}
	}
	checks, err := doctor.CheckRecord(fields, "/tmp/x", true, nil)
	if err != nil || record.Get(checks, "destinationKind") != "temporary" {
		t.Fatalf("%v %s", err, golden.Canon(checks))
	}
	delete(fields, "connected")
	if _, err := doctor.CheckRecord(fields, "/tmp/x", false, nil); err == nil || !strings.Contains(err.Error(), "missing: connected") {
		t.Fatalf("a missing result: %v", err)
	}
	fields["connected"] = record.Object{{Key: "value", Value: "probably"}}
	if _, err := doctor.CheckRecord(fields, "/tmp/x", false, nil); err == nil {
		t.Fatal("an unsupported value was recorded")
	}
	report := newHost(t).diagnose(t)
	if at(report, "checks", "results", "alwaysActive", "measuredAt") != "unknown" || at(report, "checks", "destinationKind") != "host" {
		t.Fatalf("an untimed observation: %s", golden.Canon(at(report, "checks")))
	}
}
