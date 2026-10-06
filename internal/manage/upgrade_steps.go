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
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// upgradeSumsName is the checksum file a release publishes beside its archives.
const upgradeSumsName = "SHA256SUMS"

// upgradeArchiveGlob is the archive this host installs: the linux/amd64 release build.
const upgradeArchiveGlob = "crw_*_linux_amd64.tar.gz"

// upgradeRuntimeDir is the fixed destination whose current/bin the plugin wiring runs.
const upgradeRuntimeDir = ".local/share/crw-runtime"

// upgradeCommandTimeout bounds one gh call so a hung network answer cannot hold the run.
const upgradeCommandTimeout = 60 * time.Second

// upgradeStoreTimeout bounds the read-only store open.
const upgradeStoreTimeout = 5 * time.Second

// upgradeRunCommand runs exe and returns its stdout and exit status. A command that ran and
// failed is a status with no error; one that could not be started is an error.
func upgradeRunCommand(ctx context.Context, exe string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, upgradeCommandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, exit.ExitCode(), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, 0, ctxErr
		}
		return out, 0, err
	}
	return out, 0, nil
}

// upgradeOutputHead keeps the first upgradeRecordHead bytes of a command's output.
func upgradeOutputHead(text string) string {
	if len(text) <= upgradeRecordHead {
		return text
	}
	return text[:upgradeRecordHead]
}

// verifySums is step 1: find the archive in the release directory and verify it against the
// SHA256SUMS beside it.
func (r *upgradeRunState) verifySums() (string, int, string) {
	matches, err := filepath.Glob(filepath.Join(r.opts.ReleaseDir, upgradeArchiveGlob))
	if err != nil || len(matches) == 0 {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("no %s in %s", upgradeArchiveGlob, r.opts.ReleaseDir))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	archive := matches[0]
	sums, err := os.ReadFile(filepath.Join(r.opts.ReleaseDir, upgradeSumsName))
	if err != nil {
		r.note(upgradeStepSums, nil, 1, "", err)
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	want := upgradeSumsFor(string(sums), filepath.Base(archive))
	if want == "" {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("%s names no digest for %s", upgradeSumsName, filepath.Base(archive)))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	got, err := upgradeFileDigest(archive)
	if err != nil {
		r.note(upgradeStepSums, nil, 1, "", err)
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	r.note(upgradeStepSums, nil, 0, want+"  "+filepath.Base(archive), nil)
	if !strings.EqualFold(got, want) {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("%s does not match %s", filepath.Base(archive), upgradeSumsName))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	return archive, 0, ""
}

// upgradeSumsFor is the digest SHA256SUMS lists for name, or "" when it lists none.
func upgradeSumsFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return fields[0]
		}
	}
	return ""
}

// upgradeFileDigest is a file's sha256, lowercase hex.
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

// extractAndResolve is step 2: unpack the archive into W/extract and resolve the version's
// short commit hash to a full SHA through the forge API.
func (r *upgradeRunState) extractAndResolve() (int, string) {
	r.extract = filepath.Join(r.dir, "extract")
	if err := os.MkdirAll(r.extract, 0o700); err != nil {
		r.note(upgradeStepExtract, nil, 1, "", err)
		return upgradeExitRefused, upgradeReasonExtractFailed
	}
	if err := upgradeExtract(r.archive, r.extract); err != nil {
		r.note(upgradeStepExtract, nil, 1, "", err)
		return upgradeExitRefused, upgradeReasonExtractFailed
	}
	crw := filepath.Join(r.extract, "crw")
	version, code, err := r.command(upgradeStepExtract, crw, "--version")
	if err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonExtractFailed
	}
	short := upgradeShortHash(version)
	if short == "" {
		r.note(upgradeStepCommit, nil, 1, "", fmt.Errorf("the version %q carries no -g hash", strings.TrimSpace(version)))
		return upgradeExitRefused, upgradeReasonCommitUnknown
	}
	out, code, err := r.command(upgradeStepCommit, "gh", "api", "repos/"+r.cfg.Repository+"/commits/"+short)
	if err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonCommitUnknown
	}
	var answer struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil || answer.SHA == "" {
		r.note(upgradeStepCommit, nil, 1, out, fmt.Errorf("the commit answer names no sha"))
		return upgradeExitRefused, upgradeReasonCommitUnknown
	}
	r.commit = answer.SHA
	return 0, ""
}

