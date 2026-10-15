//go:build dev

package ci

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-1186 evaluation d5: the tool-version probes make their directories in TMPDIR while the current
// keys are collected, before the work root is looked at. A TMPDIR in the account's real home is
// refused before the first probe, for a fresh run and a reuse alike, and nothing is made there.
func TestLocalVerify_aTempDirInTheAccountHomeIsRefusedBeforeAnyProbe(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	tmp := filepath.Join(home, ".codex", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	repo := newLocalFixture(t)
	for _, reuse := range []string{"", filepath.Join(t.TempDir(), "earlier.json")} {
		opts := localRunOptions(repo, localFixturePlan("true"), filepath.Join(t.TempDir(), "record.json"))
		_, _, err := localVerify(opts, reuse, io.Discard)
		var refusal *homeguard.Error
		if !errors.As(err, &refusal) {
			t.Errorf("reuse %q: want a refusal of the account home, got %v", reuse, err)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("reuse %q: %d entries were made in the account's TMPDIR", reuse, len(entries))
		}
	}
	// the probe itself, called alone, makes nothing there either
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho 'go version go1.27.1 linux/amd64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := localObserveTool("go", bin, ""); got != "" {
		t.Errorf("a probe in the account's TMPDIR answered %q", got)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("the probe made %d entries in the account's TMPDIR", len(entries))
	}
}

// CRW-1186 evaluation d4: --record may be any path; a record (and its .canonical sibling and the
// directories above them) in the account's real home is refused before the run, for a fresh run and
// a reuse alike. The real switch file is not replaced.
func TestLocalVerify_aRecordInTheAccountHomeIsRefused(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	dir := filepath.Join(home, ".codex", "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	switchFile := filepath.Join(dir, "switch.json")
	if err := os.WriteFile(switchFile, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	repo := newLocalFixture(t)
	for name, record := range map[string]string{
		"the switch file":   switchFile,
		"a new file":        filepath.Join(dir, "record.json"),
		"below a new dir":   filepath.Join(home, ".crw", "new", "record.json"),
		"through a link":    filepath.Join(link, "switch.json"),
		"through a dotdot":  link + "/../crw/switch.json",
		"runtime directory": filepath.Join(home, ".local", "share", "crw-runtime", "r.json"),
	} {
		for _, reuse := range []string{"", filepath.Join(t.TempDir(), "earlier.json")} {
			opts := localRunOptions(repo, localFixturePlan("true"), record)
			_, _, err := localVerify(opts, reuse, io.Discard)
			var refusal *homeguard.Error
			if !errors.As(err, &refusal) {
				t.Errorf("%s (reuse %q): want a refusal of the account home, got %v", name, reuse, err)
			}
		}
	}
	if raw, _ := os.ReadFile(switchFile); string(raw) != "original" {
		t.Errorf("the real switch file now holds %q", raw)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the account's crw directory holds %d entries, want only switch.json", len(entries))
	}
	for _, rel := range []string{".crw", ".local"} {
		if _, err := os.Lstat(filepath.Join(home, rel)); err == nil {
			t.Errorf("%s was made in the account home", rel)
		}
	}
}

// The checkout the record is written for may itself be a managed worktree below the account's .codex:
// a record inside that checkout is its own output and is written; one that leaves it for the account's
// protected directories, by name or through a link, is refused.
func TestLocalRecordDestination_aManagedCheckoutKeepsItsOwnRecord(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	checkout := filepath.Join(home, ".codex", "worktrees", "w")
	if err := os.MkdirAll(filepath.Join(home, ".codex", "crw"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex", "crw"), filepath.Join(checkout, "out")); err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{filepath.Join(checkout, localRecordDefault), filepath.Join(checkout, "new", "dir", "r.json")} {
		if err := localCheckRecordDestination(checkout, record); err != nil {
			t.Errorf("%s: %v", record, err)
		}
	}
	for _, record := range []string{
		filepath.Join(home, ".codex", "crw", "switch.json"),
		filepath.Join(checkout, "..", "..", "crw", "switch.json"),
		filepath.Join(checkout, "out", "switch.json"),
		filepath.Join(home, ".codex", "other", "r.json"),
	} {
		var refusal *homeguard.Error
		if err := localCheckRecordDestination(checkout, record); !errors.As(err, &refusal) {
			t.Errorf("%s: want a refusal of the account home, got %v", record, err)
		}
	}
}

// localStepTmpName is the base name of a step TMPDIR: its prefix and 8 lowercase hex characters.
func localStepTmpName(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + prefix + `[0-9a-f]{8}$`)
}

// localShortWorkRoot is a short work root of the test's own, made directly in the TMPDIR the test
// started with (before any t.Setenv), and removed when the test ends.
func localShortWorkRoot(t *testing.T) string {
	t.Helper()
	w, err := os.MkdirTemp(os.TempDir(), "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(w) })
	return w
}

// localMarkerStep is a step that writes its $TMPDIR to marker, after the checks given run.
func localMarkerStep(checks, marker string) string {
	return fmt.Sprintf("%s\nprintf %%s \"$TMPDIR\" > %q", checks, marker)
}

// localReadMarker is the step TMPDIR a marker step wrote.
func localReadMarker(t *testing.T, marker string) string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the step wrote no marker: %v", err)
	}
	return string(data)
}

// localNoEntry fails when dir holds an entry whose name starts with prefix.
func localNoEntry(t *testing.T, dir, prefix string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			t.Errorf("%s holds %s", dir, entry.Name())
		}
	}
}

