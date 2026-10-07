package manage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	upgradeSumsName    = "SHA256SUMS"
	upgradeArchiveGlob = "crw_*_linux_amd64.tar.gz"
	upgradeRuntimeDir  = ".local/share/crw-runtime"
	// upgradeCommandTimeout bounds one forge call; upgradeInstallTimeout one lifecycle command,
	// which exercises the bridge and may copy a large relay state.
	upgradeCommandTimeout = 60 * time.Second
	upgradeInstallTimeout = 30 * time.Minute
	// upgradeServiceInterval and upgradeServiceBudget are how often, and for how long, the
	// post-check reads the service status again after starting it.
	upgradeServiceInterval = 2 * time.Second
	upgradeServiceBudget   = 60 * time.Second
	// upgradeInstallOK and upgradeInstallIncomplete are the installer's own statuses for an update
	// that promoted the runtime: OK, and Incomplete (the candidate is selected and the owned
	// pointer names it, and only the claim that records it did not settle). Neither is this
	// command's status space: an update that ends Incomplete is reported as failed here, but the
	// runtime is in service, so the post-check still compares the pointer with it.
	upgradeInstallOK         = 0
	upgradeInstallIncomplete = 3
)

// upgradeRunCommand runs exe and returns its stdout, stderr and exit status.
func upgradeRunCommand(ctx context.Context, exe string, args ...string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), stderr.String(), exit.ExitCode(), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), stderr.String(), 0, ctxErr
		}
		return stdout.String(), stderr.String(), 0, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// upgradeRunDir creates the run directory exclusively, so two runs in one second never share it.
func upgradeRunDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("a run already took %s; try again a second later", dir)
		}
		return err
	}
	return nil
}

func upgradeOutputHead(text string) string {
	if len(text) <= upgradeRecordHead {
		return text
	}
	return text[:upgradeRecordHead]
}

// verifySums is step 1: copy the release archive and its SHA256SUMS into the run directory, then
// verify the copy against the copied sums. The originals in the release directory are opened once,
// by the copy, and never again: the bytes that were checked are the bytes every later step reads,
// so replacing an original after the check cannot change what is unpacked and run.
func (r *upgradeRunState) verifySums() (string, int, string) {
	matches, err := filepath.Glob(filepath.Join(r.opts.ReleaseDir, upgradeArchiveGlob))
	if err != nil || len(matches) == 0 {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("no %s in %s", upgradeArchiveGlob, r.opts.ReleaseDir))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	name := filepath.Base(matches[0])
	pinned := filepath.Join(r.dir, name)
	if err := upgradeCopy(matches[0], pinned); err != nil {
		r.note(upgradeStepSums, nil, 1, "", err)
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	if err := upgradeCopy(filepath.Join(r.opts.ReleaseDir, upgradeSumsName), filepath.Join(r.dir, upgradeSumsName)); err != nil {
		r.note(upgradeStepSums, nil, 1, "", err)
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	sums, err := os.ReadFile(filepath.Join(r.dir, upgradeSumsName))
	if err != nil {
		r.note(upgradeStepSums, nil, 1, "", err)
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	want := upgradeSumsFor(string(sums), name)
	if want == "" {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("%s names no digest for %s", upgradeSumsName, name))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	got, err := upgradeFileDigest(pinned)
	if err != nil {
		r.note(upgradeStepSums, nil, 1, "", err)
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	if !strings.EqualFold(got, want) {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("%s does not match %s", name, upgradeSumsName))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	r.note(upgradeStepSums, nil, 0, want+"  "+name, nil)
	if upgradeSumsVerified != nil {
		upgradeSumsVerified(r.opts.ReleaseDir)
	}
	return pinned, 0, ""
}

// upgradeSumsVerified runs at the one moment the release directory's originals could still change
// what runs: after the copied archive's digest has been compared with the copied SHA256SUMS, and
// before anything is unpacked. It is nil everywhere but a test, which uses it to replace the
// release directory's archive there. The run never opens that archive again, so what replaced it
// cannot be what runs; the baseline hashed the original and then copied it, and the same
// replacement between those two reads changed the bytes that were executed.
var upgradeSumsVerified func(releaseDir string)

// upgradeCopy copies src to dst, so a later reader cannot be given different bytes.
func upgradeCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// upgradeSumsFor is the digest SHA256SUMS lists for name, or "" when it lists none.
func upgradeSumsFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0]
		}
	}
	return ""
}

func upgradeFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractAndResolve is step 2: unpack the verified copy and resolve the version to a full commit
// SHA. The unpacked crw's version is kept, because the post-check compares the installed runtime's
// version with the one this archive carried.
func (r *upgradeRunState) extractAndResolve() (int, string) {
	r.extract = filepath.Join(r.dir, "extract")
	if err := os.MkdirAll(r.extract, 0o700); err != nil {
		return r.refuse(upgradeStepExtract, upgradeReasonExtractFailed, "", err)
	}
	if err := upgradeExtract(r.archive, r.extract); err != nil {
		return r.refuse(upgradeStepExtract, upgradeReasonExtractFailed, "", err)
	}
	crw := filepath.Join(r.extract, "crw")
	version, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepExtract, crw, "--version")
	if err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonExtractFailed
	}
	r.version = strings.TrimSpace(version)
	for _, ref := range upgradeCommitRefs(version) {
		out, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepCommit, "gh", "api", "repos/"+r.cfg.Repository+"/commits/"+ref)
		if err != nil || code != 0 {
			continue
		}
		var answer struct {
			SHA string `json:"sha"`
		}
		if json.Unmarshal([]byte(out), &answer) == nil && answer.SHA != "" {
			r.commit = answer.SHA
			return 0, ""
		}
	}
	r.note(upgradeStepCommit, nil, 1, "", fmt.Errorf("the version %q did not resolve to a commit", strings.TrimSpace(version)))
	return upgradeExitRefused, upgradeReasonCommitUnknown
}

// upgradeCommitRefs is what to ask the forge about: the commit after -g, else the released tag.
func upgradeCommitRefs(version string) []string {
	trimmed := strings.TrimSpace(version)
	if _, rest, found := strings.Cut(trimmed, "-g"); found {
		if hash := strings.TrimSpace(rest); hash != "" && !strings.ContainsAny(hash, " \t\r\n") {
			return []string{hash}
		}
	}
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "v") {
		return []string{trimmed}
	}
	return []string{trimmed, "v" + trimmed}
}

// checkDevGate is step 3: the commit's dev-gate check run must have concluded success.
func (r *upgradeRunState) checkDevGate() (int, string) {
	out, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepDevGate, "gh", "api", "repos/"+r.cfg.Repository+"/commits/"+r.commit+"/check-runs")
	if err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonDevGate
	}
	var answer struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			App        struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return r.refuse(upgradeStepDevGate, upgradeReasonDevGate, out, err)
	}
	for _, run := range answer.CheckRuns {
		if run.Name == "dev-gate" && run.Conclusion == "success" && (run.App.Slug == "" || run.App.Slug == "github-actions") {
			return 0, ""
		}
	}
	return r.refuse(upgradeStepDevGate, upgradeReasonDevGate, out, fmt.Errorf("no dev-gate check run concluded success"))
}

// upgradeDoctorAnswer is the part of the relay doctor's answer this command reads: where the store
// is, and how many attempts the service that holds it still has open.
type upgradeDoctorAnswer struct {
	StateSelection struct {
		Path string `json:"path"`
	} `json:"stateSelection"`
	Contents struct {
		Available    bool   `json:"available"`
		OpenAttempts *int64 `json:"openAttempts"`
	} `json:"contents"`
}

// upgradeParseDoctorAnswer reads the doctor's answer; an answer that is not JSON is an error, so a
// reader never mistakes an unreadable store for an empty one.
func upgradeParseDoctorAnswer(out string) (upgradeDoctorAnswer, error) {
	var answer upgradeDoctorAnswer
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return answer, fmt.Errorf("the relay doctor answer: %w", err)
	}
	return answer, nil
}

