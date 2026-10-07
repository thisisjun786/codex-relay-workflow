//go:build dev

package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// CRW-964: the verification record. `crw-dev ci local` leaves a
// verification-record/1 JSON describing what it ran and on what; the record's digest is the
// sha256 of its canonical serialization, so a second reader can re-derive it, and
// `--reuse` answers a record only when every key that decides the result still matches.

// The record's schema name, and the result words a step and a run carry.
const (
	recordSchema = "verification-record/1"

	// localPassed is a step or a run that did its work and succeeded.
	localPassed = "passed"
	// localFailed is a step whose command ran and failed.
	localFailed = "failed"
	// localMissingTool is a step whose tool is not on PATH: a failure, never a pass.
	localMissingTool = "missing_tool"
	// localSkipped is a step that was not run: a failure, never a pass (answer 5).
	localSkipped = "skipped"
	// localNotApplicable is a step this run never performs, with its reason recorded.
	localNotApplicableResult = "not_applicable"

	// localPass and localFail are the run's two outcomes.
	localPass = "pass"
	localFail = "fail"
)

// verificationRecord is the verification-record/1 document.
type verificationRecord struct {
	Schema       string            `json:"schema"`
	Runner       string            `json:"runner"`
	Repository   string            `json:"repository"`
	BaseCommit   string            `json:"baseCommit"`
	HeadCommit   string            `json:"headCommit"`
	TreeHash     string            `json:"treeHash"`
	CiDigest     string            `json:"ciDigest"`
	Tools        map[string]string `json:"tools"`
	Pins         map[string]string `json:"pins"`
	GoFlags      string            `json:"goFlags"`
	GoEnv        string            `json:"goEnv"`
	Range        string            `json:"range"`
	PinMismatch  []string          `json:"pinMismatch"`
	Dependencies map[string]string `json:"dependencies"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Result       string            `json:"result"`
	Jobs         []recordJob       `json:"jobs"`
	Digest       string            `json:"digest"`
}

// recordJob is one GitHub check the run performed: validate, secrets, skill-scripts-node, gui,
// one go-product leg, or dev-gate.
type recordJob struct {
	Name   string       `json:"name"`
	Result string       `json:"result"`
	Steps  []recordStep `json:"steps"`
}

// recordStep is one step: what it ran, over what range, and how it ended.
type recordStep struct {
	Job     string  `json:"job"`
	Name    string  `json:"name"`
	Command string  `json:"command"`
	Scope   string  `json:"scope"`
	Result  string  `json:"result"`
	Seconds float64 `json:"seconds"`
	Reason  string  `json:"reason"`
}

// canonicalRecord is the record's canonical serialization: the document as JSON with its keys
// sorted, no indentation and no HTML escaping, and the digest member removed (the digest is
// computed over exactly these bytes). encoding/json sorts a map's keys, so the value goes
// through a map rather than the struct, whose field order is an implementation detail.
func canonicalRecord(record verificationRecord) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "digest")
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// recordDigest is the sha256 of the canonical serialization.
func recordDigest(record verificationRecord) (string, error) {
	canonical, err := canonicalRecord(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// sealRecord fills the digest in and returns the record.
func sealRecord(record verificationRecord) (verificationRecord, error) {
	record.Schema = recordSchema
	digest, err := recordDigest(record)
	if err != nil {
		return record, err
	}
	record.Digest = digest
	return record, nil
}

// readRecord reads and checks a record: the schema, the digest it carries, and that its bytes are
// the canonical serialization. A record that does not check out is refused rather than reused.
func readRecord(path string) (verificationRecord, error) {
	var record verificationRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("%s: %w", path, err)
	}
	if record.Schema != recordSchema {
		return record, fmt.Errorf("%s: schema %q, want %q", path, record.Schema, recordSchema)
	}
	digest, err := recordDigest(record)
	if err != nil {
		return record, err
	}
	if record.Digest != digest {
		return record, fmt.Errorf("%s: digest %s does not match its contents (%s)", path, record.Digest, digest)
	}
	return record, nil
}

// writeRecord writes the record, sealed, as canonical JSON with a trailing newline, creating the
// directory. It refuses to leave a partial file: the bytes are written to a sibling and renamed.
func writeRecord(path string, record verificationRecord) (verificationRecord, error) {
	sealed, err := sealRecord(record)
	if err != nil {
		return record, err
	}
	canonical, err := canonicalRecord(sealed)
	if err != nil {
		return record, err
	}
	pretty := new(bytes.Buffer)
	encoder := json.NewEncoder(pretty)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(sealed); err != nil {
		return record, err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return record, err
		}
	}
	if err := localWriteAtomic(path, pretty.Bytes(), 0o644); err != nil {
		return record, err
	}
	// The canonical bytes are kept beside the record so a reader can compare them byte for
	// byte without re-deriving the serialization.
	if err := localWriteAtomic(path+".canonical", canonical, 0o644); err != nil {
		return record, err
	}
	return sealed, nil
}

// localWriteAtomic writes a file through a temporary file of its own name in the same directory and
// renames it into place, so two runs never share a temporary name and a reader never sees half a file.
func localWriteAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Chmod(temp, mode); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

// externalGoFlags reports whether GOFLAGS names a modfile or an overlay: the files it reads are
// inputs the reuse keys do not cover, so a record made with one is never reused.
func externalGoFlags(flags string) bool {
	return strings.Contains(flags, "modfile") || strings.Contains(flags, "overlay")
}

// localReuse decides whether a record already answers for the current state. Every key must
// match; the head commit is deliberately not one of them (answer 4), because a rebase that keeps
// the tree is the same verification, and a different commit with the same tree is too. A record
// that did not pass, or that carries a pin mismatch, is never reused.
func localReuse(reused, current verificationRecord) (bool, string) {
	switch {
	case reused.Schema != recordSchema:
		return false, "the record is not " + recordSchema
	case reused.Result != localPass:
		return false, fmt.Sprintf("the record's result is %q, not %q", reused.Result, localPass)
	case externalGoFlags(reused.GoFlags) || externalGoFlags(current.GoFlags):
		return false, "GOFLAGS names an external modfile or overlay, whose contents the reuse keys do not cover"
	case len(reused.PinMismatch) > 0:
		return false, "the record carries a pin mismatch (" + strings.Join(reused.PinMismatch, ", ") + ")"
	}
	for _, key := range []struct {
		name       string
		was, isNow string
	}{
		{"tree", reused.TreeHash, current.TreeHash},
		{"ci.yml digest", reused.CiDigest, current.CiDigest},
		// The blob and secret steps judge base..head, so a different base is a different
		// verification even when the tree is the same (the range a record covers).
		{"base commit", reused.BaseCommit, current.BaseCommit},
		{"commit range", reused.Range, current.Range},
		{"os", reused.OS, current.OS},
		{"arch", reused.Arch, current.Arch},
	} {
		if key.was != key.isNow {
			return false, fmt.Sprintf("the %s changed (%s -> %s)", key.name, key.was, key.isNow)
		}
	}
	for _, name := range sortedKeys(current.Tools) {
		if was := reused.Tools[name]; was != current.Tools[name] {
			return false, fmt.Sprintf("the %s version changed (%s -> %s)", name, was, current.Tools[name])
		}
	}
	for _, name := range sortedKeys(reused.Tools) {
		if _, ok := current.Tools[name]; !ok {
			return false, "the " + name + " version is no longer known"
		}
	}
	for _, name := range sortedKeys(current.Dependencies) {
		if was := reused.Dependencies[name]; was != current.Dependencies[name] {
			return false, fmt.Sprintf("the %s digest changed (%s -> %s)", name, was, current.Dependencies[name])
		}
	}
	// The inherited Go settings decide what the Go steps actually build, so a run with different
	// ones is not the same verification.
	for _, key := range []struct {
		name       string
		was, isNow string
	}{
		{"GOFLAGS", reused.GoFlags, current.GoFlags},
		{"GOENV", reused.GoEnv, current.GoEnv},
	} {
		if key.was != key.isNow {
			return false, fmt.Sprintf("%s changed (%q -> %q)", key.name, key.was, key.isNow)
		}
	}
	return true, "every key matches"
}

// localHostOS and localHostArch are the platform the record names.
func localHostOS() string   { return runtime.GOOS }
func localHostArch() string { return runtime.GOARCH }

// localSortedUnique is items without blanks or duplicates, sorted.
func localSortedUnique(items []string) []string {
	out := slices.DeleteFunc(slices.Clone(items), func(s string) bool { return s == "" })
	slices.Sort(out)
	return slices.Compact(out)
}
