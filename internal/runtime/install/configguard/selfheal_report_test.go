package configguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/execfile"
)

// The J4 rule (Jun, 2026-10-06) makes this hook report-only: it diagnoses the declared [features]
// state and warns when a soft key is off, and it never writes config.toml, the self-heal marker or
// the install manifest, makes no backup, and never runs "codex features enable". These tests hold
// that line, and the oracle's silent paths (an opt-out marker, an all-enabled cache, a key the user
// turned off after a heal, an unreadable listing, an absent codex).

// selfHealReportSoftOff is the declared soft flag reading false: the one case that warns.
const selfHealReportSoftOff = "default_mode_request_user_input  under development  false\n" +
	"goals                            stable             true\n" +
	"hooks                            stable             true\n" +
	"multi_agent                      stable             true\n"

// selfHealReportSoftOn is every declared flag on: nothing to say.
const selfHealReportSoftOn = "default_mode_request_user_input  under development  true\n" +
	"goals                            stable             true\n" +
	"hooks                            stable             true\n" +
	"multi_agent                      stable             true\n"

const selfHealReportSessionStart = "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"rec-s1\",\"cwd\":\"/ws\",\"source\":\"startup\"}"

// selfHealReportWarning is the one line the J4 rule prints for the soft flag: the oracle's impact
// sentence and the crw recovery command.
const selfHealReportWarning = "[crw] The codex feature flag default_mode_request_user_input that crw declares is off. " +
	"Default 모드에서 질문선택지 UI(request_user_input)가 모델에게 노출되지 않는다. Plan 모드에서는 계속 동작한다. " +
	"Turn it on with: crw install features enable\n"

const selfHealReportEnvelopePrefix = "{\"hookSpecificOutput\":{\"hookEventName\":\"SessionStart\",\"additionalContext\":"

// selfHealReportRunner records every invocation, so a test can prove "features enable" never ran.
type selfHealReportRunner struct {
	calls  [][]string
	stdout string
	stderr string
	exit   int
}

func (r *selfHealReportRunner) run(args []string) CodexRunResult {
	r.calls = append(r.calls, append([]string(nil), args...))
	return CodexRunResult{Stdout: r.stdout, Stderr: r.stderr, ExitCode: r.exit}
}

func (r *selfHealReportRunner) onlyList(t *testing.T) {
	t.Helper()
	if len(r.calls) == 0 {
		t.Fatal("the runner was never called")
	}
	for _, call := range r.calls {
		if !reflect.DeepEqual(call, []string{"features", "list"}) {
			t.Fatalf("the runner was called with %v; this hook issues only features/list", call)
		}
	}
}

// selfHealReportTempHome makes an isolated CODEX_HOME. HOME, CODEX_HOME and CRW_HOME all point into
// the temporary tree, so a test can never reach the real ones (operator rule 2026-10-04).
func selfHealReportTempHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CRW_HOME", filepath.Join(root, "crw"))
	return home
}

func selfHealReportEnv(home string) host.LookupEnv {
	return func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return home, true
		}
		return os.LookupEnv(key)
	}
}

// selfHealReportListing is every path under dir with its mode, size and content digest, so a write
// of any kind shows: a new file, a mode change, and a rewrite that keeps the length the same.
func selfHealReportListing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		digest := "-"
		if d.Type().IsRegular() {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(raw)
			digest = hex.EncodeToString(sum[:])
		}
		out = append(out, rel+" "+info.Mode().String()+" "+strconv.FormatInt(info.Size(), 10)+" "+digest)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("listing %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// selfHealReportShallowListing is the top level of a directory: cheap, and enough to catch a new file
// appearing in a home this test must not touch.
func selfHealReportShallowListing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	sort.Strings(out)
	return out
}

func selfHealReportRun(t *testing.T, home, in string) (string, int) {
	t.Helper()
	var out strings.Builder
	code := RunSelfHealReportHook(context.Background(), strings.NewReader(in), &out, selfHealReportEnv(home))
	return out.String(), code
}