// checkOpenAttempts is step 4: ask the running runtime's own relay for its doctor answer and refuse
// while any attempt is still open. The count is the doctor's contents.openAttempts alone; contents
// that are unavailable, or that carry no count, are refused rather than read as an absence, so a
// store nobody could read never looks like a store with nothing open.
func (r *upgradeRunState) checkOpenAttempts() (int, string) {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		// There is no running runtime to ask, which is the pointer's own refusal: the same reason the
		// snapshot reported for this host before this step moved ahead of it.
		return r.refuse(upgradeStepAttempts, upgradeReasonPointer, "", err)
	}
	args := []string{"--socket", r.cfg.Relay.Socket}
	if r.cfg.Relay.State != "" {
		args = append([]string{"--state", r.cfg.Relay.State}, args...)
	}
	args = append(args, "doctor")
	out, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepAttempts,
		filepath.Join(pointer, "bin", "codex-session-relay"), args...)
	if err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonStoreRead
	}
	answer, err := upgradeParseDoctorAnswer(out)
	if err != nil {
		return r.refuse(upgradeStepAttempts, upgradeReasonStoreRead, out, err)
	}
	state := r.cfg.Relay.State
	if state == "" {
		state = answer.StateSelection.Path
	}
	if state == "" {
		return r.refuse(upgradeStepAttempts, upgradeReasonStoreRead, out, errors.New("the doctor named no state directory"))
	}
	r.state = state
	if !answer.Contents.Available || answer.Contents.OpenAttempts == nil {
		return r.refuse(upgradeStepAttempts, upgradeReasonStoreRead, out, fmt.Errorf("the doctor's contents carry no open attempt count"))
	}
	if *answer.Contents.OpenAttempts != 0 {
		return upgradeExitOpenAttempts, upgradeReasonOpenAttempts
	}
	return 0, ""
}

// snapshot is step 5: what the run must be able to compare afterwards.
func (r *upgradeRunState) snapshot() (int, string) {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		return r.refuse(upgradeStepSnapshot, upgradeReasonPointer, "", err)
	}
	if _, code, err := r.command(r.ctx, upgradeInstallTimeout, upgradeStepSnapshot, filepath.Join(pointer, "bin", "crw"), "install", "status"); err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonPointer
	}
	digest, err := upgradeFileDigest(upgradeConfigPath(r.e))
	if err != nil {
		return r.refuse(upgradeStepSnapshot, upgradeReasonPointer, "", err)
	}
	r.beforeConfig = digest
	r.note(upgradeStepSnapshot, nil, 0, pointer+"\n"+digest, nil)
	return 0, ""
}

// stopAndUpdate is step 6: stop with the running executable, then install with the extracted one.
// The runtime the pointer names is recorded before the stop, so the restart after it can fall back
// to the runtime the service was running. A stop that did not succeed stops the run before the
// runtime is replaced, and the two failures are named apart in the record.
func (r *upgradeRunState) stopAndUpdate() (int, string) {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		return r.refuse(upgradeStepStop, upgradeReasonStopFailed, "", err)
	}
	r.previous = pointer
	if _, code, err := r.command(r.ctx, upgradeInstallTimeout, upgradeStepStop,
		filepath.Join(pointer, "bin", "codex-session-relay"),
		"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "stop"); err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonStopFailed
	}
	args := []string{"install", "update", "--from", r.archive, "--state", r.state,
		"--socket", r.cfg.Relay.Socket, "--backup-state-to", filepath.Join(r.dir, "state-backup")}
	if r.opts.Issue != "" {
		args = append(args, "--issue", r.opts.Issue)
	}
	out, code, err := r.command(r.ctx, upgradeInstallTimeout, upgradeStepUpdate, filepath.Join(r.extract, "crw"), args...)
	if err != nil {
		return upgradeExitUpdateFailed, upgradeReasonUpdateFailed
	}
	if code == upgradeInstallOK || code == upgradeInstallIncomplete {
		// The installer's Incomplete is a promotion whose claim did not settle: the candidate is
		// selected and the owned pointer already names it, so the runtime is in service and the
		// post-check compares the pointer and the version with it, even though this command reports
		// the run as failed because the promotion did not finish.
		r.promoted = true
		r.installed = upgradeInstalledRuntime(out)
	}
	if code != 0 {
		return upgradeExitUpdateFailed, upgradeReasonUpdateFailed
	}
	return 0, ""
}

// upgradeInstalledRuntime is the runtime directory the update's own answer names, or "" when the
// answer names none. Nothing else can say where the update put the runtime, and the post-check
// refuses rather than comparing the pointer with a guess.
func upgradeInstalledRuntime(answer string) string {
	var reported struct {
		Environment string `json:"environment"`
	}
	if json.Unmarshal([]byte(answer), &reported) != nil {
		return ""
	}
	return reported.Environment
}

