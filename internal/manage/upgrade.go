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

// runtime-upgrade replaces the relay runtime from a release archive whose commit is green on
// the dev gate, after proving that no relay attempt is still open. It is the management tool
// runtime_upgrade.sh as a product surface: the host values come from the configuration, and
// every step's command, exit status and output head is left behind in W/record.json.

// upgradeRecordHead is how many bytes of a command's output the record keeps.
const upgradeRecordHead = 2000

// The exit statuses the command reports.
const (
	upgradeExitUnexpected   = 1
	upgradeExitRefused      = 2
	upgradeExitOpenAttempts = 3
	upgradeExitPostCheck    = 4
)

// The refusal names, written to the record and to stderr.
const (
	upgradeReasonRepository    = "repository_unconfigured"
	upgradeReasonSumsFailed    = "sums_failed"
	upgradeReasonExtractFailed = "extract_failed"
	upgradeReasonCommitUnknown = "commit_unknown"
	upgradeReasonDevGate       = "dev_gate_not_green"
	upgradeReasonStoreRead     = "store_unreadable"
	upgradeReasonOpenAttempts  = "open_attempts"
	upgradeReasonPointer       = "pointer_missing"
	upgradeReasonPostCheck     = "postcheck_failed"
	upgradeReasonUpdateFailed  = "update_failed"
)

// The step names the record uses.
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

// upgradeUsage is the one line the command prints.
const upgradeUsage = "usage: crw manage runtime-upgrade --release-dir DIR [--issue KEY] [--dry-run]"

// upgradeOptions is a parsed command line.
type upgradeOptions struct {
	ReleaseDir string
	Issue      string
	DryRun     bool
}

// upgradeConfig is the configuration a run reads. It is a variable so a test can supply a
// repository and a relay state before the configuration file's loader lands; the command
// otherwise reads the package's defaults, which name no repository and no relay state.
var upgradeConfig = func(e *Env) *Config { return coreDefaults(e) }

// upgradeStepRecord is one step as record.json keeps it.
type upgradeStepRecord struct {
	Step    string   `json:"step"`
	Command []string `json:"command,omitempty"`
	Exit    int      `json:"exit"`
	Output  string   `json:"output,omitempty"`
}

// upgradeRecord is W/record.json: what the run did, step by step.
type upgradeRecord struct {
	ReleaseDir string              `json:"release_dir"`
	Issue      string              `json:"issue,omitempty"`
	DryRun     bool                `json:"dry_run"`
	StartedAt  string              `json:"started_at"`
	Directory  string              `json:"directory"`
	ExtractDir string              `json:"extract_dir,omitempty"`
	Outcome    string              `json:"outcome"`
	Reason     string              `json:"reason,omitempty"`
	Steps      []upgradeStepRecord `json:"steps"`
}

// upgradeCommand is crw manage runtime-upgrade.
var upgradeCommand = Command{
	Name:    "runtime-upgrade",
	Summary: "replace the relay runtime from a verified release archive",
	Run:     upgradeRun,
}

func init() { Register(upgradeCommand) }

// upgradeRun is crw manage runtime-upgrade. The repository is a precondition: without it the
// first gh call could not be made, so the command refuses before any step runs.
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

// upgradeParse reads the command line. handled reports that it is finished with: a help flag,
// an unknown argument, or a missing --release-dir.
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

// upgradeRunState is one run: what it reads, where it records, and the steps it has taken.
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

	beforeConfig string

	steps []upgradeStepRecord
}

// run creates the record directory, performs the steps, and leaves record.json behind.
func (r *upgradeRunState) run() int {
	r.dir = filepath.Join(r.cfg.StateDir, "upgrades", r.e.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		fmt.Fprintf(r.e.Stderr, "crw manage runtime-upgrade: error: %v\n", err)
		return upgradeExitUnexpected
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

// execute performs the nine steps in order. Each stop returns before the step after it.
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

	updateCode := r.stopAndUpdate()
	r.start()

	if code, reason = r.postCheck(); code != 0 {
		return code, reason
	}
	if updateCode != 0 {
		return updateCode, upgradeReasonUpdateFailed
	}
	return 0, ""
}

// write leaves W/record.json.
func (r *upgradeRunState) write(reason string) error {
	record := upgradeRecord{
		ReleaseDir: r.opts.ReleaseDir,
		Issue:      r.opts.Issue,
		DryRun:     r.opts.DryRun,
		StartedAt:  r.e.Now().UTC().Format(time.RFC3339),
		Directory:  r.dir,
		ExtractDir: r.extract,
		Outcome:    "ok",
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

// note appends one step to the record.
func (r *upgradeRunState) note(step string, argv []string, code int, out string, err error) {
	text := out
	if err != nil {
		text = err.Error()
	}
	r.steps = append(r.steps, upgradeStepRecord{Step: step, Command: argv, Exit: code, Output: upgradeOutputHead(text)})
}

// command runs a command, records it under step, and returns its stdout and exit status.
func (r *upgradeRunState) command(step, exe string, args ...string) (string, int, error) {
	out, code, err := upgradeRunCommand(r.ctx, exe, args...)
	r.note(step, append([]string{exe}, args...), code, out, err)
	return out, code, err
}
