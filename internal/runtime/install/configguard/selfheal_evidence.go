package configguard

import (
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
// to the working directory), and takes it from nothing else: a file's mtime is not evidence, and a record
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

// selfHealConfigDigest names the config.toml the evidence was measured against.
func selfHealConfigDigest(home string) (string, error) {
	digest, err := selfHealEvidenceHash(filepath.Join(home, "config.toml"))
	if err != nil {
		return "", err
	}
	if digest == nil {
		return selfHealConfigAbsent, nil
	}
	return *digest, nil
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
		filepath.Join(home, "managed_config.toml"), filepath.Join(home, "requirements.toml")}
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
func selfHealLayersDigest(home, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", errSelfHealLayersUnknown
	}
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
}

// RecordSelfHealEvidence is called by an explicit command after it changed the declared flags. It
// measures once more (features list and version), and records the result only when config.toml did
// not change while it measured. A measurement it cannot make removes an older record, which no
// longer describes the flags just changed, and is not an error: the hook falls back to measuring.
func RecordSelfHealEvidence(deps RecordSelfHealEvidenceDeps) error {
	before, err := selfHealConfigDigest(deps.CodexHome)
	if err != nil {
		return dropSelfHealEvidence(deps.CodexHome)
	}
	layersBefore, err := selfHealLayersDigest(deps.CodexHome, deps.Cwd)
	if err != nil {
		return dropSelfHealEvidence(deps.CodexHome)
	}
	state, err := ReadDeclaredState(deps.Run)
	if err != nil {
		return dropSelfHealEvidence(deps.CodexHome)
	}
	version := selfHealCodexVersion(deps.Run)
	after, err := selfHealConfigDigest(deps.CodexHome)
	if version == "" || err != nil || after != before {
		return dropSelfHealEvidence(deps.CodexHome)
	}
	if layersAfter, err := selfHealLayersDigest(deps.CodexHome, deps.Cwd); err != nil || layersAfter != layersBefore {
		return dropSelfHealEvidence(deps.CodexHome)
	}
	marker, err := ReadSelfHealMarkerFile(deps.CodexHome)
	if err != nil {
		return err
	}
	if marker == nil {
		marker = &SelfHealMarker{}
		if _, statErr := os.Stat(SelfHealMarkerPath(deps.CodexHome)); statErr == nil {
			// A readable but malformed marker is left alone, as ClearSelfHealOptOut leaves it.
			return nil
		}
	}
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	marker.Probe = &SelfHealProbeEvidence{CodexVersion: version, ConfigSHA256: after, LayersSHA256: layersBefore, RecordedAt: now(), Features: state}
	return WriteSelfHealMarkerFile(deps.CodexHome, marker)
}

// dropSelfHealEvidence removes the record of an older measurement after a recording attempt that
// could not measure. The attempt follows a command that changed the declared flags, so the oracle's
// mtime cache no longer describes them either: it is retired too (selfHealRetireLegacyCache), and a
// marker with neither is left as it is.
func dropSelfHealEvidence(home string) error {
	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil {
		return err
	}
	if marker.Probe == nil && !marker.probeSeen && (marker.AllEnabled == nil || !*marker.AllEnabled) {
		return nil
	}
	marker.Probe = nil
	selfHealRetireLegacyCache(marker)
	return WriteSelfHealMarkerFile(home, marker)
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
	if digest, err := selfHealConfigDigest(deps.CodexHome); err != nil || digest != e.ConfigSHA256 {
		return nil, false
	}
	if layers, err := selfHealLayersDigest(deps.CodexHome, deps.Cwd); err != nil || layers != e.LayersSHA256 {
		return nil, false
	}
	if version := selfHealCodexVersion(deps.Run); version == "" || version != e.CodexVersion {
		return nil, false
	}
	return e.Features, true
}