func selfHealReportWriteMarker(t *testing.T, home, body string) string {
	t.Helper()
	path := SelfHealMarkerPath(home)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func selfHealReportWriteConfig(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5.5\"\n\n[features]\nhooks = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// selfHealReportMarkerMtimeMs is the configMtimeMs a marker must carry for the cache to match: the
// very spelling the implementation compares with (selfHealReportMsOf, Node's statSync().mtimeMs).
// A test that spells it any other way — float64(UnixNano())/1e6, for instance — agrees only while
// the two roundings happen to coincide, which they stop doing at these timestamps.
func selfHealReportMarkerMtimeMs(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatFloat(selfHealReportMsOf(info.ModTime()), 'f', -1, 64)
}

// selfHealReportMtimeSpellings returns the two ways a millisecond mtime can be computed for one
// instant: the way Node's statSync().mtimeMs and selfHealReportMsOf do it (the seconds and the
// nanoseconds added apart), and the lossy way through a single float64 of the nanosecond count.
func selfHealReportMtimeSpellings(t time.Time) (node, lossy string) {
	node = strconv.FormatFloat(float64(t.Unix())*1e3+float64(t.Nanosecond())/1e6, 'f', -1, 64)
	lossy = strconv.FormatFloat(float64(t.UnixNano())/1e6, 'f', -1, 64)
	return node, lossy
}

// selfHealReportPinMtime gives path the first instant of the hour starting at base whose two mtime
// spellings differ and which the filesystem stores exactly as asked. It never skips: a filesystem
// that rounds one instant is asked for the next, so the returned instant is always one where the
// Node spelling and the lossy one disagree, and the caller's assertions are therefore about the
// comparison rather than about a coincidence. It fails when it finds none within the hour.
func selfHealReportPinMtime(t *testing.T, path string, base time.Time) (time.Time, string, string) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		candidate := base.Add(time.Duration(i) * time.Millisecond)
		node, lossy := selfHealReportMtimeSpellings(candidate)
		if node == lossy {
			continue
		}
		if err := os.Chtimes(path, candidate, candidate); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Unix() != candidate.Unix() || info.ModTime().Nanosecond() != candidate.Nanosecond() {
			continue
		}
		if got := selfHealReportMarkerMtimeMs(t, path); got != node {
			t.Fatalf("the pinned mtime reads as %s, want %s", got, node)
		}
		return candidate, node, lossy
	}
	t.Fatalf("no instant in the hour from %v both differs between the two spellings and survives this filesystem", base)
	return time.Time{}, "", ""
}

// selfHealReportFakeCodex puts a fake codex on PATH: "features list" answers with listing, anything
// else exits 1, and every invocation is appended to the log this returns. It uses shell builtins
// only, because the caller's PATH becomes the temporary directory that holds it.
func selfHealReportFakeCodex(t *testing.T, listing string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + log + "\"\n" +
		"if [ \"$1\" = features ] && [ \"$2\" = list ]; then\n" +
		"printf '%s' '" + listing + "'\n" +
		"exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := execfile.WriteExecutable(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return log
}

func selfHealReportCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// selfHealReportFakeCodexAt makes a directory that shadows the real PATH, so a codex written into it
// is the one the hook finds while the script may still use the ordinary utilities.
func selfHealReportFakeCodexAt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func selfHealReportWriteFakeCodex(t *testing.T, dir, body string) {
	t.Helper()
	if err := execfile.WriteExecutable(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestSelfHealReportCancellationStopsTheProbe is the first red finding of the pull request's
// review: a stalled "codex features list" must not outlive the hook's context. The hook answers
// Interrupted as the other component hooks do, rather than waiting for the child.
func TestSelfHealReportCancellationStopsTheProbe(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	dir := selfHealReportFakeCodexAt(t)
	started := filepath.Join(dir, "started")
	selfHealReportWriteFakeCodex(t, dir, "printf started > \""+started+"\"\nsleep 300\n")

	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan int, 1)
	go func() {
		done <- RunSelfHealReportHook(ctx, strings.NewReader(selfHealReportSessionStart), &out, selfHealReportEnv(home))
	}()
	// Cancel once the child is up; a hook that never checks its context would block here.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != harness.Interrupted {
			t.Fatalf("exit = %d, want %d after cancellation", code, harness.Interrupted)
		}
		if out.String() != "" {
			t.Fatalf("a cancelled run wrote %q, want nothing", out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the hook did not return after its context was cancelled")
	}
}

// TestSelfHealReportOutputBudgetEndsTheProbe is the second red finding: a probe that keeps writing
// past the shared 1 MiB budget is ended rather than drained forever, and the round is unavailable
// (silent, exit 0).
func TestSelfHealReportOutputBudgetEndsTheProbe(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	// A 4 KiB line printed without end: the shared budget is crossed after ~256 of them.
	dir := selfHealReportFakeCodexAt(t)
	selfHealReportWriteFakeCodex(t, dir, "while :; do printf '%s' \"hooks stable true \"; done\n")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out strings.Builder
	code := RunSelfHealReportHook(ctx, strings.NewReader(selfHealReportSessionStart), &out, selfHealReportEnv(home))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != "" {
		t.Fatalf("an overflowing probe wrote %q, want silence", out.String())
	}
}

// TestSelfHealReportOffSoftKeyWarns is the J4 core case end to end: a fake codex reports the soft
// flag off, the hook warns in the oracle's envelope, exits 0 and calls nothing but features/list.
func TestSelfHealReportOffSoftKeyWarns(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	log := selfHealReportFakeCodex(t, selfHealReportSoftOff)

	out, code := selfHealReportRun(t, home, selfHealReportSessionStart)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	want := selfHealReportEnvelopePrefix + pyjson.Dumps(selfHealReportWarning, pyjson.Options{Compact: true, Unicode: true}) + "}}\n"
	if out != want {
		t.Fatalf("stdout = %q\nwant    = %q", out, want)
	}
	if got := selfHealReportCalls(t, log); !reflect.DeepEqual(got, []string{"features list"}) {
		t.Fatalf("codex calls = %v, want exactly one features/list and never features/enable", got)
	}
}

// TestSelfHealReportOffSoftKeyWritesNothing is the criterion's letter: a listing of CODEX_HOME
// before and after the warning is equal.
func TestSelfHealReportOffSoftKeyWritesNothing(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	selfHealReportFakeCodex(t, selfHealReportSoftOff)
	before := selfHealReportListing(t, home)

	if _, code := selfHealReportRun(t, home, selfHealReportSessionStart); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("CODEX_HOME changed:\n before %v\n after  %v", before, after)
	}
}

