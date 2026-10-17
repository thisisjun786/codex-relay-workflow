//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
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
	w, err := tmploc.MkdirTemp("w")
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

// CRW-1489, CRW-1569: a caller TMPDIR below a real repository is never the step's: the walk up from it
// meets the repository's .git, so the run is refused before any step with temp_dir_unusable naming that
// .git, and no step TMPDIR is made below it. The steps are not moved to another place
// (TestLocal_never_moves_the_step_TMPDIR); a .git holding no repository is no marker at all
// (TestLocalStepTempDirIgnoresNonRepositoryMarkers).
func TestLocal_a_callers_TMPDIR_below_a_repository_is_refused(t *testing.T) {
	w := localShortWorkRoot(t)
	p := t.TempDir()
	runGitOrFail(t, p, "init", "-q")
	callerTmp := filepath.Join(p, "tmp")
	if err := os.Mkdir(callerTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "tmpdir")
	t.Setenv("TMPDIR", callerTmp)
	repo := newLocalFixture(t)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), filepath.Join(t.TempDir(), "record.json"))
	opts.WorkRoot = w
	opts.gitCeilings = []string{filepath.Dir(p), filepath.Dir(w)}
	_, _, err := localVerify(opts, "", io.Discard)
	if err == nil {
		t.Fatal("a caller TMPDIR below a stray .git is not refused")
	}
	for _, want := range []string{"temp_dir_unusable", filepath.Join(p, ".git")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("a step ran in %s", localReadMarker(t, marker))
	}
	localNoEntry(t, callerTmp, "ci")
	localNoEntry(t, w, "t")
}

// CRW-1489, CRW-1569: the 1209 shape runs. The trial's caller TMPDIR was 27 bytes, and its step TMPDIR
// ci<8 hex> 38; the sockets its tests bind no longer lie below either (CRW-1568), so neither length is
// judged: the run passes with its step TMPDIR directly below the 27-byte TMPDIR. A TMPDIR below a .git is
// refused by its content (TestLocal_never_moves_the_step_TMPDIR), not by its length.
func TestLocal_the_1209_shape_runs(t *testing.T) {
	// Preserve the recorded 27-byte fixture under the fixed default root;
	// inheriting a long TMPDIR would change the scenario being checked.
	short, err := tmploc.MkdirTempIn(tmploc.DefaultDir, "crw1209-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	if len(short) > 25 {
		t.Fatalf("%s is already %d bytes", short, len(short))
	}
	callerTmp := filepath.Join(short, strings.Repeat("t", 27-len(short)-1))
	if err := os.Mkdir(callerTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if len(callerTmp) != 27 {
		t.Fatalf("the caller TMPDIR %s is %d bytes, want 27", callerTmp, len(callerTmp))
	}
	repo := newLocalFixture(t)
	marker := filepath.Join(t.TempDir(), "tmpdir")
	t.Setenv("TMPDIR", callerTmp)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), filepath.Join(t.TempDir(), "record.json"))
	// The .git walk stops below /tmp, so a stray /tmp/.git on the test host does not decide this run.
	opts.gitCeilings = []string{tmploc.DefaultDir}
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatalf("the 1209 shape is refused: %v", err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %#v", made.Result, made.Jobs)
	}
	resolved, err := filepath.EvalSymlinks(callerTmp)
	if err != nil {
		t.Fatal(err)
	}
	stepTmp := localReadMarker(t, marker)
	if filepath.Dir(stepTmp) != resolved || !localStepTmpName("ci").MatchString(filepath.Base(stepTmp)) {
		t.Errorf("the step's TMPDIR is %s, want ci<8 hex> directly below %s", stepTmp, resolved)
	}
	if resolved == callerTmp && len(stepTmp) != 38 {
		t.Errorf("the step's TMPDIR %s is %d bytes, want 38", stepTmp, len(stepTmp))
	}
}