// start is step 7: start again, whatever the update did. The pointer's runtime is tried first when
// it still resolves; a pointer that does not resolve, or a start that does not succeed, falls back
// to the runtime the pointer named before the stop, which is what the service was running. A
// pointer left on an unusable runtime therefore cannot leave the relay down. Every attempt is
// recorded, and startFrom is the runtime the service came back on.
func (r *upgradeRunState) start() {
	var candidates []string
	if pointer, err := upgradePointerTarget(r.e); err == nil {
		candidates = append(candidates, pointer)
	} else if r.previous != "" {
		r.note(upgradeStepStart, nil, 1, "", err)
	} else {
		r.note(upgradeStepStart, nil, 1, "", err)
		return
	}
	if r.previous != "" && !slices.Contains(candidates, r.previous) {
		candidates = append(candidates, r.previous)
	}
	for i, runtime := range candidates {
		// The recovery step runs detached, so a cancellation after the stop cannot leave it down.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), upgradeInstallTimeout)
		argv := []string{filepath.Join(runtime, "bin", "codex-session-relay"),
			"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "start"}
		out, stderr, code, err := upgradeRunCommand(ctx, argv[0], argv[1:]...)
		cancel()
		from := "the pointer"
		if runtime == r.previous {
			from = "the runtime the pointer named before the stop"
		}
		r.note(upgradeStepStart, argv, code, "started from "+from+": "+runtime+"\n"+out+stderr, err)
		if err == nil && code == 0 {
			r.startFrom = runtime
			return
		}
		if i+1 == len(candidates) {
			// Nothing started the service. startFrom names the last runtime tried, so the post-check
			// reads the status the way this attempt left it.
			r.startFrom = runtime
		}
	}
}

// upgradePostCheck is what step 8 established: the status and reason it reports, every reason it
// found, and whether the configuration file changed.
type upgradePostCheck struct {
	code          int
	reason        string
	reasons       []string
	configChanged bool
	// mismatch reports that the post-check found the pointer and the runtime in service disagreeing
	// with the runtime the update installed. It is carried apart from the other findings because the
	// decided order lets it outrank a failed update, which a service that merely came up slowly does
	// not.
	mismatch bool
}

// postCheck is step 8: the pointer must name the runtime the update installed and that runtime's
// crw must report the version the archive carried, the service must come up running and matching
// within the wait budget, and the configuration file must be unchanged.
func (r *upgradeRunState) postCheck() upgradePostCheck {
	var post upgradePostCheck
	pointer, err := upgradePointerTarget(r.e)
	switch {
	case err != nil:
		r.note(upgradeStepPostCheck, nil, 1, "", err)
		post.reasons = append(post.reasons, upgradeReasonRuntimeMismatch)
		post.mismatch = true
	case r.promoted && r.installed == "":
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the update named no runtime it installed, so nothing shows the pointer names what it installed"))
		post.reasons = append(post.reasons, upgradeReasonRuntimeMismatch)
		post.mismatch = true
	case r.promoted && !upgradeSameDirectory(pointer, r.installed):
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the runtime pointer names %s, not %s, the runtime the update installed", pointer, r.installed))
		post.reasons = append(post.reasons, upgradeReasonRuntimeMismatch)
		post.mismatch = true
	case r.promoted && r.startFrom != "" && !upgradeSameDirectory(r.startFrom, r.installed):
		// The pointer names the runtime the update installed, but the service did not come back on it:
		// the restart fell back to the runtime the pointer named before the stop. A pointer that names
		// one runtime while the service runs another is the disagreement this step exists to catch, so
		// the recovery is recorded as a mismatch rather than a success.
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the service came back on %s, not %s, the runtime the update installed", r.startFrom, r.installed))
		post.reasons = append(post.reasons, upgradeReasonRuntimeMismatch)
		post.mismatch = true
	default:
		// The version is compared only against a runtime the update put in service. An update that
		// did not land leaves the pointer on the runtime it replaced, whose version is the previous
		// one by design: that is the rollback, not a mismatch.
		if r.promoted {
			version, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepPostCheck, filepath.Join(pointer, "bin", "crw"), "--version")
			got := strings.TrimSpace(version)
			if err != nil || code != 0 || got != r.version {
				r.note(upgradeStepPostCheck, nil, 1, version, fmt.Errorf("the runtime's crw reports the version %q, not %q, the version the archive carried", got, r.version))
				post.reasons = append(post.reasons, upgradeReasonRuntimeMismatch)
				post.mismatch = true
			}
		}
	}
	runtime := r.startFrom
	if runtime == "" {
		runtime = pointer
	}
	if runtime != "" && !r.waitForService(runtime) {
		post.reasons = append(post.reasons, upgradeReasonPostCheck)
	}
	// A configuration file the snapshot read and that is now gone is a change the run can state
	// exactly. A file that still exists but cannot be read is not a change: the run cannot tell
	// whether it differs, so that is a post-check finding of its own rather than a claimed change.
	digest, err := upgradeFileDigest(upgradeConfigPath(r.e))
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the configuration file is gone"))
		post.reasons = append(post.reasons, upgradeReasonConfigChanged)
		post.configChanged = true
	case err != nil:
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the configuration file could not be read: %w", err))
		post.reasons = append(post.reasons, upgradeReasonPostCheck)
	case digest != r.beforeConfig:
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the configuration file changed"))
		post.reasons = append(post.reasons, upgradeReasonConfigChanged)
		post.configChanged = true
	}
	if len(post.reasons) == 0 {
		return post
	}
	post.code = upgradeExitPostCheck
	post.reason = post.reasons[0]
	return post
}

