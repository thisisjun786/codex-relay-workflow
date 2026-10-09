package configguard

import (
	"os"
	"path/filepath"
	"strings"
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
// what it sees now, and takes it from nothing else: a file's mtime is not evidence, and a record
// without a readable version or digest is no record. When the version or the config changes the
// hook measures again; when the record shows a soft flag off the hook still warns every session
// (a resumed or compacted session needs the notice), it just does not run the listing to learn it.

// SelfHealProbeEvidence is what one explicit, verified `codex features list` run showed.
type SelfHealProbeEvidence struct {
	CodexVersion string
	// ConfigSHA256 is the sha256 of config.toml's bytes, or selfHealConfigAbsent when it is absent.
	ConfigSHA256 string
	RecordedAt   string
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
	at, _ := o.Get("recordedAt").(string)
	features, ok := o.Get("features").(pyjson.Object)
	if !ok || version == "" || digest == "" {
		return nil
	}
	e := &SelfHealProbeEvidence{CodexVersion: version, ConfigSHA256: digest, RecordedAt: at, Features: map[string]bool{}}
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
		{Key: "recordedAt", Value: e.RecordedAt},
		{Key: "features", Value: features},
	}
}

// selfHealConfigDigest names the config.toml the evidence was measured against.
func selfHealConfigDigest(home string) (string, error) {
	digest, err := hashOrNull(filepath.Join(home, "config.toml"))
	if err != nil {
		return "", err
	}
	if digest == nil {
		return selfHealConfigAbsent, nil
	}
	return *digest, nil
}

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
	Run       CodexRunner
	Now       func() string
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
	state, err := ReadDeclaredState(deps.Run)
	if err != nil {
		return dropSelfHealEvidence(deps.CodexHome)
	}
	version := selfHealCodexVersion(deps.Run)
	after, err := selfHealConfigDigest(deps.CodexHome)
	if version == "" || err != nil || after != before {
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
	marker.Probe = &SelfHealProbeEvidence{CodexVersion: version, ConfigSHA256: after, RecordedAt: now(), Features: state}
	return WriteSelfHealMarkerFile(deps.CodexHome, marker)
}

func dropSelfHealEvidence(home string) error {
	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil || marker.Probe == nil {
		return err
	}
	marker.Probe = nil
	return WriteSelfHealMarkerFile(home, marker)
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
	if version := selfHealCodexVersion(deps.Run); version == "" || version != e.CodexVersion {
		return nil, false
	}
	return e.Features, true
}