// CRW-1489 (PR ping-pong 2): the caller's TMPDIR is taken where the kernel resolves it, not cleaned lexically first: with link
// leading to base/x/y, "base/link/../tmp" is base/x/tmp, so the step TMPDIR is made there and nothing in the lexical base/tmp.
// The record keeps the raw value as callerTmpDir and the directory actually used as tmpDir. A TMPDIR that cannot be resolved
// ("base/missing/../tmp", whose lexical form exists) is refused with its reason (CRW-1569: temp_dir_unusable), and nothing is
// made in the lexical form or below the work root.
func TestLocal_a_callers_TMPDIR_is_taken_where_the_kernel_resolves_it(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "x", "tmp")
	lexical := filepath.Join(base, "tmp")
	for _, d := range []string{filepath.Join(base, "x", "y"), physical, lexical} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "x", "y"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	wantPhysical, err := filepath.EvalSymlinks(physical)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, raw string }{
		{"through a link", base + "/link/../tmp"},
		{"unresolvable", base + "/missing/../tmp"},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "where")
			t.Setenv("TMPDIR", c.raw)
			repo := newLocalFixture(t)
			opts := localRunOptions(repo, localFixturePlan(localWhereStep(marker)), filepath.Join(t.TempDir(), "record.json"))
			opts.gitCeilings = []string{base, repo.work}
			made, _, err := localVerify(opts, "", io.Discard)
			if c.name == "unresolvable" {
				if err == nil || !strings.Contains(err.Error(), "temp_dir_unusable") || !strings.Contains(err.Error(), c.raw) {
					t.Fatalf("an unresolvable TMPDIR is not refused with its reason: %v", err)
				}
				if _, err := os.Stat(marker); err == nil {
					t.Error("a step ran")
				}
				localNoEntry(t, lexical, "ci")
				localNoEntry(t, repo.work, "")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if made.Result != localPass {
				t.Fatalf("the run is %q: %#v", made.Result, made.Jobs)
			}
			stepTmp, _ := localReadWhere(t, marker)
			if filepath.Dir(stepTmp) != wantPhysical || !localStepTmpName("ci").MatchString(filepath.Base(stepTmp)) {
				t.Errorf("the step's TMPDIR is %s, want ci<8 hex> directly below %s", stepTmp, wantPhysical)
			}
			localNoEntry(t, lexical, "ci")
			if made.RunEnv == nil || made.RunEnv.CallerTmpDir != c.raw || made.RunEnv.TmpDir != stepTmp {
				t.Errorf("the record's runEnv is %+v, want callerTmpDir %q and tmpDir %q", made.RunEnv, c.raw, stepTmp)
			}
		})
	}
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

// CRW-1489, CRW-1569: the default work root is unchanged: $XDG_STATE_HOME/crw-ci-local, or
// ~/.local/state/crw-ci-local when XDG_STATE_HOME is unset. Its length is no longer judged (there is no
// step socket budget), and it is the work root only: the steps' TMPDIR is never made below it.
func TestLocalWorkRoot_default_is_unchanged(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got, err := localWorkRoot(localOptions{}); err != nil || got != filepath.Join(state, "crw-ci-local") {
		t.Errorf("with XDG_STATE_HOME the default work root is %q (%v), want %s", got, err, filepath.Join(state, "crw-ci-local"))
	}
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	if got, err := localWorkRoot(localOptions{}); err != nil || got != filepath.Join(home, ".local", "state", "crw-ci-local") {
		t.Errorf("without XDG_STATE_HOME the default work root is %q (%v), want %s", got, err, filepath.Join(home, ".local", "state", "crw-ci-local"))
	}
}