// waitForService reads the service status again every upgradeServiceInterval until it reports the
// daemon running and matching, or upgradeServiceBudget passes; the last answer is recorded either
// way. The wait is measured on the wall clock, not on Env.Now: a test's fixed clock must not be
// able to stop it ending.
func (r *upgradeRunState) waitForService(runtime string) bool {
	ctx, cancel := context.WithTimeout(r.ctx, upgradeServiceBudget)
	defer cancel()
	relay := filepath.Join(runtime, "bin", "codex-session-relay")
	last := ""
	for {
		out, code, err := r.command(ctx, upgradeCommandTimeout, upgradeStepPostCheck, relay,
			"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "status")
		last = out
		if err == nil && code == 0 && upgradeServiceRunning(out) {
			return true
		}
		if ctx.Err() != nil {
			r.note(upgradeStepPostCheck, nil, 1, last, fmt.Errorf("the service did not report itself running and matching within %s", upgradeServiceBudget))
			return false
		}
		if !upgradeWait(ctx, upgradeServiceInterval) {
			r.note(upgradeStepPostCheck, nil, 1, last, fmt.Errorf("the wait for the service ended before it reported itself running and matching"))
			return false
		}
	}
}

// upgradeWait waits for one interval, or until ctx ends; false when the context ended first.
func upgradeWait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// upgradeServiceRunning reports whether a status answer says the daemon runs and matches.
func upgradeServiceRunning(out string) bool {
	var answer struct {
		Running      *bool `json:"running"`
		LaunchPolicy *struct {
			MatchesRunning string `json:"matchesRunning"`
		} `json:"launchPolicy"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return false
	}
	if answer.Running == nil || !*answer.Running || answer.LaunchPolicy == nil {
		return false
	}
	return answer.LaunchPolicy.MatchesRunning == "same"
}

// upgradeSameDirectory reports whether two spellings name the same directory, resolving both so a
// path through a symbolic link compares equal to its target.
func upgradeSameDirectory(a, b string) bool {
	if a == b {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return resolvedA == resolvedB
}

// upgradePointerTarget is where the runtime pointer resolves, as a real path.
func upgradePointerTarget(e *Env) (string, error) {
	path := filepath.Join(coreHomeDir(e, "HOME", ""), upgradeRuntimeDir, "current")
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("the runtime pointer %s: %w", path, err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("the runtime pointer %s names no directory", path)
	}
	return target, nil
}

func upgradeConfigPath(e *Env) string {
	return filepath.Join(coreHomeDir(e, "CODEX_HOME", ".codex"), "config.toml")
}

// upgradeExtract unpacks a tar.gz into dir, refusing an entry that escapes it or names a symlink.
func upgradeExtract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.ToSlash(filepath.Clean(header.Name))
		if filepath.IsAbs(header.Name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("the archive entry %q is not a path inside the extract directory", header.Name)
		}
		target := filepath.Join(dir, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			// A name already a symlink would be followed outside the directory.
			if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("the archive entry %q names an existing symlink", header.Name)
			}
			mode := os.FileMode(header.Mode).Perm()
			if mode == 0 {
				mode = 0o600
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, reader); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}