// CRW-1489: the 1209 shape, a caller TMPDIR below a stray .git (the bwrap marker /tmp/.git), is not the
// step's: candidate a is refused for the .git above it, and the step runs in candidate b, directly below
// the work root, where no walk up from TMPDIR meets a .git.
func TestLocal_a_callers_TMPDIR_below_a_stray_git_is_not_the_steps(t *testing.T) {
	w := localShortWorkRoot(t)
	p := t.TempDir()
	if err := os.Mkdir(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	callerTmp := filepath.Join(p, "tmp")
	if err := os.Mkdir(callerTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "tmpdir")
	t.Setenv("TMPDIR", callerTmp)
	repo := newLocalFixture(t)
	ceilings := []string{filepath.Dir(p), filepath.Dir(w)}
	checks := fmt.Sprintf(`d="$TMPDIR"
while :; do
  case ":$CRW_TEST_CEILINGS:" in *":$d:"*) break ;; esac
  if [ -e "$d/.git" ] || [ -L "$d/.git" ]; then echo "a .git at $d"; exit 1; fi
  [ "$d" = / ] && break
  d=$(dirname "$d")
done
case "$TMPDIR" in %q/*) echo "below the stray .git"; exit 1 ;; esac`, p)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep(checks, marker)), filepath.Join(t.TempDir(), "record.json"))
	opts.WorkRoot = w
	opts.gitCeilings = ceilings
	opts.socketLimit = localStepDirLen(w, "t") + 67 + 1
	opts.Env = append(opts.Env, "CRW_TEST_CEILINGS="+strings.Join(ceilings, ":"))
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %#v", made.Result, made.Jobs)
	}
	stepTmp := localReadMarker(t, marker)
	if filepath.Dir(stepTmp) != w || !localStepTmpName("t").MatchString(filepath.Base(stepTmp)) {
		t.Errorf("the step's TMPDIR is %s, want candidate b: t<8 hex> directly below %s", stepTmp, w)
	}
	localNoEntry(t, callerTmp, "ci")
}

// CRW-1489: a caller TMPDIR too long for the step's sockets (a's length plus the 67 bytes a test adds
// reaches the limit) falls back to candidate b below a shorter work root, and nothing is made in it.
func TestLocal_a_long_callers_TMPDIR_falls_back_to_the_work_root(t *testing.T) {
	w := localShortWorkRoot(t)
	c := t.TempDir()
	for len(c) < len(w)+12 {
		c = filepath.Join(c, "d")
	}
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	if len(c) < len(w)+2 {
		t.Fatalf("the caller TMPDIR %s is not longer than the work root %s by 2", c, w)
	}
	marker := filepath.Join(t.TempDir(), "tmpdir")
	t.Setenv("TMPDIR", c)
	repo := newLocalFixture(t)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), filepath.Join(t.TempDir(), "record.json"))
	opts.WorkRoot = w
	opts.gitCeilings = []string{filepath.Dir(c), filepath.Dir(w)}
	opts.socketLimit = localStepDirLen(c, "ci") + 67
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %#v", made.Result, made.Jobs)
	}
	stepTmp := localReadMarker(t, marker)
	if filepath.Dir(stepTmp) != w || !localStepTmpName("t").MatchString(filepath.Base(stepTmp)) {
		t.Errorf("the step's TMPDIR is %s, want candidate b: t<8 hex> directly below %s", stepTmp, w)
	}
	localNoEntry(t, c, "ci")
}

// CRW-1489: the 1209 shape with a work root as long as the default one is against its limit: a has a
// .git above it and b reaches the limit, so the run is refused before any checkout or step, and the
// refusal names both reasons and the way out.
func TestLocal_the_1209_shape_is_refused_not_run(t *testing.T) {
	w := localShortWorkRoot(t)
	p := t.TempDir()
	if err := os.Mkdir(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	callerTmp := filepath.Join(p, "tmp")
	if err := os.Mkdir(callerTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("TMPDIR", callerTmp)
	repo := newLocalFixture(t)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), filepath.Join(t.TempDir(), "record.json"))
	opts.WorkRoot = w
	opts.gitCeilings = []string{filepath.Dir(p), filepath.Dir(w)}
	limit := localStepDirLen(w, "t") + 67
	opts.socketLimit = limit
	_, _, err := localVerify(opts, "", io.Discard)
	if err == nil {
		t.Fatal("the run was not refused")
	}
	for _, want := range []string{"temp_dir_unusable", filepath.Join(p, ".git"), fmt.Sprint(limit), "--work-root"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a step ran")
	}
	localNoEntry(t, callerTmp, "ci")
	localNoEntry(t, w, "t")
}

