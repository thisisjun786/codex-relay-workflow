package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

const toolsPassRecord = `{"schema":"verification-record/1","result":"pass","digest":"sha256:ff","jobs":[]}`

// toolsTree is a worktree that carries its own cmd/crw-dev, as a CRW checkout does.
func toolsTree(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "cmd", "crw-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	return tree
}

// CRW-1319: with neither crw-dev nor go on PATH and a Go root in the configuration, the verifier runs `crw-dev ci local` through
// that Go: the verified tree's own crw-dev by go run, with the configured Go first on the PATH its steps see.
func TestCILocalVerifierRunsThroughTheConfiguredTools(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	tree := toolsTree(t)
	var ran struct {
		dir, name string
		env, args []string
	}
	run := func(_ context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		ran.dir, ran.name, ran.env, ran.args = dir, name, env, args
		path := args[slices.Index(args, "--record")+1]
		return 0, nil, os.WriteFile(path, []byte(toolsPassRecord), 0o644)
	}
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{GoRoot: "/opt/go"}, RecordDir: t.TempDir(), Run: run}
	out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: strings.Repeat("a", 40), Worktree: tree, Base: strings.Repeat("b", 40)})
	if err != nil || !out.Pass || out.Note != "" {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if ran.dir != tree || ran.name != "/opt/go/bin/go" || !slices.Equal(ran.args[:6], []string{"run", "-tags", "dev", "./cmd/crw-dev", "ci", "local"}) ||
		!slices.Contains(ran.args, "--base") || !slices.Contains(ran.env, "PATH=/opt/go/bin:"+empty) || !slices.Contains(ran.env, "GOROOT=/opt/go") {
		t.Errorf("ran %s %v in %s with %v", ran.name, ran.args, ran.dir, ran.env)
	}
	// a configured crw-dev binary runs as it is, with the configured Go on its PATH
	v.Tools.CrwDev = "/x/crw-dev"
	if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: strings.Repeat("a", 40), Worktree: tree}); err != nil || !out.Pass ||
		ran.name != "/x/crw-dev" || !slices.Equal(ran.args[:2], []string{"ci", "local"}) || !slices.Contains(ran.env, "PATH=/opt/go/bin:"+empty) {
		t.Errorf("binary: ran %s %v with %v: %+v %v", ran.name, ran.args, ran.env, out, err)
	}
}

// CRW-1319: with nothing configured the verifier takes crw-dev from PATH (fail-open) and says so in a note; with nothing on PATH
// either, the verification did not run and its error is the reason the node is held, naming the setting that is missing.
func TestCILocalVerifierWithNothingConfiguredFallsBackToPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{Config: "/home/u/.config/crw/config.json"}, RecordDir: t.TempDir()}
	req := VerifyRequest{Node: "A", Head: strings.Repeat("a", 40), Worktree: t.TempDir()}
	_, err := v.Verify(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), "verifier.go_root") ||
		!strings.Contains(err.Error(), "/home/u/.config/crw/config.json") {
		t.Fatalf("nothing configured, nothing on PATH: %v", err)
	}
	v.Run = func(_ context.Context, _ string, env []string, name string, args ...string) (int, []byte, error) {
		if name != "crw-dev" || env != nil {
			t.Errorf("fail-open ran %s with %v", name, env)
		}
		return 0, nil, os.WriteFile(args[slices.Index(args, "--record")+1], []byte(toolsPassRecord), 0o644)
	}
	out, err := v.Verify(context.Background(), req)
	if err != nil || !out.Pass || !strings.Contains(out.Note, "fail-open") {
		t.Errorf("fail-open: %+v %v", out, err)
	}
}

// CRW-1319: the note a verifier gives with its outcome is recorded on the crw.verify row of the journal.
func TestVerifyRecordsTheVerifierNote(t *testing.T) {
	w := newWorld(t)
	ver := fakeVerifier{out: VerifyOutcome{Pass: true, Command: CILocalCommand, Digest: "sha256:ab", Note: "crw-dev and go come from PATH (fail-open)"}}
	w.lane.d.Verifier = &ver
	var res VerifyResult
	_ = json.Unmarshal(w.must("verify", "A", strings.Repeat("a", 40)), &res)
	row := w.j.Events()[len(w.j.Events())-1]
	if !res.Pass || row.Data["verifier_note"] != ver.out.Note {
		t.Errorf("the row: %v (%+v)", row.Data, res)
	}
	ver.out.Note = ""
	_ = json.Unmarshal(w.must("verify", "A", strings.Repeat("b", 40)), &res)
	if row := w.j.Events()[len(w.j.Events())-1]; row.Data["verifier_note"] != nil {
		t.Errorf("a verification without a note recorded one: %v", row.Data)
	}
}

// recordingRun is a RunCommand that writes a passing record where --record points and remembers that path.
func recordingRun(record *string, calls *int) RunCommand {
	return func(_ context.Context, _ string, _ []string, _ string, args ...string) (int, []byte, error) {
		*calls++
		*record = args[slices.Index(args, "--record")+1]
		return 0, nil, os.WriteFile(*record, []byte(toolsPassRecord), 0o644)
	}
}

