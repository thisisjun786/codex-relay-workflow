package configguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1150: verified execution evidence for the report-only SessionStart probe.
//
// The report-only hook must not write anything (J4), so it cannot cache what it measured. An
// explicit command can: `crw install features enable` runs `codex features list` after it has
// activated, and records here what that run showed together with the codex version and the digest
// of config.toml it was measured against. The hook reuses the record only while both still match
// what it sees now (the codex version, config.toml, and the project and system layers that apply
// to the working directory, judged after the version is read), and takes it from nothing else: a file's mtime is not evidence, and a record
// without a readable version or digest is no record. When the version or the config changes the
// hook measures again; when the record shows a soft flag off the hook still warns every session
// (a resumed or compacted session needs the notice), it just does not run the listing to learn it.

// SelfHealProbeEvidence is what one explicit, verified `codex features list` run showed.
type SelfHealProbeEvidence struct {
	CodexVersion string
	// ConfigSHA256 is the sha256 of config.toml's bytes, or selfHealConfigAbsent when it is absent.
	ConfigSHA256 string
	RecordedAt   string
	// LayersSHA256 fingerprints the other configuration layers codex reads when it runs in the
	// recorded working directory (selfHealLayersDigest).
	LayersSHA256 string
	// Features holds the state of every declared flag.
	Features map[string]bool
}

const selfHealConfigAbsent = "absent"

func parseSelfHealProbeEvidence(v any) *SelfHealProbeEvidence {
	o, ok := v.(pyjson.Object)
	if !ok {
		return nil
	}
	version, _ := o.Get("codexVersion").(string)
	digest, _ := o.Get("configSha256").(string)
	layers, _ := o.Get("layersSha256").(string)
	at, _ := o.Get("recordedAt").(string)
	features, ok := o.Get("features").(pyjson.Object)
	if !ok || version == "" || digest == "" || layers == "" {
		return nil
	}
	e := &SelfHealProbeEvidence{CodexVersion: version, ConfigSHA256: digest, LayersSHA256: layers, RecordedAt: at, Features: map[string]bool{}}
	for _, field := range features {
		if b, ok := field.Value.(bool); ok {
			e.Features[field.Key] = b
		}
	}
	return e
}

func selfHealProbeEvidenceObject(e *SelfHealProbeEvidence) pyjson.Object {
	features := pyjson.Object{}
	for _, key := range DeclaredFeatures() {
		if value, ok := e.Features[string(key)]; ok {
			features = append(features, pyjson.Field{Key: string(key), Value: value})
		}
	}
	return pyjson.Object{
		{Key: "codexVersion", Value: e.CodexVersion},
		{Key: "configSha256", Value: e.ConfigSHA256},
		{Key: "layersSha256", Value: e.LayersSHA256},
		{Key: "recordedAt", Value: e.RecordedAt},
		{Key: "features", Value: features},
	}
}

// selfHealEvidenceMaxBytes bounds one fingerprinted file: a config layer larger than this is not
// fingerprinted, and evidence is then neither recorded nor reused.
const selfHealEvidenceMaxBytes = 8 << 20

// selfHealEvidenceOpen opens a fingerprinted file; a variable only so a test can stall it.
var selfHealEvidenceOpen = os.OpenFile

// selfHealEvidenceHash is the sha256 of a config file the evidence names, or nil when nothing is
// there. The SessionStart hook reads these files inside its shared deadline, so the read can never
// wait: the file is opened without blocking (a FIFO with no writer opens at once), and anything that
// is not a regular file once open (a FIFO, a device, a directory) is an error, as is a file past
// selfHealEvidenceMaxBytes. A dangling link is an error, not an absent file, as activationReadFile
// has it.
func selfHealEvidenceHash(path string) (*string, error) {
	f, err := selfHealEvidenceOpen(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := os.Lstat(path); errors.Is(lerr, fs.ErrNotExist) {
				return nil, nil
			}
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, selfHealEvidenceMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if n > selfHealEvidenceMaxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, selfHealEvidenceMaxBytes)
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	return &digest, nil
}

