package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runtime-upgrade replaces the relay runtime from a release archive whose commit is green on the
// dev gate, after proving no relay attempt is still open. Every step's command, exit status and
// output head is left in W/record.json, together with every reason that applied to the run.

// upgradeRecordHead is how many bytes of a command's output the record keeps.
const upgradeRecordHead = 2000

const (
	upgradeExitUnexpected   = 1
	upgradeExitRefused      = 2
	upgradeExitOpenAttempts = 3
	upgradeExitPostCheck    = 4
)

// upgradeExitUpdateFailed is what a failed install reports: the installer's own codes overlap
// this command's 2 and 3, so exit 1 keeps the two contracts apart.
const upgradeExitUpdateFailed = upgradeExitUnexpected

const (
	upgradeReasonRepository    = "repository_unconfigured"
	upgradeReasonSumsFailed    = "sums_failed"
	upgradeReasonExtractFailed = "extract_failed"
	upgradeReasonCommitUnknown = "commit_unknown"
	upgradeReasonDevGate       = "dev_gate_not_green"
	upgradeReasonStoreRead     = "store_unreadable"
	upgradeReasonOpenAttempts  = "open_attempts"
	upgradeReasonPointer       = "pointer_missing"
	upgradeReasonStopFailed    = "service_stop_failed"
	upgradeReasonPostCheck     = "postcheck_failed"
	upgradeReasonUpdateFailed  = "update_failed"
	// The post-check's own findings are named apart from upgradeReasonPostCheck, so the record
	// says what was found rather than only that the post-check refused.
	upgradeReasonConfigChanged   = "config_changed"
	upgradeReasonRuntimeMismatch = "runtime_mismatch"
)

const (
	upgradeStepSums      = "sums"
	upgradeStepExtract   = "extract"
	upgradeStepCommit    = "commit"
	upgradeStepDevGate   = "dev-gate"
	upgradeStepAttempts  = "open-attempts"
	upgradeStepSnapshot  = "snapshot"
	upgradeStepStop      = "service-stop"
	upgradeStepUpdate    = "install-update"
	upgradeStepStart     = "service-start"
	upgradeStepPostCheck = "post-check"
)

const upgradeUsage = "usage: crw manage runtime-upgrade --release-dir DIR [--issue KEY] [--dry-run]"

type upgradeOptions struct {
	ReleaseDir string
	Issue      string
	DryRun     bool
}

// upgradeConfig is the configuration a run reads: a variable, so a test can supply a repository
// and a relay state before the configuration file's loader lands. The defaults name neither.
var upgradeConfig = func(e *Env) *Config { return coreDefaults(e) }

type upgradeStepRecord struct {
	Step    string   `json:"step"`
	Command []string `json:"command,omitempty"`
	Exit    int      `json:"exit"`
	Output  string   `json:"output,omitempty"`
}

type upgradeRecord struct {
	ReleaseDir string `json:"release_dir"`
	Issue      string `json:"issue,omitempty"`
	DryRun     bool   `json:"dry_run"`
	StartedAt  string `json:"started_at"`
	Directory  string `json:"directory"`
	ExtractDir string `json:"extract_dir,omitempty"`
	// StartFrom is the runtime directory the restart was called from, so the record says which
	// executable the service came back on.
	StartFrom string              `json:"start_from,omitempty"`
	Outcome   string              `json:"outcome"`
	Reason    string              `json:"reason,omitempty"`
	Reasons   []string            `json:"reasons,omitempty"`
	Steps     []upgradeStepRecord `json:"steps"`
}

var upgradeCommand = Command{
	Name:    "runtime-upgrade",
	Summary: "replace the relay runtime from a verified release archive",
	Run:     upgradeRun,
}

func init() { Register(upgradeCommand) }

// upgradeRun is crw manage runtime-upgrade. The repository is a precondition: without it the
// first gh call could not be made.
func upgradeRun(ctx context.Context, e *Env, args []string) int {
	opts, code, handled := upgradeParse(args)
	if handled {
		stream := e.Stdout
		if code != 0 {
			stream = e.Stderr
		}
		fmt.Fprintln(stream, upgradeUsage)
		return code
	}
	cfg := upgradeConfig(e)
	if cfg.Repository == "" {
		fmt.Fprintf(e.Stderr, "crw manage runtime-upgrade: error: %s\n", upgradeReasonRepository)
		return upgradeExitRefused
	}
	run := &upgradeRunState{ctx: ctx, e: e, cfg: cfg, opts: opts}
	return run.run()
}

// upgradeParse reads the command line; handled reports a help flag, a bad argument or no --release-dir.
func upgradeParse(args []string) (opts upgradeOptions, code int, handled bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return opts, 0, true
		case arg == "--dry-run":
			opts.DryRun = true
		case arg == "--release-dir" || arg == "--issue":
			if i+1 >= len(args) {
				return opts, usageExit, true
			}
			i++
			if arg == "--release-dir" {
				opts.ReleaseDir = args[i]
			} else {
				opts.Issue = args[i]
			}
		case strings.HasPrefix(arg, "--release-dir="):
			opts.ReleaseDir = strings.TrimPrefix(arg, "--release-dir=")
		case strings.HasPrefix(arg, "--issue="):
			opts.Issue = strings.TrimPrefix(arg, "--issue=")
		default:
			return opts, usageExit, true
		}
	}
	if opts.ReleaseDir == "" {
		return opts, usageExit, true
	}
	return opts, 0, false
}