// within reports whether path is dir or below it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CRW-1489: the shipped verifier (cli/laneops.go builds it as CILocalVerifier{Tools: tools}, with no RecordDir) writes its
// records under the worktree's own git directory, not under the caller's TMPDIR: a lane whose TMPDIR is a shared /tmp no longer
// puts its record where another lane's run or a sandbox marker can reach it.
func TestCILocalVerifier_the_default_record_dir_does_not_follow_TMPDIR(t *testing.T) {
	wt := t.TempDir()
	if err := os.Mkdir(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	x := t.TempDir()
	t.Setenv("TMPDIR", x)
	if within(x, wt) {
		t.Fatalf("setup: the worktree %s is under the TMPDIR %s", wt, x)
	}
	var record string
	var calls int
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
	head := strings.Repeat("c", 40)
	out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: wt})
	if err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if within(x, record) {
		t.Errorf("the record %s follows TMPDIR %s", record, x)
	}
	if want := filepath.Join(wt, ".git", "crw-verify", "verification-record-"+head+".json"); record != want {
		t.Errorf("the record is %s, want %s", record, want)
	}
}

// CRW-1489: in a linked worktree .git is a file naming the per-worktree git directory (absolute, or relative to the worktree),
// and that directory lies outside the checkout: in the repository's common git directory, which may be in the account's .codex,
// where crw-dev refuses a record that leaves the checkout. The record goes inside the checkout instead, in .crw/verify (ignored by
// git: crwdir writes .crw/.gitignore when it makes .crw), so two lanes of one repository still never share a record file, and
// nothing is written below the gitdir.
func TestCILocalVerifier_a_linked_worktree_records_inside_its_checkout(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute", true: "relative"}[relative], func(t *testing.T) {
			base := t.TempDir()
			wt := filepath.Join(base, "wt")
			g := filepath.Join(base, "main", ".git", "worktrees", "wt")
			for _, d := range []string{wt, g} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target := g
			if relative {
				var err error
				if target, err = filepath.Rel(wt, g); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", t.TempDir())
			var record string
			var calls int
			v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
			head := strings.Repeat("d", 40)
			if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: wt}); err != nil || !out.Pass {
				t.Fatalf("verify: %+v %v", out, err)
			}
			if want := filepath.Join(wt, crwdir.DirName, "verify", "verification-record-"+head+".json"); record != want {
				t.Errorf("the record is %s, want %s", record, want)
			}
			if data, err := os.ReadFile(filepath.Join(wt, crwdir.DirName, ".gitignore")); err != nil || string(data) != crwdir.GitignoreText {
				t.Errorf("the checkout's .crw/.gitignore is %q (%v), want the crwdir ignore text", data, err)
			}
			if _, err := os.Lstat(filepath.Join(g, "crw-verify")); !os.IsNotExist(err) {
				t.Errorf("something was written below the gitdir %s: %v", g, err)
			}
		})
	}
}

// CRW-1489 (PR ping-pong): the records of a linked worktree stay out of git even when the checkout's .crw already exists without
// crwdir's ignore file and the repository has no .crw rule of its own: .crw/verify carries its own ignore file, so neither git
// status nor the working snapshot (Snapshot, add -A) sees a record.
func TestCILocalVerifier_linked_worktree_records_stay_out_of_git(t *testing.T) {
	repo, first := newRepo(t)
	wt := addWorktree(t, repo, "wt", first)
	if err := os.Mkdir(filepath.Join(wt, crwdir.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	var record string
	var calls int
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
	head := strings.Repeat("f", 40)
	if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: wt}); err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if !within(filepath.Join(wt, crwdir.DirName, "verify"), record) {
		t.Fatalf("the record %s is not in the checkout's .crw/verify", record)
	}
	if status := gitIn(t, wt, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Errorf("git sees the verify records: %q", status)
	}
	snap, err := Snapshot(ctx, ExecGit("git"), wt)
	if err != nil {
		t.Fatal(err)
	}
	if tree := gitIn(t, wt, "rev-parse", "HEAD^{tree}"); snap != tree {
		t.Errorf("the working snapshot %s holds more than HEAD's tree %s: %q", snap, tree, gitIn(t, wt, "ls-tree", "-r", "--name-only", snap))
	}
}

// CRW-1489: a worktree with no git directory has no default record dir; Verify says so before it runs the command.
func TestCILocalVerifier_no_git_directory_is_an_error_before_the_command(t *testing.T) {
	var record string
	var calls int
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
	wt := t.TempDir()
	_, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: strings.Repeat("e", 40), Worktree: wt})
	if err == nil || !strings.Contains(err.Error(), "ci_record_dir") || !strings.Contains(err.Error(), wt) {
		t.Errorf("no git directory: %v", err)
	}
	if calls != 0 {
		t.Errorf("the command ran %d times", calls)
	}
}

