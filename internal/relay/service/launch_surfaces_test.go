package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/sync" // registers packet-check
)

// Every surface that reports or enforces the launch declaration reads it through
// ResolveLaunchPolicyAt, so each answers the resolution (the trees of
// testdata/fixtures/launch_policy.json) and refuses before whatever it would otherwise do first,
// as cli.py main does: doctor's and status's launchPolicy, packet-check's receive step, and a
// supervisor started with `service run`. The goldens began as Python's resolution, the refusal
// LaunchRefusal made of it, and the Python console script's bytes for the same tree.

type relayRun struct {
	code   int
	stdout string
}

func relay(t *testing.T, argv ...string) relayRun {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Execute(context.Background(), argv, &out, &errOut)
	return relayRun{code, out.String()}
}

// launchTree builds fixture case name under a fresh root, with this process's working
// directory, HOME and policy variable set as the capture set them.
func launchTree(t *testing.T, fixture launchFixture, name string) (root, state string, c launchCase) {
	t.Helper()
	c, ok := fixture.Cases[name]
	if !ok {
		t.Fatalf("no fixture case %q", name)
	}
	root = t.TempDir()
	state = buildLaunchCase(t, root, fixture, c)
	t.Chdir(root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv(execution.EnvPolicy, "")
	if c.Environment != nil {
		t.Setenv(execution.EnvPolicy, strings.ReplaceAll(*c.Environment, "<R>", root))
	}
	return root, state, c
}

// block is report[key] of a relay answer, printed alone as the relay prints a document
// (json.loads semantics, so a NaN the record carries survives the cut).
func block(t *testing.T, stdout, key string, drop ...string) string {
	t.Helper()
	decoded, err := store.LoadsJSON([]byte(stdout))
	if err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	for _, field := range decoded.(service.Object) {
		if field.Key == key {
			value, _ := field.Value.(service.Object)
			kept := service.Object{}
			for _, inner := range value {
				if !slices.Contains(drop, inner.Key) {
					kept = append(kept, inner)
				}
			}
			return emitted(t, kept)
		}
	}
	t.Fatalf("no %s in %s", key, stdout)
	return ""
}

func TestLaunchPolicy_doctor_and_status_report_the_one_resolution(t *testing.T) {
	fixture := loadLaunchFixture(t)
	// Each of these once read differently somewhere: undecodable bytes, a NaN in the record,
	// two files named at once, a policy nested past the parser, a mode-0 declaration.
	for _, name := range []string{"utf8-invalid-start", "record-nan", "conflict", "policy-deep-object", "unreadable-eacces", "record-trailing-slash"} {
		t.Run(name, func(t *testing.T) {
			if name == "unreadable-eacces" && os.Geteuid() == 0 {
				t.Skip("root reads a mode-0 file")
			}
			root, state, _ := launchTree(t, fixture, name)
			doctor := strings.ReplaceAll(block(t, relay(t, "--state", state, "--json", "doctor").stdout, "launchPolicy"), root, "<R>")
			status := relay(t, "--state", state, "service", "status")
			if got := strings.ReplaceAll(block(t, status.stdout, "launchPolicy", "appliesTo", "runningDigest", "matchesRunning"), root, "<R>"); got != doctor {
				t.Errorf("status\n%s\ndoctor\n%s", got, doctor)
			}
			checkFromPackage(t, "resolution", doctor)
		})
	}
}

// doctor's own rolePolicy reads the variable through the same parser and depth rule.
func TestLaunchPolicy_doctor_role_policy_reads_as_the_launch_does(t *testing.T) {
	fixture := loadLaunchFixture(t)
	root, state, _ := launchTree(t, fixture, "policy-deep-object")
	t.Setenv(execution.EnvPolicy, filepath.Join(root, "policy.json"))
	if err := os.Remove(filepath.Join(state, "launch-policy.json")); err != nil {
		t.Fatal(err)
	}
	var role struct{ State, Detail string }
	if err := json.Unmarshal([]byte(block(t, relay(t, "--state", state, "--json", "doctor").stdout, "rolePolicy")), &role); err != nil {
		t.Fatal(err)
	}
	if role.State != "unresolved" {
		t.Fatalf("rolePolicy %+v", role)
	}
	// The detail the launch resolution gives for the file (its golden began as Python's).
	checkFromPackage(t, "detail", strings.ReplaceAll(role.Detail, root, "<R>"))
}

func TestLaunchPolicy_the_receive_check_settles_the_declaration_first(t *testing.T) {
	fixture := loadLaunchFixture(t)
	// A refused declaration answers before a packet that is not even there is read.
	root, state, _ := launchTree(t, fixture, "utf8-invalid-start")
	got := relay(t, "--state", state, "packet-check", "--packet", filepath.Join(root, "absent.json"), "--receiver", "task")
	if got.code != 2 {
		t.Fatalf("exit %d\n%s", got.code, got.stdout)
	}
	refusal := strings.ReplaceAll(got.stdout, root, "<R>")
	// An empty --receiver still reads the store but is not the receive check main settles a
	// declaration for, so a conflicting one does not answer for it (the Python answer).
	root, state, _ = launchTree(t, fixture, "conflict")
	packet := filepath.Join(root, "packet.json")
	if err := os.WriteFile(packet, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = relay(t, "--state", state, "packet-check", "--packet", packet, "--receiver", "")
	want := "{\n  \"error\": \"refused\",\n  \"reason\": \"malformed_receipt\",\n  \"detail\": \"a packet carries a relay-envelope/1 region under envelope, not a NoneType\"\n}\n"
	if got.code != 2 || got.stdout != want {
		t.Fatalf("empty receiver: exit %d\n%s", got.code, got.stdout)
	}
	// os.environ refuses the recorded path before anything reads it.
	_, state, _ = launchTree(t, fixture, "record-nul")
	got = relay(t, "--state", state, "packet-check", "--packet", packet, "--receiver", "task")
	if want := "{\n  \"error\": \"host\",\n  \"detail\": \"ValueError: embedded null byte\"\n}\n"; got.code != 3 || got.stdout != want {
		t.Fatalf("NUL: exit %d\n%s", got.code, got.stdout)
	}
	// The refusal LaunchRefusal makes of the resolution (its golden began as Python's).
	checkFromPackage(t, "refusal", refusal)
}

func TestLaunchPolicy_service_run_is_refused_before_it_asks_for_a_host(t *testing.T) {
	fixture := loadLaunchFixture(t)
	// The refusals LaunchRefusal makes of the resolution (their goldens began as Python's).
	refusals := map[string]string{}
	for _, argv := range [][]string{{"service", "run"}, {"--socket", "app.sock", "service", "run", "--segment-seconds", "0"}} {
		root, state, _ := launchTree(t, fixture, "utf8-invalid-start")
		got := relay(t, append([]string{"--state", state}, argv...)...)
		if got.code != 2 {
			t.Fatalf("%v: exit %d\n%s", argv, got.code, got.stdout)
		}
		refusals[strings.Join(argv, " ")] = strings.ReplaceAll(got.stdout, root, "<R>")
	}
	// A run its own launch settled (the id matches, as str.strip()ped) is not asked again.
	_, state, _ := launchTree(t, fixture, "utf8-invalid-start")
	t.Setenv(service.SettledEnv, " launch-7 ")
	got := relay(t, "--state", state, "service", "run", "--launch-id", "launch-7")
	if want := "{\n  \"error\": \"usage\",\n  \"detail\": \"this command needs --socket to reach the host\"\n}\n"; got.code != 4 || got.stdout != want {
		t.Fatalf("settled: exit %d\n%s", got.code, got.stdout)
	}
	// Another launch's settlement says nothing about this one.
	root, state, _ := launchTree(t, fixture, "utf8-invalid-start")
	t.Setenv(service.SettledEnv, "launch-6")
	got = relay(t, "--state", state, "service", "run", "--launch-id", "launch-7")
	if got.code != 2 {
		t.Fatalf("another launch's settlement: exit %d\n%s", got.code, got.stdout)
	}
	refusals["another launch's settlement"] = strings.ReplaceAll(got.stdout, root, "<R>")
	t.Setenv(service.SettledEnv, "")
	_, state, _ = launchTree(t, fixture, "record-nul")
	got = relay(t, "--state", state, "service", "run")
	if want := "{\n  \"error\": \"host\",\n  \"detail\": \"embedded null byte\"\n}\n"; got.code != 3 || got.stdout != want {
		t.Fatalf("NUL: exit %d\n%s", got.code, got.stdout)
	}
	for _, key := range []string{"service run", "--socket app.sock service run --segment-seconds 0", "another launch's settlement"} {
		checkFromPackage(t, key, refusals[key])
	}
}

func digestOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// service declare records Path(value).expanduser().absolute(): the '..' the operator wrote
// stays, so after a symlink the recorded file is the one just checked, not its lexical
// neighbour (Python writes that spelling; a cleaned one named another file here).
func TestLaunchPolicy_declare_records_the_path_as_spelled(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv(execution.EnvPolicy, "")
	far := filepath.Join(root, "far")
	for path, text := range map[string]string{
		filepath.Join(root, "good.json"): `{"roles":{"parent":{"model":"m","reasoningEffort":"high"}}}`,
		filepath.Join(far, "good.json"):  `{"roles":{"parent":{"model":"far","reasoningEffort":"high"}}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(far, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(far, "deep"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "S")
	for _, spelled := range []string{filepath.Join(root, "link") + "/../good.json", "link/../good.json"} {
		got := relay(t, "--state", state, "service", "declare", "--execution-policy", spelled)
		var answer struct {
			Declared     struct{ Path string }
			LaunchPolicy struct{ Path, Digest string }
		}
		if err := json.Unmarshal([]byte(got.stdout), &answer); err != nil || got.code != 0 {
			t.Fatalf("%s: exit %d %v\n%s", spelled, got.code, err, got.stdout)
		}
		want := spelled
		if !filepath.IsAbs(want) {
			want = root + "/" + want
		}
		var record struct{ Path string }
		raw, err := os.ReadFile(filepath.Join(state, "launch-policy.json"))
		if err == nil {
			err = json.Unmarshal(raw, &record)
		}
		if err != nil {
			t.Fatal(err)
		}
		if answer.Declared.Path != want || record.Path != want || answer.LaunchPolicy.Path != want {
			t.Fatalf("recorded %q, answered %q, want %q", record.Path, answer.Declared.Path, want)
		}
		if reached := digestOf(t, filepath.Join(far, "good.json")); answer.LaunchPolicy.Digest != reached {
			t.Fatalf("digest %s is not the file the spelling reaches (%s)", answer.LaunchPolicy.Digest, reached)
		}
	}
	// An unknown ~user is a host error (decision R3C-4).
	got := relay(t, "--state", state, "service", "declare", "--execution-policy", "~no-such-user-t32/policy.json")
	if want := "{\n  \"error\": \"host\",\n  \"detail\": \"Could not determine home directory.\"\n}\n"; got.code != 3 || got.stdout != want {
		t.Fatalf("~user: exit %d\n%s", got.code, got.stdout)
	}
}

// A directory where the declaration belongs is never replaced or removed: the rename and the
// unlink both answer that it is a directory, a host error (exit 3).
func TestLaunchPolicy_a_directory_in_place_of_the_declaration_stays(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv(execution.EnvPolicy, "")
	policy := filepath.Join(root, "good.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"parent":{"model":"m","reasoningEffort":"high"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "S")
	target := filepath.Join(state, "launch-policy.json")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	got := relay(t, "--state", state, "service", "declare", "--forget-execution-policy")
	if want := "{\n  \"error\": \"host\",\n  \"detail\": \"unlink " + target + ": is a directory\"\n}\n"; got.code != 3 || got.stdout != want {
		t.Fatalf("forget: exit %d\n%s", got.code, got.stdout)
	}
	got = relay(t, "--state", state, "service", "declare", "--execution-policy", policy)
	replace := regexp.MustCompile(`^\{\n  "error": "host",\n  "detail": "rename ` + regexp.QuoteMeta(state) + `/\.launch-policy\.json\.[0-9a-f]+ ` + regexp.QuoteMeta(target) + `: is a directory"\n\}\n$`)
	if got.code != 3 || !replace.MatchString(got.stdout) {
		t.Fatalf("declare: exit %d\n%s", got.code, got.stdout)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("the directory did not stay: %v", err)
	}
}

// The state directory is joined as spelled: through a symlink and '..' the declaration lands
// in the directory the kernel resolves that spelling to, as Python's selection.path / name does.
func TestLaunchPolicy_state_files_follow_the_spelled_state_directory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv(execution.EnvPolicy, "")
	policy := filepath.Join(root, "good.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"parent":{"model":"m","reasoningEffort":"high"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(root, "far", "deep"), filepath.Join(root, "far", "S"), filepath.Join(root, "S")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "far", "deep"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	spelled := filepath.Join(root, "link") + "/../S"
	if got := relay(t, "--state", spelled, "service", "declare", "--execution-policy", policy); got.code != 0 {
		t.Fatalf("exit %d\n%s", got.code, got.stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "far", "S", "launch-policy.json")); err != nil {
		t.Fatalf("the declaration is not where the spelling leads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "S", "launch-policy.json")); err == nil {
		t.Fatal("the declaration landed in the lexically cleaned directory")
	}
}

// A declaration recording a path that holds NUL reaches `service start` and `service restart`
// resolved and not refused (it names a file), so the launcher meets it assigning the child's
// environment, after it opened daemon.log: subprocess.Popen's ValueError, exit 3, the log
// created and empty, no child.
func TestLaunchPolicy_start_and_restart_meet_a_recorded_NUL_as_the_launcher_does(t *testing.T) {
	fixture := loadLaunchFixture(t)
	for _, command := range []string{"start", "restart"} {
		_, state, _ := launchTree(t, fixture, "record-nul")
		if got := relay(t, "--state", state, "--socket", "app.sock", "service", "enable"); got.code != 0 {
			t.Fatalf("enable: exit %d\n%s", got.code, got.stdout)
		}
		got := relay(t, "--state", state, "--socket", "app.sock", "service", command, "--allow-isolated-scope")
		if want := "{\n  \"error\": \"host\",\n  \"detail\": \"embedded null byte\"\n}\n"; got.code != 3 || got.stdout != want {
			t.Fatalf("%s: exit %d\n%s", command, got.code, got.stdout)
		}
		if log, err := os.ReadFile(filepath.Join(state, "daemon.log")); err != nil || len(log) != 0 {
			t.Fatalf("%s: daemon.log %q (%v)", command, log, err)
		}
		// The files the Python console script leaves for the same tree and commands.
		entries, err := os.ReadDir(state)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if want := []string{"daemon.lock", "daemon.log", "launch-policy.json", "service.json"}; !slices.Equal(names, want) {
			t.Fatalf("%s: state holds %v, python %v", command, names, want)
		}
	}
}