// CRW-1489: the step's TMPDIR is removed with the run, and kept and printed with --keep.
func TestLocal_the_steps_TMPDIR_is_removed_after_the_run(t *testing.T) {
	for _, keep := range []bool{false, true} {
		callerTmp := t.TempDir()
		marker := filepath.Join(t.TempDir(), "tmpdir")
		t.Setenv("TMPDIR", callerTmp)
		repo := newLocalFixture(t)
		opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), filepath.Join(t.TempDir(), "record.json"))
		opts.Keep = keep
		var stdout bytes.Buffer
		made, _, err := localVerify(opts, "", &stdout)
		if err != nil {
			t.Fatal(err)
		}
		if made.Result != localPass {
			t.Fatalf("keep %v: the run is %q: %#v", keep, made.Result, made.Jobs)
		}
		stepTmp := localReadMarker(t, marker)
		if filepath.Dir(stepTmp) != callerTmp || !localStepTmpName("ci").MatchString(filepath.Base(stepTmp)) {
			t.Errorf("keep %v: the step's TMPDIR is %s, want candidate a: ci<8 hex> directly below %s", keep, stepTmp, callerTmp)
		}
		_, statErr := os.Stat(stepTmp)
		if !keep {
			if statErr == nil {
				t.Errorf("the step's TMPDIR %s is left after the run", stepTmp)
			}
			localNoEntry(t, callerTmp, "ci")
			continue
		}
		if statErr != nil {
			t.Errorf("--keep: the step's TMPDIR is gone: %v", statErr)
		}
		if !strings.Contains(stdout.String(), "local: kept "+stepTmp) {
			t.Errorf("--keep: the step's TMPDIR is not printed:\n%s", stdout.String())
		}
		os.RemoveAll(stepTmp)
	}
}

// CRW-1489: the fit is exact, decided from the name's fixed width before the directory exists, and the
// default XDG work root of this account never fits a step's sockets, on any run.
func TestLocalStepTmpFits_is_exact_and_pins_the_default_work_root(t *testing.T) {
	const defaultRoot = "/home/jun/.local/state/crw-ci-local"
	if got := localStepDirLen(defaultRoot, "t"); got != 45 {
		t.Errorf("localStepDirLen(%s, t) = %d, want 45", defaultRoot, got)
	}
	if localStepTmpFits(defaultRoot, "t", 108) {
		t.Error("the default work root fits at 108: 45 + 67 = 112 must not")
	}
	if !localStepTmpFits("/scratch/ci", "t", 108) {
		t.Error("/scratch/ci does not fit at 108: 21 + 67 = 88 must")
	}
	for _, x := range []string{"/scratch/ci", defaultRoot, "/tmp"} {
		if got := localStepDirLen(x, "ci"); got != len(x)+11 {
			t.Errorf("localStepDirLen(%s, ci) = %d, want %d", x, got, len(x)+11)
		}
	}
	at := localStepDirLen("/scratch/ci", "t") + 67
	if localStepTmpFits("/scratch/ci", "t", at) {
		t.Errorf("a length + 67 equal to the limit %d fits", at)
	}
	if !localStepTmpFits("/scratch/ci", "t", at+1) {
		t.Errorf("a length + 67 one below the limit %d does not fit", at+1)
	}
	if localStepSocketSuffix != 67 {
		t.Errorf("localStepSocketSuffix = %d, want 67", localStepSocketSuffix)
	}
	want := 108
	if runtime.GOOS == "darwin" {
		want = 104
	}
	if got := localSocketLimit(); got != want {
		t.Errorf("localSocketLimit() = %d on %s, want %d", got, runtime.GOOS, want)
	}
}

// CRW-1489: a step TMPDIR's name has a fixed width, so its length is known before it is made.
func TestLocalMkStepDir_names_have_a_fixed_width(t *testing.T) {
	parent := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		dir, err := localMkStepDir(parent, "t")
		if err != nil {
			t.Fatal(err)
		}
		if seen[dir] {
			t.Errorf("%s was made twice", dir)
		}
		seen[dir] = true
		if filepath.Dir(dir) != parent || !localStepTmpName("t").MatchString(filepath.Base(dir)) {
			t.Errorf("%s is not t<8 hex> directly below %s", dir, parent)
		}
		if len(dir) != localStepDirLen(parent, "t") {
			t.Errorf("%s is %d bytes, want %d", dir, len(dir), localStepDirLen(parent, "t"))
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("%s is not a directory: %v", dir, err)
		}
	}
}

// CRW-1489 guard: the version probes keep the caller's TMPDIR; only a step gets its own.
func TestLocal_the_probes_keep_the_callers_TMPDIR(t *testing.T) {
	x := t.TempDir()
	t.Setenv("TMPDIR", x)
	if env := localProbeEnv(filepath.Join(t.TempDir(), "home"), "/usr/bin"); !localContains(env, "TMPDIR="+x) {
		t.Errorf("the probe environment does not carry the caller's TMPDIR %s: %v", x, env)
	}
}
