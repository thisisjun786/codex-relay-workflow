package integrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
)

// Runner runs integration passes over one configured queue. The seams are what a test
// replaces: the program runner, the clock and the sleep between passes.
type Runner struct {
	Cfg   Config
	Exec  Exec
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Env is the environment programs start with (nil: the process's).
	Env []string
	// Prepare, when set, makes in place what the programs of a real run need on disk (the manage
	// command sets it to make the configured build_work). It runs right before the gate,
	// record-version or verification command starts, never in DryRun, which makes no directory.
	Prepare func() error

	// hookMove is a test seam: it runs while a pending file is being moved, after the head the
	// file holds was read and before the file leaves pending/.
	hookMove func(src string)
	// hookLink is a test seam that replaces os.Link when a claimed file is put back into
	// pending (nil: os.Link).
	hookLink func(oldname, newname string) error
}

// NewRunner is a runner over cfg (already Resolved) with the host's seams.
func NewRunner(cfg Config) *Runner {
	return &Runner{Cfg: cfg, Exec: OSExec, Now: time.Now, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// prepare runs the Prepare seam (nil: nothing to do).
func (r *Runner) prepare() error {
	if r.Prepare == nil {
		return nil
	}
	return r.Prepare()
}

func (r *Runner) baseEnv() []string {
	if r.Env != nil {
		return r.Env
	}
	return processEnv()
}

// gitEnv is the environment of a git call that writes: no routing variable inherited, no
// prompt, no editor.
func (r *Runner) gitEnv() []string {
	return gitprobe.Sanitize(r.baseEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
}

// logf appends one line to the runner log, stamped as the shell runner stamped it.
func (r *Runner) logf(format string, args ...any) {
	line := r.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, args...) + "\n"
	if err := os.MkdirAll(r.Cfg.QueueDir, 0o775); err != nil {
		return
	}
	f, err := os.OpenFile(r.qpath(fileLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o664)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = io.WriteString(f, line)
}

// git runs git in dir, writing both streams to log (nil discards), and returns its status.
func (r *Runner) git(ctx context.Context, dir string, log io.Writer, args ...string) (int, error) {
	if log == nil {
		log = io.Discard
	}
	return r.Exec(ctx, Cmd{Argv: append([]string{r.Cfg.Git, "-C", dir}, args...), Env: r.gitEnv(), Stdout: log, Stderr: log})
}

// gitOut runs git in dir and returns its standard output, trimmed. A failure carries the
// standard error.
func (r *Runner) gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	rc, err := r.Exec(ctx, Cmd{Argv: append([]string{r.Cfg.Git, "-C", dir}, args...), Env: r.gitEnv(), Stdout: &out, Stderr: &errb})
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: status %d: %s", strings.Join(args, " "), rc, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitRO runs a read-only probe: the probe policy of gitprobe (no optional locks, no hooks, no
// replacement objects) and no write to the repository.
func (r *Runner) gitRO(ctx context.Context, dir string, args ...string) (string, int, error) {
	var out bytes.Buffer
	rc, err := r.Exec(ctx, Cmd{Argv: append([]string{r.Cfg.Git}, gitprobe.Args(dir, args...)...), Env: gitprobe.ProbeEnv(r.baseEnv()), Stdout: &out, Stderr: io.Discard})
	return strings.TrimSpace(out.String()), rc, err
}

func jsonLine(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}