// upgradeShortHash is the hash after -g in a version string, or "".
func upgradeShortHash(version string) string {
	_, rest, found := strings.Cut(strings.TrimSpace(version), "-g")
	if !found {
		return ""
	}
	hash := strings.TrimSpace(rest)
	if hash == "" || strings.ContainsAny(hash, " \t\r\n") {
		return ""
	}
	return hash
}

// checkDevGate is step 3: the commit's dev-gate check run must have concluded success.
func (r *upgradeRunState) checkDevGate() (int, string) {
	out, code, err := r.command(upgradeStepDevGate, "gh", "api", "repos/"+r.cfg.Repository+"/commits/"+r.commit+"/check-runs")
	if err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonDevGate
	}
	var answer struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		r.note(upgradeStepDevGate, nil, 1, out, err)
		return upgradeExitRefused, upgradeReasonDevGate
	}
	for _, run := range answer.CheckRuns {
		if run.Name == "dev-gate" && run.Conclusion == "success" {
			return 0, ""
		}
	}
	r.note(upgradeStepDevGate, nil, 1, out, fmt.Errorf("no dev-gate check run concluded success"))
	return upgradeExitRefused, upgradeReasonDevGate
}

// checkOpenAttempts is step 4: read the relay store read-only and refuse when any attempt is
// not settled. The store is opened in place and never created.
func (r *upgradeRunState) checkOpenAttempts() (int, string) {
	state, err := r.relayState()
	if err != nil {
		r.note(upgradeStepAttempts, nil, 1, "", err)
		return upgradeExitRefused, upgradeReasonStoreRead
	}
	r.state = state
	ctx, cancel := context.WithTimeout(r.ctx, upgradeStoreTimeout)
	defer cancel()
	read, err := store.OpenReadOnly(ctx, filepath.Join(state, "relay.sqlite3"), upgradeStoreTimeout)
	if err != nil {
		r.note(upgradeStepAttempts, nil, 1, "", err)
		return upgradeExitRefused, upgradeReasonStoreRead
	}
	defer read.Close()
	var open int
	query := "select count(*) from attempts where internal_state != 'settled'"
	if err := read.QueryRowContext(ctx, query).Scan(&open); err != nil {
		r.note(upgradeStepAttempts, []string{query}, 1, "", err)
		return upgradeExitRefused, upgradeReasonStoreRead
	}
	r.note(upgradeStepAttempts, []string{query}, 0, fmt.Sprintf("%d", open), nil)
	if open != 0 {
		return upgradeExitOpenAttempts, upgradeReasonOpenAttempts
	}
	return 0, ""
}

// relayState is the state directory the run reads and stops: the configured one, else the one
// the relay's own doctor answer selects.
func (r *upgradeRunState) relayState() (string, error) {
	if r.cfg.Relay.State != "" {
		return r.cfg.Relay.State, nil
	}
	state, err := r.e.relayHelperDoctorState(r.ctx, r.cfg.Relay.Socket)
	return state, err
}