// CRW-1489: a step TMPDIR's name has a fixed width: its prefix and 8 lowercase hex characters.
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
		if len(filepath.Base(dir)) != len("t")+8 {
			t.Errorf("%s has a %d-byte name, want %d", dir, len(filepath.Base(dir)), len("t")+8)
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

// localWhereStep is a step that writes its $TMPDIR and $HOME, one per line, to marker.
func localWhereStep(marker string) string {
	return fmt.Sprintf("printf '%%s\\n%%s' \"$TMPDIR\" \"$HOME\" > %q", marker)
}

// localReadWhere is the TMPDIR and HOME a localWhereStep wrote.
func localReadWhere(t *testing.T, marker string) (string, string) {
	t.Helper()
	tmp, home, ok := strings.Cut(localReadMarker(t, marker), "\n")
	if !ok {
		t.Fatalf("the marker %s holds no HOME line", marker)
	}
	return tmp, home
}

// CRW-1489 (B): the record names where it ran: the directory crw-dev ran in, the repository root, the
// caller's TMPDIR, and the TMPDIR and HOME its steps saw. The member is written, a reader of the record
// accepts it, and the relay's own digest of the record's bytes is the digest it carries.
func TestLocal_the_record_names_where_it_ran(t *testing.T) {
	callerTmp := t.TempDir()
	marker := filepath.Join(t.TempDir(), "where")
	t.Setenv("TMPDIR", callerTmp)
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan(localWhereStep(marker)), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %#v", made.Result, made.Jobs)
	}
	stepTmp, stepHome := localReadWhere(t, marker)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := recordRunEnv{Cwd: cwd, Root: repo.root, CallerTmpDir: callerTmp, TmpDir: stepTmp, Home: stepHome}
	if made.RunEnv == nil || *made.RunEnv != want {
		t.Fatalf("the record's runEnv is %+v, want %+v", made.RunEnv, want)
	}
	sealed, err := writeRecord(record, made)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	member, ok := fields["runEnv"].(map[string]any)
	if !ok {
		t.Fatalf("the written record has no runEnv object: %s", raw)
	}
	for name, value := range map[string]string{"cwd": cwd, "root": repo.root, "callerTmpDir": callerTmp, "tmpDir": stepTmp, "home": stepHome} {
		if member[name] != value {
			t.Errorf("runEnv.%s = %v, want %q", name, member[name], value)
		}
	}
	back, err := readRecord(record)
	if err != nil {
		t.Fatalf("the record does not read back: %v", err)
	}
	if back.RunEnv == nil || *back.RunEnv != want {
		t.Errorf("the record reads back with runEnv %+v, want %+v", back.RunEnv, want)
	}
	digest, err := dagsched.VerificationRecordDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if digest != sealed.Digest {
		t.Errorf("the relay's digest of the record is %s, the record carries %s", digest, sealed.Digest)
	}
}

// CRW-1489 (B): a checkout that fails still makes a record, and that record names where it ran too.
func TestLocal_a_failed_checkout_record_names_where_it_ran(t *testing.T) {
	callerTmp := t.TempDir()
	t.Setenv("TMPDIR", callerTmp)
	repo := newLocalFixture(t)
	runGit(repo.root, "config", "filter.up.smudge", "tr a-z A-Z")
	runGit(repo.root, "config", "filter.up.clean", "tr A-Z a-z")
	repo.write(".gitattributes", "file.txt filter=up\n")
	repo.commit()
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("cat file.txt"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Fatalf("the record passes a checkout that a filter changed (result %q)", made.Result)
	}
	if made.RunEnv == nil {
		t.Fatal("the failed checkout's record names no runEnv")
	}
	if filepath.Dir(made.RunEnv.TmpDir) != callerTmp || !localStepTmpName("ci").MatchString(filepath.Base(made.RunEnv.TmpDir)) {
		t.Errorf("the failed checkout's runEnv.tmpDir is %q, want ci<8 hex> directly below %s", made.RunEnv.TmpDir, callerTmp)
	}
	if made.RunEnv.Home == "" || filepath.Base(made.RunEnv.Home) != "home" {
		t.Errorf("the failed checkout's runEnv.home is %q, want the run's own home", made.RunEnv.Home)
	}
	if made.RunEnv.CallerTmpDir != callerTmp || made.RunEnv.Root != repo.root {
		t.Errorf("the failed checkout's runEnv is %+v", made.RunEnv)
	}
}