type upgradeRunState struct {
	ctx  context.Context
	e    *Env
	cfg  *Config
	opts upgradeOptions

	dir     string // W
	extract string // W/extract
	archive string
	commit  string
	state   string
	started time.Time

	// digest is the verified archive's SHA-256, version the cleaned version the unpacked crw
	// printed, installed the runtime directory the update reported it produced, previous the
	// runtime the pointer named before the stop, and startFrom the runtime the restart used.
	digest    string
	version   string
	installed string
	previous  string
	startFrom string
	// updated reports whether the update ran and ended successfully, so the post-check knows
	// whether an install target is something it has to compare the pointer with.
	updated bool

	beforeConfig string
	reasons      []string
	steps        []upgradeStepRecord
}

func (r *upgradeRunState) run() int {
	r.started = r.e.Now().UTC()
	r.dir = filepath.Join(r.cfg.StateDir, "upgrades", r.started.Format("20060102T150405Z"))
	if err := upgradeRunDir(r.dir); err != nil {
		fmt.Fprintf(r.e.Stderr, "crw manage runtime-upgrade: error: %v\n", err)
		return upgradeExitRefused
	}
	code, reason := r.execute()
	if err := r.write(reason); err != nil {
		fmt.Fprintf(r.e.Stderr, "crw manage runtime-upgrade: error: %v\n", err)
		return upgradeExitUnexpected
	}
	if code != 0 {
		fmt.Fprintf(r.e.Stderr, "crw manage runtime-upgrade: error: %s\n", reason)
	}
	return code
}

// execute performs the steps in order. Each stop returns before the step after it; the steps
// after the update always run, because the service has to come back either way.
func (r *upgradeRunState) execute() (int, string) {
	archive, code, reason := r.verifySums()
	if code != 0 {
		return code, reason
	}
	r.archive = archive

	if code, reason = r.extractAndResolve(); code != 0 {
		return code, reason
	}
	if code, reason = r.checkDevGate(); code != 0 {
		return code, reason
	}
	if code, reason = r.checkOpenAttempts(); code != 0 {
		return code, reason
	}
	if r.opts.DryRun {
		return 0, ""
	}
	if code, reason = r.snapshot(); code != 0 {
		return code, reason
	}

	updateCode, updateReason := r.stopAndUpdate()
	r.start()

	post := r.postCheck()
	return r.outcome(updateCode, updateReason, post)
}

// outcome is the run's final status and reason, in the decided order: a configuration change the
// post-check found outranks a failed update, which in turn outranks the post-check's other
// findings. Every reason that applied is kept in the record, so a run that both failed to update
// and changed the configuration names both.
func (r *upgradeRunState) outcome(updateCode int, updateReason string, post upgradePostCheck) (int, string) {
	if updateCode != 0 {
		r.reasons = append(r.reasons, updateReason)
	}
	r.reasons = append(r.reasons, post.reasons...)
	switch {
	case post.configChanged:
		return upgradeExitPostCheck, upgradeReasonConfigChanged
	case updateCode != 0:
		return updateCode, updateReason
	case post.code != 0:
		return post.code, post.reason
	}
	return 0, ""
}

func (r *upgradeRunState) write(reason string) error {
	record := upgradeRecord{
		ReleaseDir: r.opts.ReleaseDir,
		Issue:      r.opts.Issue,
		DryRun:     r.opts.DryRun,
		StartedAt:  r.started.Format(time.RFC3339),
		Directory:  r.dir,
		ExtractDir: r.extract,
		StartFrom:  r.startFrom,
		Outcome:    "ok",
		Reasons:    r.reasons,
		Steps:      r.steps,
	}
	if reason != "" {
		record.Outcome = "failed"
		record.Reason = reason
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, "record.json"), append(data, '\n'), 0o600)
}

func (r *upgradeRunState) note(step string, argv []string, code int, out string, err error) {
	text := out
	if err != nil {
		text = err.Error()
	}
	r.steps = append(r.steps, upgradeStepRecord{Step: step, Command: argv, Exit: code, Output: upgradeOutputHead(text)})
}

// command runs one command under its own timeout and records it.
func (r *upgradeRunState) command(ctx context.Context, timeout time.Duration, step, exe string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, stderr, code, err := upgradeRunCommand(ctx, exe, args...)
	r.note(step, append([]string{exe}, args...), code, out+stderr, err)
	return out, code, err
}

// refuse records a failed step and returns the status and reason that report it.
func (r *upgradeRunState) refuse(step, reason, out string, err error) (int, string) {
	r.note(step, nil, 1, out, err)
	return upgradeExitRefused, reason
}
