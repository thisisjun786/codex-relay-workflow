package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"golang.org/x/sys/unix"
)

// ReceiptCLIArgs is receipt-cli.ts ReceiptCliArgs, including the declared generated paths.
type ReceiptCLIArgs struct {
	Verb      string   `json:"verb"`
	Cwd       string   `json:"cwd"`
	Session   string   `json:"session,omitempty"`
	Command   []string `json:"command"`
	Generated []string `json:"generated,omitempty"`
}

// ReceiptCLIParseError is a parser refusal, distinct from a runner's thrown filesystem error.
type ReceiptCLIParseError struct{ Message string }

func (e ReceiptCLIParseError) Error() string { return e.Message }

// ReceiptCLIResult is runReceiptCli's output and observed exit code; it does not include child stdio.
type ReceiptCLIResult struct {
	Output string
	Code   int
}

// ReceiptRunOptions supplies the caller's streams and cancellation. Nil fields inherit process stdio and an uncancelled
// context. Cancellation kills only the process this call started, then follows the oracle's after-capture/result ordering.
// Caller-owned reader/writer callbacks must make progress; closing a pipe cannot interrupt a blocked callback.
type ReceiptRunOptions struct {
	Context        context.Context
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// ParseReceiptCLIArgs ports receipt-cli.ts:37-65. Only tokens after -- are commands. Missing session/cwd values and empty
// command arguments retain the oracle's quirks; --generated normalizes separators but does not validate containment.
func ParseReceiptCLIArgs(argv []string, cwd string) (ReceiptCLIArgs, error) {
	verb := ""
	if len(argv) > 0 {
		verb = argv[0]
	}
	out := ReceiptCLIArgs{Verb: strings.ToLower(verb), Cwd: cwd, Command: []string{}}
	switch out.Verb {
	case "help", "--help", "-h":
		out.Verb = "help"
		return out, nil
	case "test":
	default:
		return ReceiptCLIArgs{}, ReceiptCLIParseError{fmt.Sprintf("unknown receipt verb '%s' (expected test); run crw pabcd receipt --help", verb)}
	}
	i := 1
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			i++
			break
		}
		value := func() string {
			i++
			if i < len(argv) {
				return argv[i]
			}
			return ""
		}
		switch a {
		case "--session":
			out.Session = value()
		case "--cwd":
			out.Cwd = cwd
			if v := value(); i < len(argv) {
				out.Cwd = v
			}
		case "--generated":
			v := value()
			if v == "" {
				return ReceiptCLIArgs{}, ReceiptCLIParseError{"receipt test: --generated needs a repo-relative path"}
			}
			out.Generated = append(out.Generated, strings.TrimPrefix(strings.ReplaceAll(v, `\`, "/"), "./"))
		default:
			return ReceiptCLIArgs{}, ReceiptCLIParseError{fmt.Sprintf("unexpected argument '%s' before --", a)}
		}
	}
	for ; i < len(argv); i++ {
		if argv[i] != "" {
			out.Command = append(out.Command, argv[i])
		}
	}
	return out, nil
}

// ReceiptPathFor is the fixed receipt name per sanitized session (receipt-cli.ts:69-71).
func ReceiptPathFor(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, evidence.Subdir, state.SanitizeKey(sessionID), "test-receipt.json")
}

const receiptHelp = `crw pabcd receipt — record a check receipt that binds a command's result to a source tree

Usage:
  crw pabcd receipt test --session <id> [--cwd <path>] [--generated <path>]... -- <command> [args...]
  crw pabcd receipt --help

Notes:
  Everything after ` + "`--`" + ` is the command; nothing before it is.
  The session must be at phase C — a receipt is produced during Check.
  The receipt is written to <cwd>/.crw/evidence/<session>/test-receipt.json
  and is refused if the command changes the source while it runs.
  --generated declares paths the check REGENERATES by design (a validator that
  rebuilds its own artifacts). Repeatable. Undeclared rewrites are still refused.

Example:
  crw pabcd receipt test --session <id> -- npm test`

// receiptInterrupted is the refusal a cancelled receipt test answers with, from the late-cancel
// check or from the publication windows.
const receiptInterrupted = "receipt test: the command did not run to completion (interrupted); no receipt written"

// receiptBeforePublishHook, when non-nil, runs after the late-cancellation check and immediately
// before the receipt is published. It is nil in production; receipt_publish_cancel_test.go sets it
// to cancel the context in the window between that check and the rename, where PublishContext
// reports the cancellation at its rename step.
var receiptBeforePublishHook func()

// receiptAfterPublishHook, when non-nil, runs immediately after the receipt has been published and
// before the post-publication cancellation check. It is nil in production;
// receipt_publish_cancel_test.go sets it to cancel the context in the window the check closes,
// where the rename beat the cancellation.
var receiptAfterPublishHook func()

// receiptLockRetry is how often a run whose context can end asks again for a receipt lock file that another run holds.
const receiptLockRetry = 20 * time.Millisecond

// receiptLateCancelHook, when non-nil, runs immediately before the late-cancellation check in a
// receipt test. It is nil in production (an uninitialized variable, no package-level work at
// start); receipt_late_cancel_test.go sets it to cancel the context at the one point where the
// window the check closes is deterministic, after the command has returned and the source has been
// captured again.
var receiptLateCancelHook func()

// receiptLockAfterCompareHook, when non-nil, runs inside a withdrawal after it has compared the receipt on
// disk with its own bytes and found them equal, and before it unlinks them. It is nil in production;
// receipt_withdraw_race_test.go uses it to let a second run publish at the one point where a withdrawal
// that does not exclude other runs would remove that run's receipt.
var receiptLockAfterCompareHook func()

// receiptLockWaitParkedHook, when non-nil, runs once by the lock wait after its first refused attempt, when
// the run is parked on a lock another holder has. It is nil in production (an uninitialized variable, no
// package-level work at start); receipt_withdraw_race_test.go uses it to end the context only once the run
// is really waiting, instead of from a timer that can fire before the wait begins.
var receiptLockWaitParkedHook func()

// receiptLockRefusedHook, when non-nil, runs after the lock file could not be opened and the run judged the directory not
// writable. A test changes the evidence directory's mode there, the moment a lockless publication would become possible.
var receiptLockRefusedHook func()

// RunReceiptCLI ports receipt-cli.ts:75-185: guard, unlink stale receipt, capture, execute argv without a shell, capture again
// and publish only a successful unchanged-tree result. The receipt stays native while a bound command runs in its source.
// A cancellation seen anywhere before the rename refuses the receipt, and one that lands after the publication check
// withdraws the receipt the rename beat it to.
// The publication, the cancellation check after it and that withdrawal run under one exclusive flock on the receipt's lock file
// (receiptLockFile), so a withdrawal never removes a receipt another run published after the withdrawal compared the bytes.
// The lock needs no read permission on the directory. A wait for it that ends with the context publishes nothing and
// answers the interrupted refusal. The lock is cooperative: the run-start removal below, which is the oracle's, and any other
// writer of the file do not take it.
// A non-nil error models the oracle's thrown remove/before-capture/publication errors. Atomic publication intentionally fixes
// the oracle's record-truncation defect; ordinary parser and runner refusals remain result values.
func RunReceiptCLI(args ReceiptCLIArgs, options ReceiptRunOptions) (ReceiptCLIResult, error) {
	refuse := func(message string) (ReceiptCLIResult, error) { return ReceiptCLIResult{Output: message, Code: 1}, nil }
	if args.Verb == "help" {
		return ReceiptCLIResult{Output: receiptHelp}, nil
	}
	sid := text.Trim(args.Session)
	if sid == "" {
		return refuse("receipt test: --session <id> is required")
	}
	if len(args.Command) == 0 {
		return refuse("receipt test: a command is required after `--`, e.g. `crw pabcd receipt test --session <id> -- npm test`")
	}
	st := state.ReadState(args.Cwd, sid)
	if st.Phase != state.PhaseC {
		return refuse(fmt.Sprintf("receipt test: session is at %s, not C — a check receipt is produced during Check", st.Phase))
	}
	if st.CheckEpoch == nil || *st.CheckEpoch == "" {
		return refuse("receipt test: no check binding on this session. Step back with `crw pabcd orchestrate B` and re-enter `crw pabcd orchestrate C` to mint one (this cycle predates CHECK-BINDING-01).")
	}
	path := ReceiptPathFor(args.Cwd, sid)
	if err := removeReceipt(path); err != nil {
		return ReceiptCLIResult{}, err
	}
	root, err := session.Resolve(args.Cwd, sid)
	if err != nil {
		return refuse("receipt test: SOURCE-ROOT: " + err.Error())
	}
	exclude := true
	capture := session.CaptureOptions{ExcludeStateArtifacts: &exclude, GeneratedPaths: args.Generated}
	before, err := session.Capture(args.Cwd, sid, capture)
	if err != nil {
		return ReceiptCLIResult{}, err
	}
	runErr := runReceiptCommand(args.Command, root, options)
	after, err := session.Capture(args.Cwd, sid, capture)
	if err != nil {
		return refuse("receipt test: SOURCE-ROOT: " + err.Error() + "; no receipt written")
	}
	if runErr != nil {
		var exited *exec.ExitError
		if errors.As(runErr, &exited) && exited.ProcessState.ExitCode() >= 0 {
			code := exited.ProcessState.ExitCode()
			return ReceiptCLIResult{Output: fmt.Sprintf("receipt test: the command exited %d; no receipt written", code), Code: code}, nil
		}
		reason := "terminated by signal"
		if !errors.As(runErr, &exited) {
			reason = receiptSpawnError(args.Command[0], runErr)
		}
		return refuse("receipt test: the command did not run to completion (" + reason + "); no receipt written")
	}
	cmp := source.Compare(before, after)
	switch cmp.Kind {
	case source.ComparisonDifferent:
		return refuse("receipt test: the command changed the source while running (" + cmp.Detail + "); no receipt written — a check cannot certify a tree it rewrote.\nIf the check REGENERATES artifacts by design, declare them:\n  crw pabcd receipt test --session <id> --generated <path> -- <command>\n(repeatable; a path covers that file or that directory. Undeclared rewrites are still refused.)")
	case source.ComparisonUnavailable:
		return refuse("receipt test: git could not resolve the source identity (" + cmp.Reason + "); no receipt written")
	}
	// The last moment the publication can still be skipped: a cancellation that landed after the
	// command returned refuses the receipt here, as the oracle's deferred signal refuses it.
	if receiptLateCancelHook != nil {
		receiptLateCancelHook()
	}
	if options.Context != nil && options.Context.Err() != nil {
		return refuse(receiptInterrupted)
	}
	record := receiptRecord{Kind: "test", SourceIdentity: after, Command: strings.Join(args.Command, " "), ExitCode: 0, CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), OwnerSessionID: sid, CheckEpoch: *st.CheckEpoch, GeneratedPaths: args.Generated}
	if _, err = crwdir.EnsureDir(args.Cwd); err != nil {
		return ReceiptCLIResult{}, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return ReceiptCLIResult{}, err
	}
	data, err := encodeReceipt(record)
	if err != nil {
		return ReceiptCLIResult{}, err
	}
	if receiptBeforePublishHook != nil {
		receiptBeforePublishHook()
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	lock, err := receiptLockFile(path)
	if err != nil {
		// A directory without write permission takes no temporary file either, so the publication below refuses with the
		// error it always gave. Any other failure, or a lock refused in a directory that can be written, is returned.
		if !errors.Is(err, fs.ErrPermission) || unix.Access(filepath.Dir(path), unix.W_OK) == nil {
			return ReceiptCLIResult{}, err
		}
		if receiptLockRefusedHook != nil {
			receiptLockRefusedHook()
		}
	} else {
		defer lock.Close() // drops the lock
		if err = receiptLockWait(ctx, lock); err != nil {
			if ctx.Err() != nil && err == ctx.Err() {
				return refuse(receiptInterrupted)
			}
			return ReceiptCLIResult{}, err
		}
	}
	if err = crwdir.PublishContext(ctx, path, data); err != nil {
		if result, refused := receiptPublishRefusal(ctx, err); refused {
			return result, nil
		}
		return ReceiptCLIResult{}, err
	}
	if receiptAfterPublishHook != nil {
		receiptAfterPublishHook()
	}
	// The rename beat a cancellation that landed after the check above: withdraw the receipt just
	// published, as the oracle's dead process leaves nothing behind either. Only the bytes written
	// here are removed: a receipt another invocation published in the meantime is left alone.
	if ctx.Err() != nil {
		if err := withdrawReceipt(path, data); err != nil {
			return ReceiptCLIResult{}, err
		}
		return refuse(receiptInterrupted)
	}
	return ReceiptCLIResult{Output: path}, nil
}

// receiptPublishRefusal maps the error of the receipt's publish onto the caller's result: the
// interrupted refusal when the cancellation was clean - err is the context's own error, which
// PublishContext returns after the temp file it had written was removed - and refused=false when the
// error must be reported as it happened. That includes a cancellation joined with a failed removal,
// which publish joins, so a stranded temporary file is never hidden behind "no receipt written".
func receiptPublishRefusal(ctx context.Context, err error) (ReceiptCLIResult, bool) {
	if err == nil || ctx.Err() == nil || err != ctx.Err() {
		return ReceiptCLIResult{}, false
	}
	return ReceiptCLIResult{Output: receiptInterrupted, Code: 1}, true
}

// receiptLockFile opens the lock file of the receipt at path, creating it when it is missing. The lock file is path + ".lock",
// the sidecar-lock name of the state files (state.WithSessionLock); its name ends in .lock, so the listings that take only
// .json names skip it. It needs the write and search permission on the directory that a publication needs, and no read
// permission on the directory itself. The file is left in place, so no two runs can lock different inodes of one name.
func receiptLockFile(path string) (*os.File, error) {
	return os.OpenFile(path+".lock", os.O_RDONLY|os.O_CREATE, 0o666)
}

// receiptLockWait takes the exclusive lock on the receipt's lock file. A context that can end is asked for it without blocking,
// and again every receiptLockRetry while another run holds it, so the wait ends with ctx and returns its error. One that can
// never end (context.Background) blocks in the kernel. A free lock is taken even when ctx has ended since the caller looked:
// PublishContext then reports the cancellation at its rename step, as it did before the lock existed.
func receiptLockWait(ctx context.Context, lock *os.File) error {
	if ctx.Done() == nil {
		return unix.Flock(int(lock.Fd()), unix.LOCK_EX)
	}
	tick := time.NewTicker(receiptLockRetry)
	defer tick.Stop()
	refused := false
	for {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		if !refused {
			refused = true
			if receiptLockWaitParkedHook != nil {
				receiptLockWaitParkedHook()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// withdrawReceipt removes the receipt this invocation published at path, while the bytes on disk are
// still the ones it wrote; a path another invocation for the same session replaced stays. An already
// absent path is fine, and any other failure is reported as it happened.
func withdrawReceipt(path string, published []byte) error {
	current, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, published) {
		return nil
	}
	if receiptLockAfterCompareHook != nil {
		receiptLockAfterCompareHook()
	}
	return removeReceipt(path)
}

// removeReceipt unlinks a receipt; its absence is fine, as rmSync with force treats it. The removal
// never falls back to rmdir: rmSync without recursive refuses an empty directory too.
func removeReceipt(path string) error {
	if err := syscall.Unlink(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}

func runReceiptCommand(argv []string, cwd string, o ReceiptRunOptions) error {
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	file, err := receiptExecutable(argv[0], cwd)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, file, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Dir, cmd.Env = cwd, source.GitEnv(nil)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, o.Stdout, o.Stderr
	// Own non-file output adapters rather than let Cmd wait indefinitely for a descendant's inherited descriptors.
	// Normal completion drains through EOF without a timeout; only actual cancellation closes these readers.
	var pipes []receiptOutputPipe
	for _, slot := range []*io.Writer{&cmd.Stdout, &cmd.Stderr} {
		if _, file := (*slot).(*os.File); file {
			continue
		}
		shared := false
		for _, p := range pipes {
			if reflect.ValueOf(p.destination).Comparable() && p.destination == *slot {
				*slot = p.child
				shared = true
				break
			}
		}
		if shared {
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		defer r.Close()
		defer w.Close()
		pipes = append(pipes, receiptOutputPipe{r, w, *slot})
		*slot = w
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	drained := make(chan error, len(pipes))
	for _, p := range pipes {
		_ = p.child.Close()
		go func() {
			defer p.reader.Close() // a failed destination must not leave the child blocked on a full pipe
			_, err := io.Copy(p.destination, p.reader)
			drained <- err
		}()
	}
	stop := make(chan struct{})
	defer close(stop)
	if len(pipes) > 0 && o.Context != nil {
		go func() {
			select {
			case <-ctx.Done():
				for _, p := range pipes {
					_ = p.reader.Close()
				}
			case <-stop:
			}
		}()
	}
	err = cmd.Wait()
	for range pipes {
		copyErr := <-drained
		if err == nil && copyErr != nil {
			err = copyErr
		}
	}
	return err
}

type receiptOutputPipe struct {
	reader, child *os.File
	destination   io.Writer
}

// Node's POSIX PATH search resolves relative entries in the command cwd, whereas exec.LookPath uses the caller cwd and
// rejects relative results with ErrDot. Resolve before constructing Cmd so its own lookup cannot silently change that.
func receiptExecutable(name, cwd string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	permission := false
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(absolute, dir)
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			if err = syscall.Access(candidate, 1); err == nil {
				return candidate, nil
			}
		}
		if errors.Is(err, fs.ErrPermission) || err == nil {
			permission = true
		}
	}
	if permission {
		return "", fs.ErrPermission
	}
	return "", exec.ErrNotFound
}

func receiptSpawnError(name string, err error) string {
	code := ""
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		code = "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		code = "EACCES"
	case errors.Is(err, syscall.ENOTDIR):
		code = "ENOTDIR"
	case errors.Is(err, syscall.ENOEXEC):
		code = "ENOEXEC"
	}
	if code != "" {
		return "spawnSync " + name + " " + code
	}
	return err.Error()
}

// Field order and optional generatedPaths are JSON.stringify(receipt,null,2)'s record shape (receipt-cli.ts:171-184).
type receiptRecord struct {
	Kind           string          `json:"kind"`
	SourceIdentity source.Identity `json:"sourceIdentity"`
	Command        string          `json:"command"`
	ExitCode       int             `json:"exitCode"`
	CreatedAt      string          `json:"createdAt"`
	OwnerSessionID string          `json:"ownerSessionId"`
	CheckEpoch     string          `json:"checkEpoch"`
	GeneratedPaths []string        `json:"generatedPaths,omitempty"`
}

func encodeReceipt(record receiptRecord) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(record); err != nil {
		return nil, err
	}
	in, out := b.Bytes(), make([]byte, 0, b.Len())
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return out, nil
}
