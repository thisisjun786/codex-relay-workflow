package manage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The upgrade harness: temporary homes, one fake crw/relay script placed everywhere a runtime is
// reached, a release directory holding one verified archive, a temporary relay store, and a clock
// the test holds. Nothing here reads or writes a real home, runtime, service or relay store.

const (
	// upgradeGoodCommit is the commit the fake gh resolves a released version to.
	upgradeGoodCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// upgradeArchiveName is the release asset the harness writes.
	upgradeArchiveName = "crw_0.4.0_linux_amd64.tar.gz"
	// upgradeArchiveVersion is the version that asset's name carries, and so the version the
	// installer's runtime directory rule would use.
	upgradeArchiveVersion = "0.4.0"
	// upgradeStatusSame and upgradeStatusUnknown are the service status answers the fake gives.
	upgradeStatusSame    = `{"running":true,"launchPolicy":{"matchesRunning":"same"}}`
	upgradeStatusUnknown = `{"running":true,"launchPolicy":{"matchesRunning":"unknown"}}`
	// upgradeFakeVersion is the version the fakes answer with unless a test says otherwise.
	upgradeFakeVersion = "v0.4.0-4633-geb2567df7"
	// upgradeClockStart is where the injected clock starts, so two runs of one test share a second
	// whatever the wall clock does.
	upgradeClockStart = "2026-01-01T00:00:00Z"
)

// upgradeEnv is one run's environment.
type upgradeEnv struct {
	t       *testing.T
	home    string
	codex   string
	state   string
	release string

	// calls is the arguments of every call the fake answered; exeCalls is one line per call as
	// "<executable><TAB><arguments>", so a test can see which runtime a call came from.
	calls    string
	exeCalls string
	ghCalls  string

	statusAnswers string
	statusCounter string
	// started marks that a service start succeeded, so a status read reports a service that is
	// really up rather than one the script was merely asked about.
	started     string
	pointerLink string

	// previous is the runtime directory the owned pointer names before the run, and installed the
	// runtime directory the fake update reports it produced.
	previous  string
	installed string
	// other is a runtime that is neither the one the pointer names before the run nor the one the
	// update produces, used to show that a pointer moved to a third runtime is refused.
	other string

	// script is the fake, and now is the clock every run reads.
	script string
	now    time.Time
}

type upgradeHarnessOptions struct {
	// version is what the release archive's crw answers --version with.
	version string
	// installedVersion is what the runtime the update produces answers --version with; empty
	// means the archive's version.
	installedVersion string
	// previousVersion is what the runtime the pointer already names answers --version with; empty
	// means the archive's version. A real upgrade replaces one version with another, so the
	// runtime left behind answers differently from the archive.
	previousVersion string
	// openAttempts is the doctor answer's contents.openAttempts.
	openAttempts int
	// doctorUnavailable makes the doctor answer report contents it could not read.
	doctorUnavailable bool
	// doctorNoCount makes the doctor answer report readable contents that carry no open attempt
	// count, which is the other half of the fail-closed guard.
	doctorNoCount bool
	gh            map[string]upgradeGhAnswer
	// pointer is whether the owned pointer exists before the run.
	pointer bool
	// installExit is the status the fake update ends with.
	installExit int
	// produceRuntime makes the fake update create a runtime directory and name it in its answer.
	produceRuntime bool
	// pointAtIt makes the fake update move the owned pointer to that directory.
	pointAtIt bool
	// pointAtOther makes the fake update move the owned pointer to a runtime that is neither the one
	// the pointer named before the run nor the one the update produces, as a concurrent install or
	// rollback would.
	pointAtOther bool
	// breakPointer makes the fake update remove the owned pointer, as an update that died between
	// committing the new selection and placing the link would.
	breakPointer bool
	// mutateConfig makes the fake update append to the configuration file.
	mutateConfig bool
	// unreadableConfigAfter makes the fake update leave the configuration file unreadable without
	// changing its contents, so the post-check cannot tell whether it changed.
	unreadableConfigAfter bool
	// deleteConfigAfter makes the fake update remove the configuration file the snapshot read, which
	// is a change the post-check can state exactly.
	deleteConfigAfter bool
	// breakInstalledStart makes the runtime the update produces fail to start the service, so the
	// restart has to fall back.
	breakInstalledStart bool
	// breakPointerDuringWait removes the owned pointer on the second service status read, as another
	// process on the host moving the pointer while the run waits for the service would.
	breakPointerDuringWait bool
	// installedStartAlreadyRunning makes the runtime the update produces answer its start with the
	// service already up, as it does when something started that runtime between the stop and the
	// restart.
	installedStartAlreadyRunning bool
	// stopExit is the status the fake service stop ends with.
	stopExit int
	// statusAnswers is the matchesRunning answer for each service status read, the last one
	// repeating; empty means one answer that runs and matches.
	statusAnswers []string
}

