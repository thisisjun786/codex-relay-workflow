package doctor_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hostenv "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// The retrust tests never touch the real Codex home: every one builds a temporary home with a fake
// codex on PATH and a plugin package that declares two command hooks, and points HOME, CODEX_HOME and
// CRW_HOME into it (the operator rule after CRW-499). Two hooks are declared because the oracle's
// safety pin refuses a plugin whose every existing entry is drifted, so the update path needs one
// entry that still matches. TestHookTrustRetrust_real_home_is_untouched compares the real ~/.codex
// and ~/.crw listings before and after the file runs.

type retrustFixture struct {
	t       *testing.T
	root    string
	home    string
	plugin  string
	bin     string
	key     string
	entries []doctor.HookTrustEntry
}

const retrustKey = "crw@local"

func newRetrustFixture(t *testing.T, config string) *retrustFixture {
	t.Helper()
	root := t.TempDir()
	f := &retrustFixture{
		t:      t,
		root:   root,
		home:   filepath.Join(root, "codex"),
		plugin: filepath.Join(root, "plugin"),
		bin:    filepath.Join(root, "bin"),
		key:    retrustKey,
	}
	for _, dir := range []string{f.home, filepath.Join(f.plugin, ".codex-plugin"), filepath.Join(f.plugin, "hooks"), f.bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(f.plugin, ".codex-plugin", "plugin.json"), `{"name":"crw","hooks":["./hooks/one.json","./hooks/two.json"]}`+"\n")
	f.write(filepath.Join(f.plugin, "hooks", "one.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo one"}]}]}}`+"\n")
	f.write(filepath.Join(f.plugin, "hooks", "two.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo two"}]}]}}`+"\n")
	f.writeExec(filepath.Join(f.bin, "codex"), "#!/bin/sh"+"\n"+"exit 0"+"\n")
	// The verification probe runs the codex the runner names on the process PATH (the oracle
	// passes process.env), so the fake binary has to be findable there.
	t.Setenv("PATH", f.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if config != "" {
		f.write(filepath.Join(f.home, "config.toml"), config)
	}
	entries, err := doctor.ListHookTrustEntries(f.plugin, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the fixture plugin declares %d entries, want 2", len(entries))
	}
	f.entries = entries
	return f
}