// CRW-1489 (B): the member is additive. A record without it (every record written before it) has no
// runEnv member, reads back, and keeps its digest: dropping the member from a record that has one and
// resealing it by the canonical rule gives the digest the record without it was sealed with.
func TestLocal_a_record_written_before_runEnv_still_reads(t *testing.T) {
	base := verificationRecord{Runner: "local", Repository: "fixture", HeadCommit: "abc", Result: localPass, Sealed: true, IgnoredEnv: []string{}}
	old, err := sealRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, has := fields["runEnv"]; has {
		t.Errorf("a record with no runEnv writes the member: %s", raw)
	}
	path := filepath.Join(t.TempDir(), "old.json")
	if _, err := writeRecord(path, base); err != nil {
		t.Fatal(err)
	}
	back, err := readRecord(path)
	if err != nil {
		t.Fatalf("a record written without runEnv does not read: %v", err)
	}
	if back.RunEnv != nil || back.Digest != old.Digest {
		t.Errorf("the record reads back with runEnv %+v and digest %s, want none and %s", back.RunEnv, back.Digest, old.Digest)
	}
	withEnv := base
	withEnv.RunEnv = &recordRunEnv{Cwd: "/c", Root: "/r", CallerTmpDir: "", TmpDir: "/t/ci0000abcd", Home: "/t/run/home"}
	sealed, err := sealRecord(withEnv)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Digest == old.Digest {
		t.Error("the runEnv member is not part of the digest")
	}
	raw, err = json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	fields = map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	member, ok := fields["runEnv"].(map[string]any)
	if !ok {
		t.Fatalf("a record with runEnv lacks the member: %s", raw)
	}
	if got, has := member["callerTmpDir"]; !has || got != "" {
		t.Errorf("an unset caller TMPDIR is %v (present %v), want an empty string", got, has)
	}
	delete(fields, "runEnv")
	dropped, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := dagsched.VerificationRecordDigest(dropped)
	if err != nil {
		t.Fatal(err)
	}
	if digest != old.Digest {
		t.Errorf("without its runEnv the record reseals to %s, the record written before it carries %s", digest, old.Digest)
	}
}

// CRW-1489 (B): runEnv is not a reuse key. Two runs of one tree under different caller TMPDIRs: the
// second is answered by the first's record, which keeps the runEnv of the run that verified it.
func TestLocalReuse_runEnv_is_not_a_reuse_key(t *testing.T) {
	// The key comparison alone: records that differ only in runEnv match.
	a := verificationRecord{Schema: recordSchema, Result: localPass, RunEnv: &recordRunEnv{TmpDir: "/a", CallerTmpDir: "/x"}}
	b := verificationRecord{Schema: recordSchema, Result: localPass, RunEnv: &recordRunEnv{TmpDir: "/b", CallerTmpDir: "/y"}}
	if ok, why := localReuse(a, b); !ok {
		t.Errorf("records that differ only in runEnv do not match: %s", why)
	}
	if ok, why := localReuse(a, verificationRecord{Schema: recordSchema, Result: localPass}); !ok {
		t.Errorf("a record with runEnv does not match a current state without it: %s", why)
	}
	first := t.TempDir()
	t.Setenv("TMPDIR", first)
	repo := newLocalFixture(t)
	node := localToolVersions(localPathEnv(nil))["node"]
	if node == "" {
		t.Skip("no node on PATH, so the fixture's Node pin cannot match this host")
	}
	if workflow := strings.Replace(localFixtureWorkflow, "'24.20.0'", "'"+node+"'", 1); workflow != localFixtureWorkflow {
		repo.write(".github/workflows/ci.yml", workflow)
		repo.commit()
	}
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan("echo hello"), record)
	made, reused, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reused || made.Result != localPass || len(made.PinMismatch) != 0 {
		t.Fatalf("the first run: reused %v, result %q, pin mismatch %v", reused, made.Result, made.PinMismatch)
	}
	if made.RunEnv == nil || made.RunEnv.CallerTmpDir != first {
		t.Fatalf("the first run's runEnv is %+v, want the caller TMPDIR %s", made.RunEnv, first)
	}
	if _, err := writeRecord(record, made); err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	t.Setenv("TMPDIR", second)
	opts = localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "again.json"))
	again, reused, err := localVerify(opts, record, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("a run under another caller TMPDIR is not answered by the record: runEnv acted as a reuse key")
	}
	if again.RunEnv == nil || *again.RunEnv != *made.RunEnv {
		t.Errorf("the reused record's runEnv is %+v, want the verifying run's %+v", again.RunEnv, made.RunEnv)
	}
}