// selfHealKernelJoin is dir/name without cleaning dir: the codex run inherits CODEX_HOME as it is
// spelled, and the kernel resolves a ".." that follows a symbolic link physically, where
// filepath.Join folds it lexically and would name another directory's file. The evidence has to
// fingerprint the file codex reads, so it takes the spelling to the kernel unchanged (CRW-1150).
func selfHealKernelJoin(dir, name string) string {
	switch {
	case dir == "":
		return name
	case strings.HasSuffix(dir, "/"):
		return dir + name
	}
	return dir + "/" + name
}

// selfHealBounded runs one fingerprint step inside the round's deadline. A read of a regular file on
// a stalled filesystem cannot be interrupted, so the step runs in its own goroutine and is abandoned
// when ctx ends: the caller gets ctx's error at once and the round answers with a measurement that
// failed. The abandoned goroutine ends with the process or when the read returns.
func selfHealBounded[T any](ctx context.Context, step func() (T, error)) (T, error) {
	if ctx == nil || ctx.Done() == nil {
		return step()
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := step()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// selfHealConfigDigest names the config.toml the evidence was measured against.
func selfHealConfigDigest(ctx context.Context, home string) (string, error) {
	return selfHealBounded(ctx, func() (string, error) {
		digest, err := selfHealEvidenceHash(selfHealKernelJoin(home, "config.toml"))
		if err != nil {
			return "", err
		}
		if digest == nil {
			return selfHealConfigAbsent, nil
		}
		return *digest, nil
	})
}

// selfHealLayerFiles are the config layers other than the user's config.toml that codex reads for a
// session in cwd: the project layers (.codex/config.toml of cwd and of each ancestor, which also
// names the project) and the system and managed layers. A layer that is absent contributes nothing,
// so one appearing changes the fingerprint. cwd is walked as given and, when it differs, as the
// physical directory it resolves to: codex takes its working directory from getcwd, which names the
// physical directory, while the hook's os.Getwd keeps a logical PWD that went through a symbolic
// link, so both ancestor chains are covered.
func selfHealLayerFiles(home string, cwds ...string) []string {
	files := []string{"/etc/codex/config.toml", "/etc/codex/managed_config.toml", "/etc/codex/requirements.toml",
		selfHealKernelJoin(home, "managed_config.toml"), selfHealKernelJoin(home, "requirements.toml")}
	seen := map[string]bool{}
	for _, cwd := range cwds {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			if layer := filepath.Join(dir, ".codex", "config.toml"); !seen[layer] {
				seen[layer] = true
				files = append(files, layer)
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return files
}

// selfHealLayersDigest fingerprints the layers that apply to a codex run in cwd besides the user's
// config.toml. An unknown or relative cwd, one whose physical directory cannot be resolved, or a
// layer that cannot be read, is an error: evidence is then neither recorded nor reused, and the hook
// measures.
func selfHealLayersDigest(ctx context.Context, home, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", errSelfHealLayersUnknown
	}
	return selfHealBounded(ctx, func() (string, error) {
		physical, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return "", err
		}
		sum := sha256.New()
		for _, path := range selfHealLayerFiles(home, filepath.Clean(cwd), filepath.Clean(physical)) {
			digest, err := selfHealEvidenceHash(path)
			if err != nil {
				return "", err
			}
			if digest != nil {
				sum.Write([]byte(path + "\t" + *digest + "\n"))
			}
		}
		return hex.EncodeToString(sum.Sum(nil)), nil
	})
}

var errSelfHealLayersUnknown = errSelfHealReport("the working directory of the codex run is not known")

// selfHealCodexVersion is the trimmed first line of `codex --version`, or "" when it cannot be read.
func selfHealCodexVersion(run CodexRunner) string {
	res := run([]string{"--version"})
	if res.ExitCode != 0 {
		return ""
	}
	line, _, _ := strings.Cut(text.Trim(res.Stdout), "\n")
	return text.Trim(line)
}

// RecordSelfHealEvidenceDeps are injected, as the other explicit commands' are.
type RecordSelfHealEvidenceDeps struct {
	CodexHome string
	// Cwd is the directory `codex features list` runs in, which decides the project config layers that
	// apply. "" means it is not known, and no evidence is recorded.
	Cwd string
	Run CodexRunner
	Now func() string
	// Ctx ends the recording: a subprocess, a fingerprint read or a marker lock wait that has not
	// finished when it ends is abandoned and the call returns. A marker publication already running
	// cannot be cancelled and finishes on its own (see RecordSelfHealEvidence). nil means no deadline.
	Ctx context.Context
}

// RecordSelfHealEvidence is called by an explicit command after it changed the declared flags. It
// measures once more (features list, bracketed by two version reads) and records the result only when
// config.toml and the other layers did not change while it measured, and the codex that listed is the
// one whose version is recorded: each runner call resolves the executable afresh, so a binary
// replaced between the listing and the version read would otherwise pair one codex's listing with
// another's version. A measurement it cannot make removes an older record, which no longer describes
// the flags just changed, and is not an error: the hook falls back to measuring.
func RecordSelfHealEvidence(deps RecordSelfHealEvidenceDeps) error {
	ctx := deps.Ctx
	before, err := selfHealConfigDigest(ctx, deps.CodexHome)
	if err != nil {
		return dropSelfHealEvidence(ctx, deps.CodexHome)
	}
	layersBefore, err := selfHealLayersDigest(ctx, deps.CodexHome, deps.Cwd)
	if err != nil {
		return dropSelfHealEvidence(ctx, deps.CodexHome)
	}
	version := selfHealCodexVersion(deps.Run)
	state, err := ReadDeclaredState(deps.Run)
	if err != nil {
		return dropSelfHealEvidence(ctx, deps.CodexHome)
	}
	versionAfter := selfHealCodexVersion(deps.Run)
	after, err := selfHealConfigDigest(ctx, deps.CodexHome)
	if version == "" || version != versionAfter || err != nil || after != before {
		return dropSelfHealEvidence(ctx, deps.CodexHome)
	}
	if layersAfter, err := selfHealLayersDigest(ctx, deps.CodexHome, deps.Cwd); err != nil || layersAfter != layersBefore {
		return dropSelfHealEvidence(ctx, deps.CodexHome)
	}
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	probe := &SelfHealProbeEvidence{CodexVersion: version, ConfigSHA256: after, LayersSHA256: layersBefore, RecordedAt: now(), Features: state}
	// The marker is read and published under its lock, so a disable that completes meanwhile is read
	// here and keeps its opt-out, or waits and overwrites this record (never the reverse).
	publish := func() error {
		return updateSelfHealMarker(deps.CodexHome, true, func() (*SelfHealMarker, error) {
			if ctx != nil && ctx.Err() != nil {
				// The deadline ended while this waited for the lock: nothing is recorded.
				return nil, nil
			}
			marker, err := ReadSelfHealMarkerFile(deps.CodexHome)
			if err != nil {
				return nil, err
			}
			if marker == nil {
				if _, statErr := os.Stat(SelfHealMarkerPath(deps.CodexHome)); statErr == nil {
					// A readable but malformed marker is left alone, as ClearSelfHealOptOut leaves it.
					return nil, nil
				}
				marker = &SelfHealMarker{}
			}
			if marker.OptedOut != nil && *marker.OptedOut {
				// An explicit opt-out that came after the enable's own clear stands.
				return nil, nil
			}
			marker.Probe = probe
			return marker, nil
		})
	}
	if ctx == nil {
		return publish()
	}
	// The deadline also bounds the caller's wait for the lock, the read and the publication. A file
	// operation cannot be cancelled, so one already running when the deadline ends finishes on its
	// own, still under the lock and from the marker it read under it: it neither overwrites an
	// opt-out nor outlives the lock, and the evidence it writes is checked against the config
	// digests before every use. A step that has not started when the deadline ends writes nothing.
	published := make(chan error, 1)
	go func() { published <- publish() }()
	select {
	case err := <-published:
		return err
	case <-ctx.Done():
		return nil
	}
}

// updateSelfHealMarker is the one read-modify-write of the marker file, under an exclusive lock so the
// explicit commands' writers (enable's recorder, disable's opt-out, enable's clear, the removal of
// stale evidence) never interleave: a stale marker read before another writer published is never
// published after it (CRW-1150). update returns the marker to publish, or nil to leave the file as it
// is. A busy lock is an error and writes nothing; when the filesystem cannot lock at all, a writer
// that is not optional (an explicit opt-out or clear, which must not be lost) goes on without the
// lock, as it did before the lock existed, and the optional recorder records nothing.
func updateSelfHealMarker(home string, requireLock bool, update func() (*SelfHealMarker, error)) error {
	unlock, err := lockSelfHealMarker(home)
	switch {
	case err == nil:
		defer unlock()
	case requireLock || errors.Is(err, errSelfHealMarkerBusy):
		return err
	}
	marker, err := update()
	if err != nil || marker == nil {
		return err
	}
	return WriteSelfHealMarkerFile(home, marker)
}

// dropSelfHealEvidence removes the record of an older measurement after a recording attempt that
// could not measure. The attempt follows a command that changed the declared flags, so the oracle's
// mtime cache no longer describes them either: it is retired too (selfHealRetireLegacyCache), and a
// marker with neither is left as it is.
//
// The deadline bounds how long the caller waits for the drop, not whether it retires what the
// failed recording left behind (CRW-1169). The marker read, the lock wait and the write run in their
// own goroutine and the call returns nil the moment ctx ends, with no further wait, so a held lock
// or a stalled filesystem cannot keep the explicit command past it. The goroutine does not look at
// ctx: a recording whose time ran out (a slow codex used it up) still retires the oracle's mtime
// cache and the older record when the lock is free, as the synchronous drop did, so a legacy-only
// allEnabled cache cannot vouch again for flags the command just changed, if the process lives to
// see it finish. A drop still waiting for a lock held past the deadline finishes on its own later,
// from the marker it reads under the lock (it never overwrites an opt-out), or ends without writing
// when the separate marker-lock wait (selfHealMarkerLockWait) runs out or an I/O step fails; it is
// not retried. nil ctx means no deadline.
func dropSelfHealEvidence(ctx context.Context, home string) error {
	drop := func() error {
		if marker, err := ReadSelfHealMarkerFile(home); err != nil || marker == nil {
			return err
		}
		return updateSelfHealMarker(home, false, func() (*SelfHealMarker, error) {
			marker, err := ReadSelfHealMarkerFile(home)
			if err != nil || marker == nil {
				return nil, err
			}
			if marker.Probe == nil && !marker.probeSeen && (marker.AllEnabled == nil || !*marker.AllEnabled) {
				return nil, nil
			}
			marker.Probe = nil
			selfHealRetireLegacyCache(marker)
			return marker, nil
		})
	}
	if ctx == nil {
		return drop()
	}
	dropped := make(chan error, 1)
	go func() { dropped <- drop() }()
	select {
	case err := <-dropped:
		return err
	case <-ctx.Done():
		return nil
	}
}

// selfHealRetireLegacyCache turns the oracle's all-enabled cache off (allEnabled false is the
// oracle's own spelling of a cache miss) once probe evidence has been recorded in, read from or
// dropped from a marker, so the mtime cache cannot vouch again when the evidence is gone or no
// longer parses. A marker that never carried evidence keeps its cache.
func selfHealRetireLegacyCache(marker *SelfHealMarker) {
	if marker.AllEnabled != nil && *marker.AllEnabled {
		off := false
		marker.AllEnabled = &off
	}
}

// selfHealEvidenceState is the recorded flag state when the evidence still describes this codex
// and this config.toml and covers every soft flag; ok is false when the hook has to measure.
func selfHealEvidenceState(deps SelfHealReportDeps, marker *SelfHealMarker, healable []DeclaredFeature) (map[string]bool, bool) {
	if marker == nil || marker.Probe == nil {
		return nil, false
	}
	e := marker.Probe
	for _, key := range healable {
		if _, ok := e.Features[string(key)]; !ok {
			return nil, false
		}
	}
	// The version is read first: the probe can take most of the round's time, and a change completed
	// while it ran must be seen by the digests that judge the record after it.
	if version := selfHealCodexVersion(deps.Run); version == "" || version != e.CodexVersion {
		return nil, false
	}
	if digest, err := selfHealConfigDigest(deps.Ctx, deps.CodexHome); err != nil || digest != e.ConfigSHA256 {
		return nil, false
	}
	if layers, err := selfHealLayersDigest(deps.Ctx, deps.CodexHome, deps.Cwd); err != nil || layers != e.LayersSHA256 {
		return nil, false
	}
	return e.Features, true
}