func (f *retrustFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *retrustFixture) writeExec(path, content string) {
	f.t.Helper()
	f.write(path, content)
	if err := os.Chmod(path, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *retrustFixture) read(path string) string {
	f.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

func (f *retrustFixture) config() string { return filepath.Join(f.home, "config.toml") }

func (f *retrustFixture) installed() string {
	return "model = " + q("gpt-5.5") + "\n" + "\n" + "[plugins." + q(f.key) + "]" + "\n" + "enabled = true" + "\n"
}

// q is the double-quoted spelling of one value.
func q(value string) string { return `"` + value + `"` }

// section is one [hooks.state."<key>"] trusted_hash section for the fixture's nth entry.
func (f *retrustFixture) section(n int, hash string) string {
	return "[hooks.state." + q(f.entries[n].Key) + "]" + "\n" + "trusted_hash = " + q(hash) + "\n"
}

func (f *retrustFixture) backupName() string {
	return f.config() + ".bak-2026-01-01T00-00-00.000Z"
}

// env is the environment the command reads: HOME, CODEX_HOME and CRW_HOME are the temporary home.
func (f *retrustFixture) env() hostenv.LookupEnv {
	vars := map[string]string{
		"HOME":       f.root,
		"CODEX_HOME": f.home,
		"CRW_HOME":   filepath.Join(f.root, "crw"),
		"PATH":       f.bin,
	}
	return func(key string) (string, bool) { value, ok := vars[key]; return value, ok }
}

// now is the instant the backup name embeds, so the tests do not depend on the wall clock.
func (f *retrustFixture) now() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

// run executes the command with the real subprocess runner: the fake codex is a script on the
// process PATH, so the environment seam the verification needs is exercised for real.
func (f *retrustFixture) run(args ...string) (string, string, int) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := doctor.HookTrustRetrustCLI(args, &stdout, &stderr, f.env(), retrustRunner, f.plugin, f.now())
	return stdout.String(), stderr.String(), code
}

// retrustRunner runs file with argv under env, the shape of the product's own runner.
func retrustRunner(file string, args []string, env []string) doctor.HookTrustRetrustRun {
	command := exec.Command(file, args...)
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	run := doctor.HookTrustRetrustRun{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		zero := 0
		run.Status = &zero
		return run
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		run.Status = &code
		return run
	}
	run.Error = err.Error()
	return run
}

func TestHookTrustRetrust_refuses_without_bootstrap_then_writes(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())

	stdout, stderr, code := f.run()
	if code != 1 || stderr != "crw doctor retrust: no existing hook trust entries match this plugin key; pass --bootstrap-ok to initialize trust"+"\n" {
		t.Fatalf("refusal: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// CRW-844: the refusal reports the items it would have changed, so the operator sees what
	// --bootstrap-ok would do without running it. It writes nothing.
	for _, entry := range f.entries {
		if !strings.Contains(stdout, entry.Key) {
			t.Fatalf("the refusal does not name the planned item %q: %q", entry.Key, stdout)
		}
	}
	if !strings.Contains(stdout, "config.toml unchanged") {
		t.Fatalf("the refusal does not say nothing was published: %q", stdout)
	}
	if f.read(f.config()) != original {
		t.Fatal("the refused run changed config.toml")
	}
	if _, err := os.Stat(f.backupName()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused run left a backup: %v", err)
	}

	stdout, stderr, code = f.run("--bootstrap-ok")
	if code != 0 || stderr != "" {
		t.Fatalf("bootstrap: code=%d stderr=%q", code, stderr)
	}
	want := ""
	for _, entry := range f.entries {
		want += "[trusted] " + entry.Key + " expected=" + entry.Hash + " actual=" + entry.Hash + "\n"
	}
	want += "updated=0 appended=2" + "\n" + "backup: " + f.backupName() + "\n"
	if !strings.HasPrefix(stdout, want) {
		t.Fatalf("bootstrap stdout: got %q want prefix %q", stdout, want)
	}
	content := f.read(f.config())
	if !strings.HasPrefix(content, f.installed()) {
		t.Fatalf("the write changed what was already there:\n%s", content)
	}
	for n := range f.entries {
		if !strings.Contains(content, f.section(n, f.entries[n].Hash)) {
			t.Fatalf("config.toml does not carry entry %d:\n%s", n, content)
		}
	}
	if got := f.read(f.backupName()); got != original {
		t.Fatalf("the backup does not hold the original bytes: %q", got)
	}
}

func TestHookTrustRetrust_updates_a_drifted_hash(t *testing.T) {
	f := newRetrustFixture(t, "")
	// One entry still matches, so the safety pin passes; the other is drifted and is rewritten.
	// The oracle rewrites the value of EVERY entry whose section exists, so both count as updated.
	f.write(f.config(), f.installed()+"\n"+f.section(0, f.entries[0].Hash)+"\n"+f.section(1, "sha256:"+strings.Repeat("0", 64)))

	stdout, stderr, code := f.run()
	if code != 0 || stderr != "" {
		t.Fatalf("update: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "updated=2 appended=0"+"\n") {
		t.Fatalf("the answer does not report one update: %q", stdout)
	}
	content := f.read(f.config())
	if !strings.Contains(content, f.section(1, f.entries[1].Hash)) {
		t.Fatalf("the drifted hash was not rewritten:\n%s", content)
	}
	if strings.Contains(content, strings.Repeat("0", 64)) {
		t.Fatalf("the drifted hash is still there:\n%s", content)
	}
}

func TestHookTrustRetrust_keeps_crlf_content(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), "model = "+q("gpt-5.5")+"\r\n\r\n[plugins."+q(f.key)+"]\r\nenabled = true\r\n")

	if _, stderr, code := f.run("--bootstrap-ok"); code != 0 {
		t.Fatalf("bootstrap: code=%d stderr=%q", code, stderr)
	}
	got := f.read(f.config())
	for n := range f.entries {
		want := "[hooks.state." + q(f.entries[n].Key) + "]\r\ntrusted_hash = " + q(f.entries[n].Hash) + "\r\n"
		if !strings.Contains(got, want) {
			t.Fatalf("the appended block did not keep CRLF for entry %d: %q", n, got)
		}
	}
}

func TestHookTrustRetrust_inserts_before_an_existing_hook_state_section(t *testing.T) {
	f := newRetrustFixture(t, "")
	// An exact section for an unrelated hook: the missing block is inserted before it, with the
	// separator the oracle adds and a newline before the remainder (hook-trust.ts:352-357).
	other := "[hooks.state." + q("crw@local:other.json:stop:0:0") + "]" + "\n" + "trusted_hash = " + q("sha256:"+strings.Repeat("1", 64)) + "\n"
	f.write(f.config(), f.installed()+"\n"+other)

	if _, stderr, code := f.run("--bootstrap-ok"); code != 0 {
		t.Fatalf("bootstrap: code=%d stderr=%q", code, stderr)
	}
	want := f.installed() + "\n"
	for n := range f.entries {
		want += f.section(n, f.entries[n].Hash) + "\n"
	}
	want += other
	if got := f.read(f.config()); got != want {
		t.Fatalf("insert-before branch: got %q want %q", got, want)
	}
}

func TestHookTrustRetrust_refuses_a_duplicate_section_header(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed()+"\n"+f.section(0, f.entries[0].Hash)+"\n"+f.section(0, f.entries[0].Hash))
	original := f.read(f.config())

	_, stderr, code := f.run()
	if code != 1 || stderr != "crw doctor retrust: duplicate section header: [hooks.state."+q(f.entries[0].Key)+"]"+"\n" {
		t.Fatalf("duplicate refusal: code=%d stderr=%q", code, stderr)
	}
	if f.read(f.config()) != original {
		t.Fatal("the refused run changed config.toml")
	}
}