type upgradeGhAnswer struct {
	Body string
	Exit int
}

func upgradeHarness(t *testing.T, opts upgradeHarnessOptions) *upgradeEnv {
	t.Helper()
	if opts.version == "" {
		opts.version = upgradeFakeVersion
	}
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	state := filepath.Join(home, "relay-state")
	for _, dir := range []string{codex, state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("XDG_STATE_HOME", "")
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte("model = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".local", "share", "crw-runtime")
	started, err := time.Parse(time.RFC3339, upgradeClockStart)
	if err != nil {
		t.Fatal(err)
	}
	h := &upgradeEnv{t: t, home: home, codex: codex, state: state, release: t.TempDir(),
		calls: filepath.Join(home, "crw-calls.txt"), exeCalls: filepath.Join(home, "crw-exe-calls.txt"),
		ghCalls:       filepath.Join(home, "gh-calls.jsonl"),
		statusAnswers: filepath.Join(home, "service-status-answers"),
		statusCounter: filepath.Join(home, "service-status-count"),
		started:       filepath.Join(home, "service-started"),
		pointerLink:   filepath.Join(root, "current"),
		previous:      filepath.Join(root, "bin-"+upgradeArchiveVersion+"-000000000000"),
		installed:     filepath.Join(root, "bin-"+upgradeArchiveVersion+"-111111111111"),
		other:         filepath.Join(root, "bin-"+upgradeArchiveVersion+"-222222222222"),
		now:           started}
	h.script = h.fakeScript(opts)
	h.writeFakes(opts)
	h.writeStore()
	h.writeArchive(opts)
	previous := opts.previousVersion
	if previous == "" {
		previous = opts.version
	}
	h.installRuntime(h.previous, previous)
	h.installRuntime(h.other, previous)
	h.installPointer(opts.pointer)
	return h
}

// advance moves the injected clock, so a test can put two runs in different seconds.
func (h *upgradeEnv) advance(d time.Duration) { h.now = h.now.Add(d) }

// pointerTarget is where the owned pointer resolves, or an error when it resolves nowhere.
func (h *upgradeEnv) pointerTarget() (string, error) {
	target, err := filepath.EvalSymlinks(h.pointerLink)
	if err != nil {
		return "", err
	}
	return target, nil
}

// fakeScript is the one fake the harness puts everywhere a crw or a codex-session-relay is
// reached: on PATH, in the runtime the pointer names, in the runtime the update produces, and
// inside the release archive. It records its call, answers the version of the runtime it sits in,
// the relay doctor and the service status, and its install arm does what the options say an update
// does.
func (h *upgradeEnv) fakeScript(opts upgradeHarnessOptions) string {
	installed := opts.installedVersion
	if installed == "" {
		installed = opts.version
	}
	doctor := "{\"stateSelection\":{\"path\":" + jsonString(h.state) + "},\"contents\":{\"available\":true,\"openAttempts\":" + strconv.Itoa(opts.openAttempts) + "}}"
	if opts.doctorUnavailable {
		doctor = "{\"stateSelection\":{\"path\":" + jsonString(h.state) + "},\"contents\":{\"available\":false,\"openAttempts\":null}}"
	}
	if opts.doctorNoCount {
		doctor = "{\"stateSelection\":{\"path\":" + jsonString(h.state) + "},\"contents\":{\"available\":true,\"openAttempts\":null}}"
	}
	answers := opts.statusAnswers
	if len(answers) == 0 {
		answers = []string{upgradeStatusSame}
	}
	if err := os.WriteFile(h.statusAnswers, []byte(strings.Join(answers, "\n")+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	bin := filepath.Join(h.installed, "bin")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("printf '%s\\n' \"$*\" >> " + coreShellQuote(h.calls) + "\n")
	b.WriteString("printf '%s\\t%s\\n' \"$0\" \"$*\" >> " + coreShellQuote(h.exeCalls) + "\n")
	// The version a runtime answers with is the file beside it, so the archive, the runtime the
	// pointer already names and the runtime the update produces can each report their own.
	b.WriteString("dir=" + "$" + "{0%/*}\n")
	b.WriteString("case \"$*\" in\n")
	b.WriteString("*\"service stop\"*) exit " + strconv.Itoa(opts.stopExit) + ";;\n")
	b.WriteString("*\"service start\"*)\n")
	if opts.breakInstalledStart {
		// The runtime the update produced cannot start the service: its relay executable fails.
		b.WriteString("  [ \"$dir\" = " + coreShellQuote(filepath.Join(h.installed, "bin")) + " ] && exit 1\n")
	}
	if opts.installedStartAlreadyRunning {
		// The service is already up on the runtime the update produced: start refuses with
		// already_running rather than launching a second one. The started marker is set so a status
		// read reports a service that is really up.
		b.WriteString("  [ \"$dir\" = " + coreShellQuote(filepath.Join(h.installed, "bin")) + " ] && { printf '%s\\n' \"$dir\" > " + coreShellQuote(h.started) + "; printf '%s\\n' " + coreShellQuote("{\"ok\":false,\"reason\":\"already_running\"}") + "; exit 2; }\n")
	}
	b.WriteString("  printf '%s\\n' \"$dir\" > " + coreShellQuote(h.started) + "\n")
	b.WriteString("  exit 0;;\n")
	b.WriteString("*\"service status\"*)\n")
	b.WriteString("  n=$(cat " + coreShellQuote(h.statusCounter) + " 2>/dev/null || printf '0')\n")
	b.WriteString("  n=$((n + 1))\n")
	if opts.breakPointerDuringWait {
		b.WriteString("  [ \"$n\" = \"2\" ] && rm -f " + coreShellQuote(h.pointerLink) + "\n")
	}
	// A status read before anything started the service reports it as down, so a run that never
	// restarts cannot pass the post-check by reading an answer that was scripted for another case.
	b.WriteString("  [ -f " + coreShellQuote(h.started) + " ] || { printf '%s\\n' " + coreShellQuote("{\"running\":false,\"launchPolicy\":{\"matchesRunning\":\"different\"}}") + "; exit 0; }\n")
	b.WriteString("  lines=$(wc -l < " + coreShellQuote(h.statusAnswers) + ")\n")
	b.WriteString("  [ \"$n\" -gt \"$lines\" ] && n=$lines\n")
	b.WriteString("  printf '%s\\n' \"$n\" > " + coreShellQuote(h.statusCounter) + "\n")
	b.WriteString("  sed -n \"" + "$" + "{n}p\" " + coreShellQuote(h.statusAnswers) + "\n")
	b.WriteString("  ;;\n")
	b.WriteString("*doctor*) printf '%s\\n' " + coreShellQuote(doctor) + " ;;\n")
	b.WriteString("*\"install update\"*)\n")
	if opts.breakPointer {
		b.WriteString("  rm -f " + coreShellQuote(h.pointerLink) + "\n")
	}
	if opts.pointAtOther {
		b.WriteString("  ln -sfn " + coreShellQuote(h.other) + " " + coreShellQuote(h.pointerLink) + "\n")
	}
	if opts.mutateConfig {
		b.WriteString("  printf 'changed\\n' >> \"$CODEX_HOME/config.toml\"\n")
	}
	if opts.unreadableConfigAfter {
		b.WriteString("  chmod 000 \"$CODEX_HOME/config.toml\"\n")
	}
	if opts.deleteConfigAfter {
		b.WriteString("  rm -f \"$CODEX_HOME/config.toml\"\n")
	}
	if opts.produceRuntime {
		b.WriteString("  mkdir -p " + coreShellQuote(bin) + "\n")
		b.WriteString("  cp \"$0\" " + coreShellQuote(filepath.Join(bin, "crw")) + "\n")
		b.WriteString("  cp \"$0\" " + coreShellQuote(filepath.Join(bin, "codex-session-relay")) + "\n")
		b.WriteString("  chmod 755 " + coreShellQuote(filepath.Join(bin, "crw")) + " " + coreShellQuote(filepath.Join(bin, "codex-session-relay")) + "\n")
		b.WriteString("  printf '%s\\n' " + coreShellQuote(installed) + " > " + coreShellQuote(filepath.Join(bin, "version")) + "\n")
		if opts.pointAtIt {
			b.WriteString("  ln -sfn " + coreShellQuote(h.installed) + " " + coreShellQuote(h.pointerLink) + "\n")
		}
		b.WriteString("  printf '%s\\n' " + coreShellQuote("{\"environment\":"+jsonString(h.installed)+"}") + "\n")
	}
	b.WriteString("  exit " + strconv.Itoa(opts.installExit) + ";;\n")
	b.WriteString("*\"--version\"*) cat \"$dir/version\" 2>/dev/null || printf '%s\\n' " + coreShellQuote(opts.version) + " ;;\n")
	b.WriteString("esac\n")
	b.WriteString("exit 0\n")
	return b.String()
}

// jsonString is s as a JSON string, for a fake answer built by concatenation.
func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func upgradeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeArchive writes the release archive and its SHA256SUMS. The archive holds the fake crw and
// the version file beside it, so the extracted crw reports the archive's version.
func (h *upgradeEnv) writeArchive(opts upgradeHarnessOptions) {
	h.t.Helper()
	body := upgradeTarGz(h.t, map[string]string{"crw": h.script, "version": opts.version + "\n"})
	if err := os.WriteFile(filepath.Join(h.release, upgradeArchiveName), body, 0o600); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	sums := hex.EncodeToString(sum[:]) + "  " + upgradeArchiveName + "\n"
	if err := os.WriteFile(filepath.Join(h.release, upgradeSumsName), []byte(sums), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// writeStore writes a relay store whose attempts are all settled. The run reads open attempts from
// the relay doctor, so a run that went back to reading this store would see none: the doctor cases
// are what pin the count.
func (h *upgradeEnv) writeStore() {
	h.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.state, "relay.sqlite3"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE attempts (request_id TEXT, internal_state TEXT)"); err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.Exec("INSERT INTO attempts VALUES (?, ?)", fmt.Sprintf("row-%d", i), "settled"); err != nil {
			h.t.Fatal(err)
		}
	}
}

// writeFakes places the fake crw and gh on PATH.
func (h *upgradeEnv) writeFakes(opts upgradeHarnessOptions) {
	h.t.Helper()
	bin := filepath.Join(h.home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crw"), []byte(h.script), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "version"), []byte(opts.version+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + coreShellQuote(h.ghCalls) + "\n")
	body.WriteString("case \"$2\" in\n")
	for path, answer := range opts.gh {
		body.WriteString(coreShellQuote(path) + ") printf '%s\\n' " + coreShellQuote(answer.Body) + "; exit " + strconv.Itoa(answer.Exit) + ";;\n")
	}
	body.WriteString("*) exit 1;;\nesac\n")
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body.String()), 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installRuntime places a runtime directory holding the fake crw, the fake relay and the version
// the runtime reports.
func (h *upgradeEnv) installRuntime(dir, version string) {
	h.t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		h.t.Fatal(err)
	}
	for _, name := range []string{"crw", "codex-session-relay"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(h.script), 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "version"), []byte(version+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// installPointer places the owned pointer at the runtime the harness calls previous.
func (h *upgradeEnv) installPointer(want bool) {
	h.t.Helper()
	if !want {
		return
	}
	if err := os.Symlink(h.previous, h.pointerLink); err != nil {
		h.t.Fatal(err)
	}
}

// run drives the registered command with this harness's own Env, whose clock the harness holds:
// the run directory is named for the second the run started in, so a test that needs two runs
// inside one second pins Env.Now instead of racing the wall clock.
func (h *upgradeEnv) run(args ...string) int {
	h.t.Helper()
	old := upgradeConfig
	upgradeConfig = func(e *Env) *Config {
		cfg := coreDefaults(e)
		cfg.Repository = "owner/repo"
		cfg.StateDir = filepath.Join(h.home, "manage-state")
		cfg.Relay.State = h.state
		cfg.Relay.Socket = filepath.Join(h.codex, "app-server-control.sock")
		return cfg
	}
	h.t.Cleanup(func() { upgradeConfig = old })
	var out, errOut strings.Builder
	e := &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		Getenv:     os.Getenv,
		Now:        func() time.Time { return h.now },
		Executable: coreExecutable(),
	}
	defer coreForgetConfig(e)
	return upgradeCommand.Run(context.Background(), e, args)
}

// callArgs is the arguments of every call the fake answered, one string per call.
func (h *upgradeEnv) callArgs() []string {
	h.t.Helper()
	return upgradeLines(h.t, h.calls)
}

// callLines is one line per call the fake answered, as "<executable><TAB><arguments>".
func (h *upgradeEnv) callLines() []string {
	h.t.Helper()
	return upgradeLines(h.t, h.exeCalls)
}

func upgradeLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (h *upgradeEnv) crwCalls() []string    { return strings.Fields(strings.Join(h.callArgs(), " ")) }
func (h *upgradeEnv) ghCallLines() []string { return upgradeLines(h.t, h.ghCalls) }

// calledFrom reports whether the fake was reached as exe and asked for the verb.
func (h *upgradeEnv) calledFrom(exe, verb string) bool {
	h.t.Helper()
	resolved := exe
	if target, err := filepath.EvalSymlinks(exe); err == nil {
		resolved = target
	}
	for _, line := range h.callLines() {
		got, args, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		if same, err := filepath.EvalSymlinks(got); err == nil {
			got = same
		}
		if got == resolved && strings.Contains(args, verb) {
			return true
		}
	}
	return false
}

// recordDirs is the run directories the command left behind.
func (h *upgradeEnv) recordDirs() []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.home, "manage-state", "upgrades"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		h.t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

// recordJSON is the run's record.json as written, so a case can read the keys the record adds
// without depending on the struct.
func (h *upgradeEnv) recordJSON(t *testing.T) map[string]any {
	t.Helper()
	root := filepath.Join(h.home, "manage-state", "upgrades")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the record directory holds %d entries, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(root, entries[0].Name(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func (h *upgradeEnv) recordOf(t *testing.T) upgradeRecord {
	t.Helper()
	root := filepath.Join(h.home, "manage-state", "upgrades")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the record directory holds %d entries, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(root, entries[0].Name(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record upgradeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// upgradeGhPathsFor answers the forge calls a released version makes: the commit ref the version
// names, and the dev-gate check runs of that commit.
func upgradeGhPathsFor(version, commit string) map[string]upgradeGhAnswer {
	out := map[string]upgradeGhAnswer{}
	for _, ref := range upgradeCommitRefs(version) {
		out["repos/owner/repo/commits/"+ref] = upgradeGhAnswer{Body: "{\"sha\":\"" + commit + "\"}"}
	}
	out["repos/owner/repo/commits/"+commit+"/check-runs"] = upgradeGhAnswer{Body: "{\"check_runs\":[{\"name\":\"dev-gate\",\"conclusion\":\"success\"}]}"}
	return out
}

// upgradeGhPaths answers the forge calls the harness's default version makes.
func upgradeGhPaths(commit string) map[string]upgradeGhAnswer {
	return upgradeGhPathsFor(upgradeFakeVersion, commit)
}

// The command prints its usage: exit 0 for the help flags, exit 2 for anything unusable.
func TestUpgradeCommandPrintsItsUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-h"}, 0}, {[]string{"--help"}, 0},
		{[]string{"nope"}, usageExit}, {nil, usageExit},
	} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), append([]string{"runtime-upgrade"}, tc.args...), strings.NewReader(""), &out, &errOut); code != tc.code {
			t.Errorf("%q: exit %d, want %d", tc.args, code, tc.code)
		}
	}
}

// A released tag version resolves through the tag ref (and its v-prefixed form); a git-describe
// version resolves through its short hash.
func TestUpgradeCommitRefs(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"v0.4.0-4633-geb2567df7", "eb2567df7"},
		{"v0.4.1", "v0.4.1"},
		{"0.4.1", "0.4.1,v0.4.1"},
		{"", ""},
	} {
		if got := strings.Join(upgradeCommitRefs(tc.version), ","); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.version, got, tc.want)
		}
	}
}

// An entry through an existing symlink is refused, so a crafted archive cannot reach a file
// outside the extract directory.
func TestUpgradeExtractRefusesASymlinkEscape(t *testing.T) {
	dir, outside := t.TempDir(), filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "crw")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, upgradeTarGz(t, map[string]string{"crw": "overwritten"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upgradeExtract(archive, dir); err == nil {
		t.Fatal("an entry through a symlink was accepted")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "original" {
		t.Errorf("the file outside the extract directory changed: %q %v", data, err)
	}
}

// The full success flow for both version shapes: with the pointer in place, an update that
// installs what it reports and moves the pointer to it, and a service that runs and matches, every
// step runs and the command exits 0.
func TestUpgradeSucceedsOnAHealthyHost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
	}{{"a git-describe version", upgradeFakeVersion}, {"a released tag", "v0.4.1"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := upgradeHarness(t, upgradeHarnessOptions{version: tc.version, gh: upgradeGhPathsFor(tc.version, upgradeGoodCommit),
				pointer: true, produceRuntime: true, pointAtIt: true})
			if code := h.run("--release-dir", h.release); code != 0 {
				t.Fatalf("exit %d, want 0; the record is %+v", code, h.recordOf(t))
			}
			record := h.recordOf(t)
			if record.Outcome != "ok" || record.Reason != "" {
				t.Errorf("the record is %+v", record)
			}
			if len(record.Reasons) != 0 {
				t.Errorf("a healthy run named the reasons %v", record.Reasons)
			}
			if record.StartFrom != h.installed {
				t.Errorf("the restart used %q, want the runtime the update installed %q", record.StartFrom, h.installed)
			}
			var steps []string
			for _, s := range record.Steps {
				steps = append(steps, s.Step)
			}
			joined := strings.Join(steps, ",")
			for _, want := range []string{upgradeStepSums, upgradeStepExtract, upgradeStepCommit, upgradeStepDevGate,
				upgradeStepAttempts, upgradeStepSnapshot, upgradeStepStop, upgradeStepUpdate, upgradeStepStart, upgradeStepPostCheck} {
				if !strings.Contains(joined, want) {
					t.Errorf("the record does not name the step %q: %v", want, joined)
				}
			}
		})
	}
}