// CRW-1494: localGitAbove walks the path as written and its resolved form. A link that points into a
// repository is the case only the resolved form meets: the written path (through the link) never
// reaches the repository's .git, the resolved one does. The ceiling keeps a stray .git above the
// temp root out of the walk.
func TestLocal_git_above_follows_a_link_to_a_parent_with_a_git(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoGit := filepath.Join(base, "repo", ".git")
	for _, d := range []string{filepath.Join(base, "repo", "sub", "tmp"), filepath.Join(base, "plain", "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGitOrFail(t, filepath.Join(base, "repo"), "init", "-q")
	if err := os.Symlink(filepath.Join(base, "repo", "sub"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	ceilings := []string{filepath.Dir(base)}
	got, err := localGitAbove(filepath.Join(base, "link", "tmp"), ceilings)
	if err != nil || got != repoGit {
		t.Errorf("a directory below a link to a repository: want %q, got %q, %v", repoGit, got, err)
	}
	if got, err := localGitAbove(filepath.Join(base, "plain", "tmp"), ceilings); err != nil || got != "" {
		t.Errorf("a directory with no repository above it and no link: want none, got %q, %v", got, err)
	}
}

// CRW-1569: a long TMPDIR is accepted as it is. No test socket lies below TMPDIR (CRW-1568), so its
// length is no reason to refuse it or to move the steps elsewhere: the step TMPDIR is ci<8 hex>
// directly below the resolved 120-byte TMPDIR, and the run passes.
func TestLocal_accepts_a_long_TMPDIR(t *testing.T) {
	repo := newLocalFixture(t)
	long := t.TempDir()
	if len(long) > 118 {
		t.Fatalf("the test's temporary directory %s is already %d bytes", long, len(long))
	}
	long = filepath.Join(long, strings.Repeat("l", 120-len(long)-1))
	if len(long) != 120 {
		t.Fatalf("the long TMPDIR %s is %d bytes, want 120", long, len(long))
	}
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "tmpdir")
	t.Setenv("TMPDIR", long)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), filepath.Join(t.TempDir(), "record.json"))
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatalf("a 120-byte TMPDIR is refused: %v", err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %#v", made.Result, made.Jobs)
	}
	resolved, err := filepath.EvalSymlinks(long)
	if err != nil {
		t.Fatal(err)
	}
	stepTmp := localReadMarker(t, marker)
	if filepath.Dir(stepTmp) != resolved || !localStepTmpName("ci").MatchString(filepath.Base(stepTmp)) {
		t.Errorf("the step's TMPDIR is %s, want ci<8 hex> directly below %s", stepTmp, resolved)
	}
	localNoEntry(t, repo.work, "t")
}

// CRW-1588: a .git that holds no repository (the empty directory a sandbox leaves behind, or a file
// that is no gitfile) is no marker: the step TMPDIR is made below such a TMPDIR. A marker that resolves
// to a real Git directory still refuses (TestLocal_never_moves_the_step_TMPDIR).
func TestLocalStepTempDirIgnoresNonRepositoryMarkers(t *testing.T) {
	for _, kind := range []string{"empty directory", "garbage file"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			marker := filepath.Join(base, ".git")
			if kind == "empty directory" {
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(marker, []byte("not a gitfile\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tmp := filepath.Join(base, "tmp")
			if err := os.Mkdir(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", tmp)
			made, err := localStepTempDir(localOptions{gitCeilings: []string{filepath.Dir(base)}})
			if err != nil {
				t.Fatalf("non-repository marker rejected: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(made) })
			if filepath.Dir(made) != tmp || !localStepTmpName("ci").MatchString(filepath.Base(made)) {
				t.Fatalf("step TMPDIR %q is not directly below %q", made, tmp)
			}
		})
	}
}

// CRW-1569: the step TMPDIR is never moved. A TMPDIR below a .git is refused with temp_dir_unusable
// naming the .git and the way out (set TMPDIR), before any checkout or step: no step ran, no record was
// written, no step TMPDIR was made below the TMPDIR, and nothing was made below the work root. The base
// offered the work root as a second place instead.
func TestLocal_never_moves_the_step_TMPDIR(t *testing.T) {
	repo := newLocalFixture(t)
	p := t.TempDir()
	runGitOrFail(t, p, "init", "-q")
	callerTmp := filepath.Join(p, "tmp")
	if err := os.Mkdir(callerTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv("TMPDIR", callerTmp)
	opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), record)
	opts.gitCeilings = []string{filepath.Dir(p), filepath.Dir(repo.work)}
	made, _, err := localVerify(opts, "", io.Discard)
	if err == nil {
		t.Fatalf("a TMPDIR below a .git is not refused: the run is %q", made.Result)
	}
	gitDir, err2 := filepath.EvalSymlinks(filepath.Join(p, ".git"))
	if err2 != nil {
		t.Fatal(err2)
	}
	for _, want := range []string{"temp_dir_unusable", gitDir, "set TMPDIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	for _, unwanted := range []string{"work root", "--work-root"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the refusal %q offers %q: the step TMPDIR has no second place", err, unwanted)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a step ran")
	}
	if _, err := os.Lstat(record); err == nil {
		t.Error("a record was written")
	}
	// The version probes make (and remove) their own directories in TMPDIR, as they do for any run; no
	// step TMPDIR is made there.
	localNoEntry(t, callerTmp, "ci")
	localNoEntry(t, repo.work, "")
}

// CRW-1569: ci local makes and removes a socket folder below the socket root before any checkout, and
// a folder that cannot be made, or that leaves no room for the longest socket tail within the bound, is
// socket_dir_unusable with its reason (for a long root: the bytes needed, the bound and how far over).
// No step ran, no record was written, and nothing was made below TMPDIR or the work root.
func TestLocal_refuses_when_the_socket_dir_does_not_fit(t *testing.T) {
	repo := newLocalFixture(t)
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(base, strings.Repeat("s", tmploc.SocketBound))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	callerTmp := t.TempDir()
	t.Setenv("TMPDIR", callerTmp)
	for _, c := range []struct {
		name, root string
		want       []string
	}{
		{"a regular file", file, []string{"socket_dir_unusable", file}},
		{"a missing root", filepath.Join(base, "missing"), []string{"socket_dir_unusable", filepath.Join(base, "missing")}},
		{"too long", long, []string{"socket_dir_unusable", long, "bytes", fmt.Sprint(tmploc.SocketBound), "over", fmt.Sprint(tmploc.SocketTailMax)}},
	} {
		marker := filepath.Join(t.TempDir(), "ran")
		record := filepath.Join(t.TempDir(), "record.json")
		opts := localRunOptions(repo, localFixturePlan(localMarkerStep("true", marker)), record)
		opts.socketRoot = c.root
		made, _, err := localVerify(opts, "", io.Discard)
		if err == nil {
			t.Errorf("%s: the socket root %s is not refused: the run is %q", c.name, c.root, made.Result)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal %q does not name %q", c.name, err, want)
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: a step ran", c.name)
		}
		if _, err := os.Lstat(record); err == nil {
			t.Errorf("%s: a record was written", c.name)
		}
		localNoEntry(t, callerTmp, "ci")
		localNoEntry(t, repo.work, "")
		localNoEntry(t, long, "")
	}
	// the real socket root, as the command takes it, fits
	opts := localRunOptions(repo, localFixturePlan("true"), filepath.Join(t.TempDir(), "record.json"))
	if made, _, err := localVerify(opts, "", io.Discard); err != nil || made.Result != localPass {
		t.Errorf("the socket root %s is refused: %v (%q)", tmploc.SocketRoot, err, made.Result)
	}
}