func TestHookTrustRetrust_refuses_a_missing_trusted_hash(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed()+"\n"+"[hooks.state."+q(f.entries[0].Key)+"]"+"\n"+"other = true"+"\n")

	_, stderr, code := f.run()
	if code != 1 || stderr != "crw doctor retrust: missing trusted_hash in [hooks.state."+q(f.entries[0].Key)+"]"+"\n" {
		t.Fatalf("missing-hash refusal: code=%d stderr=%q", code, stderr)
	}
}

// CRW-844: the oracle wrote, verified and rolled back. The port verifies next in a temporary Codex
// home first, so a verification failure leaves config.toml exactly as it was, writes no backup, and
// still prints the plan.
func TestHookTrustRetrust_verification_failure_leaves_the_config(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())
	f.writeExec(filepath.Join(f.bin, "codex"), "#!/bin/sh"+"\n"+"exit 1"+"\n")

	stdout, stderr, code := f.run("--bootstrap-ok")
	if code != 1 {
		t.Fatalf("verification failure: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "pre-write verification failed: codex features list verification failed: ; config.toml unchanged") {
		t.Fatalf("verification failure stderr: %q", stderr)
	}
	for _, entry := range f.entries {
		if !strings.Contains(stdout, entry.Key) {
			t.Fatalf("the refusal does not name the planned item %q: %q", entry.Key, stdout)
		}
	}
	if !strings.Contains(stdout, "config.toml unchanged") {
		t.Fatalf("the refusal does not say nothing was published: %q", stdout)
	}
	if got := f.read(f.config()); got != original {
		t.Fatalf("the refused run changed config.toml: %q", got)
	}
	if _, err := os.Stat(f.backupName()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused run left a backup: %v", err)
	}
}

// A config.toml the process cannot open for writing is refused before anything is written (the
// CRW-427 rule the port keeps), and the report names what would have been published.
func TestHookTrustRetrust_refuses_an_unwritable_config(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())
	if err := os.Chmod(f.config(), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.config(), 0o644) })
	if probe, err := os.OpenFile(f.config(), os.O_WRONLY, 0); err == nil {
		_ = probe.Close()
		t.Skip("this process can write a mode 0444 file (running as root?), so an unwritable config cannot be provoked")
	}

	_, stderr, code := f.run("--bootstrap-ok")
	if code != 1 {
		t.Fatalf("unwritable config: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "permission denied") {
		t.Fatalf("the refusal does not name the cause: %q", stderr)
	}
	if got := f.read(f.config()); got != original {
		t.Fatalf("the refused run changed config.toml: %q", got)
	}
	if _, err := os.Stat(f.backupName()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused run left a backup: %v", err)
	}
}

