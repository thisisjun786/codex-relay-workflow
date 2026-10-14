package job

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-1375: the bg store refuses a symbolic link anywhere in .crw/bg, as the rest of the .crw tree does (CRW-475).

// listing is every entry below dir, links named as links, with the content of each regular file, so a test can show that nothing
// the link leads to changed.
func listing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		entry := strings.TrimPrefix(path, dir)
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			entry += " -> " + target
		case d.Type().IsRegular():
			data, _ := os.ReadFile(path)
			entry += "=" + string(data)
		}
		out = append(out, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// refusedLink is whether err refuses path as a symbolic link: it wraps crwdir.ErrLink and names path.
func refusedLink(err error, path string) bool {
	return errors.Is(err, crwdir.ErrLink) && strings.Contains(err.Error(), path)
}

// A link at .crw/bg is refused by every writer of the store, whether it leads inside the workspace (which CRW-1134 accepted),
// outside it or nowhere: the error names the link, nothing is written, created or removed where it leads, and the link stays.
func TestTheBgWritersRefuseALinkAtTheBgDirectory(t *testing.T) {
	for _, where := range []string{"inside", "outside", "dangling"} {
		t.Run(where, func(t *testing.T) {
			ws := workspace(t)
			target := filepath.Join(ws, "elsewhere")
			switch where {
			case "outside":
				target = filepath.Join(workspace(t), "bg")
			case "dangling":
				target = filepath.Join(ws, "nowhere")
			}
			if where != "dangling" {
				put(t, filepath.Join(target, DisabledFile), "keep")
			}
			mkdir(t, filepath.Join(ws, crwdir.DirName))
			link := BGDir(ws)
			symlink(t, target, link)
			before := listing(t, filepath.Dir(target))
			started := false
			writers := map[string]func() error{
				"EnsureDir":    func() error { _, err := EnsureDir(ws); return err },
				"AtomicWrite":  func() error { return AtomicWrite(ws, RecordPath(ws, "r"), "x") },
				"appendLedger": func() error { return appendLedger(ws, Event{{"event", "x"}}, time.Now) },
				"RemovePath":   func() error { return RemovePath(ws, DisabledPath(ws)) },
				"lockStore": func() error {
					unlock, err := lockStore(ws)
					if err == nil {
						unlock()
					}
					return err
				},
				"runBackground": func() error {
					_, err := runBackground(ws, RunOptions{Command: []string{"true"}}, time.Now, func(*exec.Cmd) error { started = true; return nil })
					return err
				},
			}
			for name, write := range writers {
				if err := write(); !refusedLink(err, link) {
					t.Errorf("%s: %v, want a refusal naming %s as a symbolic link", name, err, link)
				}
			}
			if started {
				t.Error("a job was started through the link")
			}
			if after := listing(t, filepath.Dir(target)); after != before {
				t.Errorf("what the link leads to changed:\n%s\nwas\n%s", after, before)
			}
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the link was not kept: %v", err)
			}
		})
	}
}

// A link at a file of the store, the record, a switch file or the ledger, is refused by its writer, naming it: the file it leads
// to is not written and the link is not replaced.
func TestTheBgWritersRefuseALinkAtTheirFile(t *testing.T) {
	ws := workspace(t)
	store(t, ws)
	victim := filepath.Join(workspace(t), "victim")
	put(t, victim, "keep")
	for name, c := range map[string]struct {
		path  string
		write func() error
	}{
		"record":     {RecordPath(ws, "r"), func() error { return AtomicWrite(ws, RecordPath(ws, "r"), "x") }},
		"disabled":   {DisabledPath(ws), func() error { return AtomicWrite(ws, DisabledPath(ws), "x") }},
		"enabled-at": {EnabledAtPath(ws), func() error { return AtomicWrite(ws, EnabledAtPath(ws), "x") }},
		"ledger":     {filepath.Join(BGDir(ws), LedgerFile), func() error { return appendLedger(ws, Event{{"event", "x"}}, time.Now) }},
	} {
		symlink(t, victim, c.path)
		if err := c.write(); !refusedLink(err, c.path) {
			t.Errorf("%s: %v, want a refusal naming %s as a symbolic link", name, err, c.path)
		}
		if target, err := os.Readlink(c.path); err != nil || target != victim {
			t.Errorf("%s: the link was not kept: %q %v", name, target, err)
		}
		if get(t, victim) != "keep" {
			t.Errorf("%s: the link target holds %q", name, get(t, victim))
		}
		if err := os.Remove(c.path); err != nil {
			t.Fatal(err)
		}
	}
}

// The shell writes the job's output and its exit code into files the launcher created before the shell ran, without following a
// link, so links put in their place after the stale files were cleared (here at the start itself) lead nowhere: the files they
// point to keep their content, and the exit code is not moved into a directory a link at <id>.exit leads to.
func TestAShellTargetLinkedAfterTheClearingIsNotFollowed(t *testing.T) {
	ws := workspace(t)
	outside := workspace(t)
	victimOut, victimExit, victimDir := filepath.Join(outside, "out"), filepath.Join(outside, "exit"), filepath.Join(outside, "dir")
	put(t, victimOut, "KEEP")
	put(t, victimExit, "KEEP")
	mkdir(t, victimDir)
	out, exit := OutPath(ws, "fixed"), ExitPath(ws, "fixed")
	plant := func(target, link string) {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		symlink(t, target, link)
	}
	rec, err := runBackground(ws, RunOptions{ID: "fixed", Command: []string{"echo", "new"}}, ticking(), func(cmd *exec.Cmd) error {
		plant(victimOut, out)
		plant(victimExit, exit+".tmp")
		plant(victimDir, exit)
		return cmd.Start()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if rec.PID != nil {
			_ = syscall.Kill(-*rec.PID, syscall.SIGKILL)
		}
	})
	until(t, "the shell to publish its exit code", func() bool { return !exists(exit + ".tmp") })
	if get(t, victimOut) != "KEEP" || get(t, victimExit) != "KEEP" {
		t.Errorf("a link planted after the clearing was written through: out %q, exit %q", get(t, victimOut), get(t, victimExit))
	}
	if left := listing(t, victimDir); left != "" {
		t.Errorf("the exit code was moved into the directory a link at <id>.exit leads to: %s", left)
	}
}