// snapshot is step 5: what the run must be able to compare afterwards.
func (r *upgradeRunState) snapshot() (int, string) {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepSnapshot, nil, 1, "", err)
		return upgradeExitRefused, upgradeReasonPointer
	}
	if _, code, err := r.command(upgradeStepSnapshot, filepath.Join(pointer, "bin", "crw"), "install", "status"); err != nil || code != 0 {
		return upgradeExitRefused, upgradeReasonPointer
	}
	digest, err := upgradeFileDigest(upgradeConfigPath(r.e))
	if err != nil {
		r.note(upgradeStepSnapshot, nil, 1, "", err)
		return upgradeExitRefused, upgradeReasonPointer
	}
	r.beforeConfig = digest
	r.note(upgradeStepSnapshot, nil, 0, pointer+"\n"+digest, nil)
	return 0, ""
}

// stopAndUpdate is step 6: stop the service with the running executable, then install the
// archive with the extracted one. It returns the update's exit status.
func (r *upgradeRunState) stopAndUpdate() int {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepStop, nil, 1, "", err)
		return upgradeExitRefused
	}
	if _, _, err := r.command(upgradeStepStop, filepath.Join(pointer, "bin", "codex-session-relay"),
		"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "stop"); err != nil {
		return upgradeExitRefused
	}
	args := []string{"install", "update", "--from", r.archive, "--state", r.state,
		"--socket", r.cfg.Relay.Socket, "--backup-state-to", filepath.Join(r.dir, "state-backup")}
	if r.opts.Issue != "" {
		args = append(args, "--issue", r.opts.Issue)
	}
	_, code, err := r.command(upgradeStepUpdate, filepath.Join(r.extract, "crw"), args...)
	if err != nil {
		return upgradeExitRefused
	}
	return code
}

// start is step 7: start the service from the new pointer's executable. It is called whether
// or not the update succeeded, so a failed update never leaves the service stopped.
func (r *upgradeRunState) start() {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepStart, nil, 1, "", err)
		return
	}
	_, _, _ = r.command(upgradeStepStart, filepath.Join(pointer, "bin", "codex-session-relay"),
		"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "start")
}

// postCheck is step 8: the new pointer, the new version, the service running and matching the
// launch policy, and the configuration file unchanged.
func (r *upgradeRunState) postCheck() (int, string) {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepPostCheck, nil, 1, "", err)
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	version, code, err := r.command(upgradeStepPostCheck, filepath.Join(pointer, "bin", "crw"), "--version")
	if err != nil || code != 0 || strings.TrimSpace(version) == "" {
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	out, code, err := r.command(upgradeStepPostCheck, filepath.Join(pointer, "bin", "codex-session-relay"),
		"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "status")
	if err != nil || code != 0 {
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	if !upgradeServiceRunning(out) {
		r.note(upgradeStepPostCheck, nil, 1, out, fmt.Errorf("the service is not running and matching"))
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	digest, err := upgradeFileDigest(upgradeConfigPath(r.e))
	if err != nil || digest != r.beforeConfig {
		r.note(upgradeStepPostCheck, nil, 1, "", fmt.Errorf("the configuration file changed"))
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	return 0, ""
}

// upgradeServiceRunning reports whether a service status answer says the daemon runs and
// matches the launch policy.
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

// upgradeConfigPath is the configuration file whose digest must not change.
func upgradeConfigPath(e *Env) string {
	return filepath.Join(coreHomeDir(e, "CODEX_HOME", ".codex"), "config.toml")
}

// upgradeExtract unpacks a tar.gz into dir, refusing an entry that would escape it.
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
		target, err := upgradeJoin(dir, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := upgradeWrite(target, reader, header); err != nil {
				return err
			}
		}
	}
}

// upgradeJoin is dir/name. A name that is absolute or that walks out of dir is refused rather
// than neutralized, because an archive carrying one is not a release archive.
func upgradeJoin(dir, name string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if filepath.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("the archive entry %q is not a path inside the extract directory", name)
	}
	return filepath.Join(dir, name), nil
}

// upgradeWrite writes one archive entry.
func upgradeWrite(target string, body io.Reader, header *tar.Header) error {
	mode := os.FileMode(header.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, body); err != nil {
		return err
	}
	return nil
}