// CRW-1489 (PR ping-pong 2): a .git that is a symbolic link to a directory is not the checkout's own directory entry, and its
// target may lie in a protected place (a repository below the account's .codex), where crw-dev refuses a record. So only a .git
// that is a real directory (Lstat) holds crw-verify; a linked .git, like a .git file, gets .crw/verify, and nothing is written
// through the link. A linked .git that leads to no directory is the "no git directory" error before the command runs.
func TestCILocalVerifier_a_symlinked_git_records_inside_its_checkout(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	target := filepath.Join(base, "protected", "gitdir")
	for _, d := range []string{wt, target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	var record string
	var calls int
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
	head := strings.Repeat("a", 40)
	if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: wt}); err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if want := filepath.Join(wt, crwdir.DirName, "verify", "verification-record-"+head+".json"); record != want {
		t.Errorf("the record is %s, want %s", record, want)
	}
	if _, err := os.Lstat(filepath.Join(target, "crw-verify")); !os.IsNotExist(err) {
		t.Errorf("something was written through the linked .git into %s: %v", target, err)
	}

	// a .git linked to a git file (a gitdir: line, as git accepts it) is read as that file: the checkout verifies and records in .crw/verify
	gitfileWt := filepath.Join(base, "wt-gitfile")
	admin := filepath.Join(base, "main", ".git", "worktrees", "wt-gitfile")
	for _, d := range []string{gitfileWt, admin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitfile := filepath.Join(base, "gitfile")
	if err := os.WriteFile(gitfile, []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitfile, filepath.Join(gitfileWt, ".git")); err != nil {
		t.Fatal(err)
	}
	if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: gitfileWt}); err != nil || !out.Pass {
		t.Fatalf("verify of a .git linked to a git file: %+v %v", out, err)
	}
	if want := filepath.Join(gitfileWt, crwdir.DirName, "verify", "verification-record-"+head+".json"); record != want {
		t.Errorf("the record of a .git linked to a git file is %s, want %s", record, want)
	}

	file := filepath.Join(base, "afile")
	if err := os.WriteFile(file, []byte("not a git file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, to := range map[string]string{"to a file": file, "to nothing": filepath.Join(base, "missing")} {
		t.Run(name, func(t *testing.T) {
			wt := t.TempDir()
			if err := os.Symlink(to, filepath.Join(wt, ".git")); err != nil {
				t.Fatal(err)
			}
			calls = 0
			_, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: wt})
			if err == nil || !strings.Contains(err.Error(), "ci_record_dir") || !strings.Contains(err.Error(), "no git directory") {
				t.Errorf("a .git linked %s: %v", name, err)
			}
			if calls != 0 {
				t.Errorf("the command ran %d times", calls)
			}
		})
	}
}

// CRW-1489 (PR ping-pong 2): a relative gitdir target is resolved as the kernel resolves it, with the links in it followed before
// a "..", not cleaned lexically first: "link/../g" with link leading to base/a/b names base/a/g, which git accepts, while the
// lexical <worktree>/g does not exist. ciGitDir answers the physical path.
func TestCILocalVerifier_a_relative_gitdir_resolves_as_the_kernel_does(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	g := filepath.Join(base, "a", "g")
	for _, d := range []string{wt, filepath.Join(base, "a", "b"), g} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "a", "b"), filepath.Join(wt, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: link/../g\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(g)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ciGitDir(wt); err != nil || got != want {
		t.Errorf("ciGitDir is %q (%v), want the physical %s", got, err, want)
	}
	t.Setenv("TMPDIR", t.TempDir())
	var record string
	var calls int
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
	head := strings.Repeat("b", 40)
	if out, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: head, Worktree: wt}); err != nil || !out.Pass {
		t.Fatalf("verify: %+v %v", out, err)
	}
	if !within(filepath.Join(wt, crwdir.DirName, "verify"), record) {
		t.Errorf("the record %s is not in the checkout's .crw/verify", record)
	}
	if _, err := os.Lstat(filepath.Join(g, "crw-verify")); !os.IsNotExist(err) {
		t.Errorf("something was written below the gitdir %s: %v", g, err)
	}
}

// CRW-1489 (PR ping-pong 2): the ignore file of .crw/verify is created through a descriptor of that directory opened without
// following a link, so a .crw/verify replaced by a symbolic link after it was checked is an error, and nothing is written
// through the link outside the checkout.
func TestCILocalVerifier_a_verify_dir_swapped_for_a_link_is_not_written_through(t *testing.T) {
	repo, first := newRepo(t)
	wt := addWorktree(t, repo, "wt", first)
	outside := t.TempDir()
	made := ciRecordDirMade
	t.Cleanup(func() { ciRecordDirMade = made })
	ciRecordDirMade = func(dir string) {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dir); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", t.TempDir())
	var record string
	var calls int
	v := CILocalVerifier{Tools: crwconfig.VerifierTools{CrwDev: "/x/crw-dev"}, Run: recordingRun(&record, &calls)}
	_, err := v.Verify(context.Background(), VerifyRequest{Node: "A", Head: strings.Repeat("9", 40), Worktree: wt})
	if !errors.Is(err, crwdir.ErrLink) {
		t.Errorf("a .crw/verify swapped for a link: %v, want a refusal of the link", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("the ignore file was written through the link into %s: %v", outside, err)
	}
	if calls != 0 {
		t.Errorf("the command ran %d times", calls)
	}
}