func TestHookTrustRetrust_writes_a_symlinked_config_target(t *testing.T) {
	f := newRetrustFixture(t, "")
	target := filepath.Join(f.root, "real-config.toml")
	f.write(target, f.installed())
	if err := os.Symlink(target, f.config()); err != nil {
		t.Fatal(err)
	}

	if _, stderr, code := f.run("--bootstrap-ok"); code != 0 {
		t.Fatalf("symlinked config: code=%d stderr=%q", code, stderr)
	}
	if info, err := os.Lstat(f.config()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	if got := f.read(target); !strings.Contains(got, "trusted_hash = "+q(f.entries[0].Hash)) {
		t.Fatalf("the target was not rewritten:\n%s", got)
	}
}

// A settings writer that publishes while the verification probe runs is caught by the last check
// before the exchange (CRW-844): the publication is refused, nothing is written, and no backup is
// made for bytes that are already stale.
func TestHookTrustRetrust_keeps_a_concurrent_edit_during_the_probe(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	concurrent := "model = \"written-by-another-process\"\n"
	runner := func(string, []string, []string) doctor.HookTrustRetrustRun {
		// Another settings writer publishes during the probe.
		if err := os.WriteFile(f.config(), []byte(concurrent), 0o644); err != nil {
			t.Fatal(err)
		}
		zero := 0
		return doctor.HookTrustRetrustRun{Status: &zero}
	}
	_, _, err := doctor.HookTrustRetrust(f.home, f.plugin, f.key, true, runner, f.env(), f.now())
	if err == nil || !strings.Contains(err.Error(), "changed after it was read") {
		t.Fatalf("the concurrent edit was not reported: %v", err)
	}
	if got := f.read(f.config()); got != concurrent {
		t.Fatalf("the concurrent edit was replaced: %q", got)
	}
	if _, statErr := os.Stat(f.backupName()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a backup was made for bytes that were already stale: %v", statErr)
	}
}

// TestHookTrustRetrust_resolves_the_plugin_root_from_the_cache covers the user-run path: with no
// PLUGIN_ROOT and no --plugin-root, the one plugin package under the Codex home's plugin cache is
// the package the hooks are read from.
func TestHookTrustRetrust_resolves_the_plugin_root_from_the_cache(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	cached := filepath.Join(f.home, "plugins", "cache", "local", "crw", "0.4.0")
	for _, rel := range []string{".codex-plugin/plugin.json", "hooks/one.json", "hooks/two.json"} {
		src := filepath.Join(f.plugin, rel)
		dst := filepath.Join(cached, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := doctor.HookTrustRetrustCLI([]string{"--bootstrap-ok"}, &stdout, &stderr, f.env(), retrustRunner, "", f.now())
	if code != 0 {
		t.Fatalf("cache resolution: code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated=0 appended=2") {
		t.Fatalf("the cached plugin's hooks were not trusted: %q", stdout.String())
	}
}

// TestHookTrustRetrust_keeps_a_concurrent_edit_on_rollback covers the guard the port adds over the
// oracle: a settings writer that publishes while the verification probe runs is not replaced by the
// rollback, which reports the conflict and keeps the backup instead.
func TestHookTrustRetrustCLI_unknown_option(t *testing.T) {
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	_, stderr, code := f.run("--nope")
	if code != 1 || stderr != "crw doctor retrust: unknown hooks option: --nope"+"\n" {
		t.Fatalf("unknown option: code=%d stderr=%q", code, stderr)
	}
	_, stderr, code = f.run("--help")
	if code != 1 || stderr != "crw doctor retrust: unknown hooks option: --help"+"\n" {
		t.Fatalf("--help is not help: code=%d stderr=%q", code, stderr)
	}
}

// TestHookTrustRetrust_real_home_is_untouched is the operator rule after CRW-499: the real ~/.codex
// and ~/.crw listings must not change while these tests run.
func TestHookTrustRetrust_real_home_is_untouched(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no real home to compare")
	}
	before := realHomeListing(home)
	f := newRetrustFixture(t, "")
	f.write(f.config(), f.installed())
	if _, stderr, code := f.run("--bootstrap-ok"); code != 0 {
		t.Fatalf("bootstrap: code=%d stderr=%q", code, stderr)
	}
	if after := realHomeListing(home); before != after {
		t.Fatalf("the real home changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func realHomeListing(home string) string {
	var lines []string
	for _, name := range []string{".codex", ".crw"} {
		root := filepath.Join(home, name)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(home, path)
			info, err := d.Info()
			if err != nil {
				return nil
			}
			lines = append(lines, rel+" "+info.Mode().String())
			return nil
		})
	}
	return strings.Join(lines, "\n")
}
