// Package projectcfg is the project's crw.json: whether a plan request opens with the Interview and
// whether the PABCD hooks run. It is the Go form of CXC v0.2.40 pabcd-state/src/interview-policy.ts,
// with the file renamed from codexclaw.json (port decision 1). The reader and the writer live
// together so the CLI cannot drift from what a hook reads on every prompt.
package projectcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ConfigFilename is where the setting lives: committed, at the repo root.
const ConfigFilename = "crw.json"

// PabcdEnv overrides the file's pabcd.enabled (CODEXCLAW_PABCD in CXC).
const PabcdEnv = "CRW_PABCD"

// Policy is when a plan request opens with the Interview.
type Policy string

// The three policies, in the order the oracle lists them.
const (
	PolicyOff     Policy = "off"
	PolicyNewUnit Policy = "new-unit"
	PolicyAlways  Policy = "always"
)

// DefaultPolicy promotes on the first plan request of a unit, and not once a cycle is running.
const DefaultPolicy = PolicyNewUnit

// IsPolicy is whether value is exactly one of the three policies.
func IsPolicy(value string) bool {
	switch Policy(value) {
	case PolicyOff, PolicyNewUnit, PolicyAlways:
		return true
	}
	return false
}

// ConfigPath is the project's crw.json.
func ConfigPath(cwd string) string { return filepath.Join(cwd, ConfigFilename) }

// PabcdEnabled is readPabcdEnabled: a recognized CRW_PABCD value (after trim and lower-casing)
// overrides the project file, which otherwise enables PABCD unless pabcd.enabled is false. Any other
// state of the file enables it. The caller passes os.Getenv; an unset and an empty variable read alike.
func PabcdEnabled(cwd string, env func(key string) string) bool {
	switch strings.ToLower(text.Trim(env(PabcdEnv))) {
	case "off", "0", "false":
		return false
	case "on", "1", "true":
		return true
	}
	config, _ := readObject(ConfigPath(cwd))
	pabcd, _ := config.get("pabcd").(object)
	return pabcd.get("enabled") != false
}

// ReadPolicy is readInterviewPolicy. A missing or unreadable file, malformed JSON and an unknown
// value all fall back to the default: a hook must never fail on a prompt.
func ReadPolicy(cwd string) Policy {
	config, _ := readObject(ConfigPath(cwd))
	if value, _ := config.get("interview").(string); IsPolicy(value) {
		return Policy(value)
	}
	return DefaultPolicy
}

// readObject is the file's top-level object; ok is false when it is unreadable or not a JSON object. The readers use it:
// a hook must never fail on a prompt, so every failure is the default. The writer tells the failures apart.
func readObject(path string) (members object, ok bool) {
	data, _ := os.ReadFile(path)
	return parseObject(data)
}

// parseObject is the top-level object of a document; ok is false when it does not parse or is not a JSON object.
func parseObject(data []byte) (members object, ok bool) {
	value, parsed := parse(data)
	members, isObject := value.(object)
	return members, parsed && isObject
}

// WriteResult is what WritePolicy did.
type WriteResult struct {
	Path string
	// ReplacedMalformed: the file existed but was not a JSON object, so it was replaced.
	ReplacedMalformed bool
}

// WritePolicy is writeInterviewPolicy: it sets "interview" and keeps every other member, rewriting
// the file as JSON.stringify(value, null, 2) plus a newline would. A file that was read and is not a JSON
// object is replaced, not merged, and the caller is told. Two departures from the oracle, both data-loss
// fixes (CRW-427): the file is published by rename (crwdir.Publish), so a concurrent reader, or a process that
// dies part-way, leaves the old file or the new one whole, where the oracle truncates and rewrites it in place; and a file that
// exists but cannot be read (mode 0200, say) is refused with a reason and left as it is, where the oracle
// takes it for malformed and rewrites it with only "interview". Reading and publishing are not one locked
// step: a file changed in between is not noticed, as in the oracle.
func WritePolicy(cwd string, policy Policy) (WriteResult, error) {
	path := ConfigPath(cwd)
	data, err := os.ReadFile(path)
	existed := !errors.Is(err, fs.ErrNotExist)
	if err != nil && existed {
		return WriteResult{}, fmt.Errorf("could not read %s (left unchanged): %w", path, err)
	}
	members, ok := parseObject(data)
	if !ok {
		members = nil // an existing file that is not an object is replaced, not merged
	}
	document := stringify(members.set("interview", string(policy)), "") + "\n"
	if err := crwdir.Publish(path, []byte(document)); err != nil {
		return WriteResult{}, fmt.Errorf("could not write %s: %w", path, err)
	}
	return WriteResult{Path: path, ReplacedMalformed: existed && !ok}, nil
}

// EntryInput is what decides whether a plan request is advised to open with the Interview.
type EntryInput struct {
	// Trigger is the raw trigger the prompt matched: "I", "P", "A", "B", "C", "D", or "" for none.
	Trigger string
	Policy  Policy
	// OrchestrationActive: a PABCD cycle is already running.
	OrchestrationActive bool
	// GoalSuppresses: goal mode owns the turn (an active or unreadable goal state suppresses the Interview).
	GoalSuppresses bool
}

// EntryDecision: Phase is the raw trigger, always; AdviseInterview only chooses the directive text.
type EntryDecision struct {
	Phase           string
	AdviseInterview bool
}

// DecideEntry is decideInterviewEntry: only P is promoted, never under goal mode or "off", and under
// "new-unit" never in a running cycle.
func DecideEntry(in EntryInput) EntryDecision {
	if in.Trigger != "P" || in.GoalSuppresses || in.Policy == PolicyOff || in.Policy == PolicyNewUnit && in.OrchestrationActive {
		return EntryDecision{Phase: in.Trigger}
	}
	return EntryDecision{Phase: in.Trigger, AdviseInterview: true}
}