// A shell target is created exclusively without following a link: a link at its path (put there between the clearing and the
// creation) is refused naming it, and its target is untouched; any other entry there is not opened either.
func TestAShellTargetIsCreatedWithoutFollowingALink(t *testing.T) {
	ws := workspace(t)
	store(t, ws)
	victim := filepath.Join(workspace(t), "victim")
	put(t, victim, "keep")
	link := OutPath(ws, "j")
	symlink(t, victim, link)
	if f, err := createShellTarget(link); f != nil || !refusedLink(err, link) {
		t.Errorf("a link at the target: %v, want a refusal naming %s as a symbolic link", err, link)
	}
	if get(t, victim) != "keep" {
		t.Errorf("the link target holds %q", get(t, victim))
	}
	planted := ExitPath(ws, "j") + ".tmp"
	put(t, planted, "planted")
	if f, err := createShellTarget(planted); f != nil || !errors.Is(err, os.ErrExist) {
		t.Errorf("a file at the target: %v", err)
	}
	if get(t, planted) != "planted" {
		t.Errorf("a planted file was opened: %q", get(t, planted))
	}
	fresh := OutPath(ws, "k")
	f, err := createShellTarget(fresh)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if info, err := os.Lstat(fresh); err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Errorf("a new target: %v %v", info, err)
	}
}

// The hooks fail open on a linked .crw/bg: they answer nothing, as on any store failure, and change nothing where the link leads.
func TestTheBgHooksFailOpenOnALinkedBgDirectory(t *testing.T) {
	ws := workspace(t)
	hookDone(t, ws, "due", "S1")
	elsewhere := filepath.Join(ws, "elsewhere")
	if err := os.Rename(BGDir(ws), elsewhere); err != nil {
		t.Fatal(err)
	}
	symlink(t, elsewhere, BGDir(ws))
	before := listing(t, elsewhere)
	p := HookPayload{SessionID: "S1", Cwd: ws}
	for name, hook := range map[string]func() string{
		"Stop":             func() string { return HandleStop(p, ws, hookEnv(nil), noon) },
		"UserPromptSubmit": func() string { return HandleUserPromptSubmit(p, ws, hookEnv(nil), noon) },
		"SessionStart":     func() string { return HandleSessionStart(p, ws, hookEnv(nil), noon) },
	} {
		if out := hook(); out != "" {
			t.Errorf("%s answered through the link: %q", name, out)
		}
	}
	if after := listing(t, elsewhere); after != before {
		t.Errorf("the hooks wrote where the link leads:\n%s\nwas\n%s", after, before)
	}
}

// The CLI reports the refusal as an input error naming the link.
func TestTheBgCLIRefusesALinkedBgDirectory(t *testing.T) {
	ws := workspace(t)
	mkdir(t, filepath.Join(ws, "elsewhere"))
	mkdir(t, filepath.Join(ws, crwdir.DirName))
	symlink(t, filepath.Join(ws, "elsewhere"), BGDir(ws))
	for _, argv := range [][]string{{"run", "--", "true"}, {"off"}} {
		if _, err := RunCLI(argv, ws, os.LookupEnv, time.Now); !refusedLink(err, BGDir(ws)) {
			t.Errorf("%v: %v, want a refusal naming %s as a symbolic link", argv, err, BGDir(ws))
		}
	}
	if left := listing(t, filepath.Join(ws, "elsewhere")); left != "" {
		t.Errorf("the CLI wrote where the link leads: %s", left)
	}
}

// A job over without an exit code (gone without one, or cancelled once its group is gone) leaves no <id>.exit.tmp when that is still
// the empty file the launcher created for the shell: the oracle's shell made the file only to write the code, so its store has none
// (corpus cli__bg__cancel_running_job_kills_group). A temporary file the shell has written is kept, and so is a link.
func TestAJobOverWithoutAnExitCodeLeavesNoUnwrittenExitTemp(t *testing.T) {
	pid := deadPID(t)
	for _, status := range []BgStatus{StatusRunning, StatusCancelRequested} {
		for _, body := range []string{"", "7"} {
			ws := workspace(t)
			r := mk(ws, "e")
			r.Status, r.PID = status, &pid
			save(t, ws, r)
			tmp := ExitPath(ws, "e") + ".tmp"
			put(t, tmp, body)
			got, err := Reconcile(ws, r, noonClock)
			if err != nil || !IsTerminal(got.Status) || got.ExitCode != nil {
				t.Fatalf("%s %q: %+v %v", status, body, got, err)
			}
			if exists(tmp) != (body != "") {
				t.Errorf("%s %q: the temporary exit file is there: %v", status, body, exists(tmp))
			}
		}
	}
	ws := workspace(t)
	r := mk(ws, "e")
	r.PID = &pid
	save(t, ws, r)
	victim := filepath.Join(workspace(t), "victim")
	put(t, victim, "")
	symlink(t, victim, ExitPath(ws, "e")+".tmp")
	if got, err := Reconcile(ws, r, noonClock); err != nil || !IsTerminal(got.Status) || !exists(ExitPath(ws, "e")+".tmp") || !exists(victim) {
		t.Errorf("a link at the temporary exit file: %+v %v", got, err)
	}
}