// TestSelfHealReportOffOutcomeIsOneLinePerOffKey drives the pure rule: the soft key off with no
// healedKeys entry, so one off outcome and one warning line, and no write.
func TestSelfHealReportOffOutcomeIsOneLinePerOffKey(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	runner := &selfHealReportRunner{stdout: selfHealReportSoftOff}
	before := selfHealReportListing(t, home)

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	runner.onlyList(t)
	if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff || outcomes[0].Key != "default_mode_request_user_input" {
		t.Fatalf("outcomes = %+v, want one off outcome for the soft key", outcomes)
	}
	if got := RenderSelfHealReportContext(outcomes); got != selfHealReportWarning {
		t.Fatalf("context = %q\nwant    = %q", got, selfHealReportWarning)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("CODEX_HOME changed:\n before %v\n after  %v", before, after)
	}
}

func TestSelfHealReportOptedOutMarkerIsSilentWithoutCodex(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	marker := selfHealReportWriteMarker(t, home, "{\"optedOut\":true,\"optedOutAt\":\"2025-12-01T00:00:00.000Z\"}\n")
	runner := &selfHealReportRunner{stdout: selfHealReportSoftOff}
	before := selfHealReportListing(t, home)

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	if len(runner.calls) != 0 {
		t.Fatalf("the opt-out marker still called codex: %v", runner.calls)
	}
	if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportSkipped || outcomes[0].Reason != SelfHealReasonOptedOut {
		t.Fatalf("outcomes = %+v, want one skipped/opted-out", outcomes)
	}
	if got := RenderSelfHealReportContext(outcomes); got != "" {
		t.Fatalf("an opt-out rendered %q, want silence", got)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("the opt-out path wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "{\"optedOut\":true,\"optedOutAt\":\"2025-12-01T00:00:00.000Z\"}\n" {
		t.Fatalf("the marker was rewritten: %q err=%v", raw, err)
	}
}

func TestSelfHealReportCacheHitIsSilentWithoutCodex(t *testing.T) {
	home := selfHealReportTempHome(t)
	path := selfHealReportWriteConfig(t, home)
	ms := selfHealReportMarkerMtimeMs(t, path)
	selfHealReportWriteMarker(t, home, "{\"checkedAt\":\"2025-12-31T00:00:00.000Z\",\"allEnabled\":true,\"healedKeys\":[],"+
		"\"cachedKeys\":[\"default_mode_request_user_input\"],\"configMtimeMs\":"+ms+"}\n")
	runner := &selfHealReportRunner{stdout: selfHealReportSoftOff}
	before := selfHealReportListing(t, home)

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	if len(runner.calls) != 0 {
		t.Fatalf("the cache hit still called codex: %v", runner.calls)
	}
	if len(outcomes) != 1 || outcomes[0].Reason != SelfHealReasonCached {
		t.Fatalf("outcomes = %+v, want one skipped/cached", outcomes)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("the cache hit wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
}

// TestSelfHealReportCacheHitPinnedMtime pins the cache key's arithmetic to an instant where the two
// ways of spelling a millisecond mtime disagree: 2026-06-15T08:09:10.250Z is 1781510950250 the Node
// way (the seconds and the nanoseconds added apart) and 1781510950249.9998 through a single
// float64 of the nanosecond count. The marker carries the literal spelling under test, so the first
// case fails if the comparison goes back to the lossy form and the second fails if it ever accepts
// it, whatever the test helper does.
func TestSelfHealReportCacheHitPinnedMtime(t *testing.T) {
	// 2026-06-15T08:09:10Z is the first instant of a window in which the two spellings disagree; the
	// helper walks forward from it when a filesystem stores a coarser timestamp.
	base := time.Date(2026, 6, 15, 8, 9, 10, 0, time.UTC)

	for _, tc := range []struct {
		name, ms string
		want     SelfHealReportAction
	}{
		{"the node spelling hits", "node", SelfHealReportSkipped},
		{"the lossy spelling misses", "lossy", SelfHealReportOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := selfHealReportTempHome(t)
			path := selfHealReportWriteConfig(t, home)
			pinned, node, lossy := selfHealReportPinMtime(t, path, base)
			ms := node
			if tc.ms == "lossy" {
				ms = lossy
			}
			selfHealReportWriteMarker(t, home, "{\"checkedAt\":\"2025-12-31T00:00:00.000Z\",\"allEnabled\":true,\"healedKeys\":[],"+
				"\"cachedKeys\":[\"default_mode_request_user_input\"],\"configMtimeMs\":"+ms+"}\n")
			runner := &selfHealReportRunner{stdout: selfHealReportSoftOff}

			outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
			if tc.want == SelfHealReportSkipped {
				if len(runner.calls) != 0 {
					t.Fatalf("the pinned cache hit at %v still called codex: %v", pinned, runner.calls)
				}
				if len(outcomes) != 1 || outcomes[0].Reason != SelfHealReasonCached {
					t.Fatalf("outcomes = %+v, want one skipped/cached at %v (%s)", outcomes, pinned, node)
				}
				return
			}
			runner.onlyList(t)
			if len(outcomes) != 1 || outcomes[0].Action != tc.want {
				t.Fatalf("outcomes = %+v, want one off outcome: the lossy spelling %s of %v missed the mtime %s", outcomes, lossy, pinned, node)
			}
		})
	}
}

// TestSelfHealReportCacheMissOnMovedMtime proves the cache rule still compares config.toml's mtime:
// the same marker with a stale mtime re-probes.
func TestSelfHealReportCacheMissOnMovedMtime(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	// 1767225600000 is 2026-01-01T00:00:00Z, deliberately not the freshly written config.toml's mtime.
	selfHealReportWriteMarker(t, home, "{\"checkedAt\":\"2025-12-31T00:00:00.000Z\",\"allEnabled\":true,\"healedKeys\":[],"+
		"\"cachedKeys\":[\"default_mode_request_user_input\"],\"configMtimeMs\":1767225600000}\n")
	runner := &selfHealReportRunner{stdout: selfHealReportSoftOff}

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	runner.onlyList(t)
	if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("outcomes = %+v, want one off outcome after the cache missed", outcomes)
	}
}

func TestSelfHealReportHealedKeyIsSilent(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	selfHealReportWriteMarker(t, home, "{\"healedKeys\":[\"default_mode_request_user_input\"]}\n")
	runner := &selfHealReportRunner{stdout: selfHealReportSoftOff}

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	runner.onlyList(t)
	if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportDeclined || outcomes[0].Key != "default_mode_request_user_input" {
		t.Fatalf("outcomes = %+v, want one declined for the healed key", outcomes)
	}
	if got := RenderSelfHealReportContext(outcomes); got != "" {
		t.Fatalf("a healed key rendered %q, want silence", got)
	}
}

func TestSelfHealReportFeaturesListFailureIsSilent(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	before := selfHealReportListing(t, home)
	runner := &selfHealReportRunner{stderr: "error: unknown command\n", exit: 1}

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportUnavailable {
		t.Fatalf("outcomes = %+v, want one unavailable", outcomes)
	}
	if got := RenderSelfHealReportContext(outcomes); got != "" {
		t.Fatalf("a list failure rendered %q, want silence", got)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("a list failure wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
}

func TestSelfHealReportAllEnabledIsSilent(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	before := selfHealReportListing(t, home)
	runner := &selfHealReportRunner{stdout: selfHealReportSoftOn}

	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	runner.onlyList(t)
	if len(outcomes) != 1 || outcomes[0].Reason != SelfHealReasonAlreadyEnabled {
		t.Fatalf("outcomes = %+v, want one skipped/already-enabled", outcomes)
	}
	if got := RenderSelfHealReportContext(outcomes); got != "" {
		t.Fatalf("all enabled rendered %q, want silence", got)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("all enabled wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
}

// TestSelfHealReportFailOpen covers every input that must not stop a session from starting. The
// diagnosis does not depend on the payload (the oracle reads stdin only for the observation record),
// so fail-open here means exit 0, no answer while the listing is unreadable, and no write.
func TestSelfHealReportFailOpen(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	t.Setenv("PATH", t.TempDir()) // no codex: the listing cannot be read
	before := selfHealReportListing(t, home)
	for _, in := range []string{"", "not json", "[]", "{}", strings.Repeat("x", 32), "{\"session_id\":123}"} {
		out, code := selfHealReportRun(t, home, in)
		if code != 0 {
			t.Fatalf("input %q answered exit %d, want 0", in, code)
		}
		if out != "" {
			t.Fatalf("input %q answered %q, want silence while the listing is unreadable", in, out)
		}
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("malformed input wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
}

// TestSelfHealReportMissingCodexIsSilent is the baseline case: no codex on PATH, so the listing
// cannot be read and the hook says nothing.
func TestSelfHealReportMissingCodexIsSilent(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	t.Setenv("PATH", t.TempDir())
	before := selfHealReportListing(t, home)

	out, code := selfHealReportRun(t, home, selfHealReportSessionStart)
	if code != 0 || out != "" {
		t.Fatalf("a missing codex answered exit %d with %q, want silent exit 0", code, out)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("a missing codex wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
}

// TestSelfHealReportObservationIsTheOnlyWrite separates the two kinds of write: the shared
// component-hook observation record every row leaves, and the state this hook must never touch.
func TestSelfHealReportObservationIsTheOnlyWrite(t *testing.T) {
	home := selfHealReportTempHome(t)
	config := selfHealReportWriteConfig(t, home)
	marker := selfHealReportWriteMarker(t, home, "{\"healedKeys\":[\"default_mode_request_user_input\"]}\n")
	manifest := filepath.Join(home, ".crw-install.json")
	if err := os.WriteFile(manifest, []byte("{\"version\":2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"), []byte("{\"version\":\"0.4.0\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLUGIN_ROOT", plugin)
	selfHealReportFakeCodex(t, selfHealReportSoftOff)

	before := map[string]string{}
	for name, path := range map[string]string{"config.toml": config, "marker": marker, "manifest": manifest} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(raw)
	}

	if _, code := selfHealReportRun(t, home, selfHealReportSessionStart); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	for name, path := range map[string]string{"config.toml": config, "marker": marker, "manifest": manifest} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(raw) != before[name] {
			t.Fatalf("%s was rewritten:\n before %q\n after  %q", name, before[name], string(raw))
		}
	}
	observations := filepath.Join(home, "crw", "hook-observations")
	var found []string
	if err := filepath.WalkDir(observations, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("the observation record is missing: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("observation records = %v, want exactly one", found)
	}
	raw, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\"component\":\"config-guard\"") || !strings.Contains(string(raw), "\"event\":\"session-start\"") {
		t.Fatalf("observation = %s, want the config-guard session-start record", raw)
	}
}

// TestSelfHealReportNeverTouchesTheRealHomes lists ~/.codex and ~/.crw before and after runs whose
// inputs are all temporary. A difference is reported, never cleaned up here.
func TestSelfHealReportNeverTouchesTheRealHomes(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no real home to watch: %v", err)
	}
	codexBefore := selfHealReportShallowListing(t, filepath.Join(realHome, ".codex"))
	crwBefore := selfHealReportShallowListing(t, filepath.Join(realHome, ".crw"))

	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	selfHealReportFakeCodex(t, selfHealReportSoftOff)
	for _, in := range []string{selfHealReportSessionStart, "garbage", ""} {
		if _, code := selfHealReportRun(t, home, in); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
	}

	if after := selfHealReportShallowListing(t, filepath.Join(realHome, ".codex")); !reflect.DeepEqual(codexBefore, after) {
		t.Fatalf("the real ~/.codex changed:\n before %v\n after  %v", codexBefore, after)
	}
	if after := selfHealReportShallowListing(t, filepath.Join(realHome, ".crw")); !reflect.DeepEqual(crwBefore, after) {
		t.Fatalf("the real ~/.crw changed:\n before %v\n after  %v", crwBefore, after)
	}
}
