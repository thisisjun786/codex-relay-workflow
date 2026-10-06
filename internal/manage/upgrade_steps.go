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

const (
	upgradeSumsName     = "SHA256SUMS"
	upgradeArchiveGlob  = "crw_*_linux_amd64.tar.gz"
	upgradeRuntimeDir   = ".local/share/crw-runtime"
	upgradeStoreTimeout = 5 * time.Second
	// upgradeCommandTimeout bounds one forge call; upgradeInstallTimeout one lifecycle command,
	// which exercises the bridge and may copy a large relay state.
	upgradeCommandTimeout = 60 * time.Second
	upgradeInstallTimeout = 30 * time.Minute
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

// verifySums is step 1: find the archive and verify it against the SHA256SUMS beside it.
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
	if !strings.EqualFold(got, want) {
		r.note(upgradeStepSums, nil, 1, "", fmt.Errorf("%s does not match %s", filepath.Base(archive), upgradeSumsName))
		return "", upgradeExitRefused, upgradeReasonSumsFailed
	}
	r.note(upgradeStepSums, nil, 0, want+"  "+filepath.Base(archive), nil)
	// Pin the verified bytes so the archive cannot be swapped after the check; the installer
	// rechecks against the SHA256SUMS, so that file is pinned too.
	pinned := filepath.Join(r.dir, filepath.Base(archive))
	for _, src := range []string{archive, filepath.Join(r.opts.ReleaseDir, upgradeSumsName)} {
		if err := upgradeCopy(src, filepath.Join(r.dir, filepath.Base(src))); err != nil {
			r.note(upgradeStepSums, nil, 1, "", err)
			return "", upgradeExitRefused, upgradeReasonSumsFailed
		}
	}
	return pinned, 0, ""
}

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

// extractAndResolve is step 2: unpack the archive and resolve the version to a full commit SHA.
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

// checkOpenAttempts is step 4: read the store read-only and refuse while any attempt is unsettled.
func (r *upgradeRunState) checkOpenAttempts() (int, string) {
	state, err := r.relayState()
	if err != nil {
		return r.refuse(upgradeStepAttempts, upgradeReasonStoreRead, "", err)
	}
	r.state = state
	ctx, cancel := context.WithTimeout(r.ctx, upgradeStoreTimeout)
	defer cancel()
	read, err := store.OpenReadOnly(ctx, filepath.Join(state, "relay.sqlite3"), upgradeStoreTimeout)
	if err != nil {
		return r.refuse(upgradeStepAttempts, upgradeReasonStoreRead, "", err)
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

// relayState is the state directory the run reads and stops: configured, else the doctor's.
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
func (r *upgradeRunState) stopAndUpdate() int {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepStop, nil, 1, "", err)
		return upgradeExitRefused
	}
	if _, code, err := r.command(r.ctx, upgradeInstallTimeout, upgradeStepStop,
		filepath.Join(pointer, "bin", "codex-session-relay"),
		"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "stop"); err != nil || code != 0 {
		return upgradeExitRefused
	}
	args := []string{"install", "update", "--from", r.archive, "--state", r.state,
		"--socket", r.cfg.Relay.Socket, "--backup-state-to", filepath.Join(r.dir, "state-backup")}
	if r.opts.Issue != "" {
		args = append(args, "--issue", r.opts.Issue)
	}
	_, code, err := r.command(r.ctx, upgradeInstallTimeout, upgradeStepUpdate, filepath.Join(r.extract, "crw"), args...)
	if err != nil {
		return upgradeExitRefused
	}
	return code
}

// start is step 7: start from the new pointer's executable, whether or not the update succeeded.
func (r *upgradeRunState) start() {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepStart, nil, 1, "", err)
		return
	}
	// The recovery step runs detached, so a cancellation after the stop cannot leave it down.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), upgradeInstallTimeout)
	defer cancel()
	_, _, _ = r.command(ctx, upgradeInstallTimeout, upgradeStepStart, filepath.Join(pointer, "bin", "codex-session-relay"),
		"--state", r.state, "--socket", r.cfg.Relay.Socket, "service", "start")
}

// postCheck is step 8: the new pointer and version, the service running and matching, and an
// unchanged configuration file.
func (r *upgradeRunState) postCheck() (int, string) {
	pointer, err := upgradePointerTarget(r.e)
	if err != nil {
		r.note(upgradeStepPostCheck, nil, 1, "", err)
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	version, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepPostCheck, filepath.Join(pointer, "bin", "crw"), "--version")
	if err != nil || code != 0 || strings.TrimSpace(version) == "" {
		return upgradeExitPostCheck, upgradeReasonPostCheck
	}
	out, code, err := r.command(r.ctx, upgradeCommandTimeout, upgradeStepPostCheck, filepath.Join(pointer, "bin", "codex-session-relay"),
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
