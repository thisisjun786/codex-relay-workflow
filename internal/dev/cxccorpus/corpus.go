// Package cxccorpus records and checks the CXC v0.2.40 behaviour corpus (CRW-279): fixtures
// under contract/fixtures/cxc that later Go port issues replay through the name-substitution
// table in contract/schema/cxc. The recorder drives the real Node build of CXC v0.2.40 (the
// oracle) in an isolated temporary root, so it shells out to node: it is development tooling,
// built only with -tags dev, and never part of crw. The lint half needs no oracle and runs in
// CI (`crw-dev ci contracts`). What a replay shares with the recorder (the fixture types, the
// normaliser, the rename table, the declarations and the case engine) carries no build tag, so a
// Go test can run a build against the corpus through the same code.
package cxccorpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The corpus layout, relative to the repository root.
const (
	SchemaDir    = "contract/schema/cxc"
	SpecDir      = SchemaDir + "/specs"
	FixtureDir   = "contract/fixtures/cxc"
	Normalise    = SchemaDir + "/normalisation.json"
	Substitution = SchemaDir + "/name-substitution.json"
	Coverage     = SchemaDir + "/coverage.json"
	Declarations = SchemaDir + "/hook-declarations.json"
)

// Oracle identifies the build every fixture was recorded from.
const (
	OracleTag    = "v0.2.40"
	OracleCommit = "3c1459acadeb1906d97c00a598e1457327ae372d"
)

// Given is a scenario's starting state. Every path is relative to the case root and starts
// with one of the roots in Roots; text may carry the placeholders of Placeholders.
type Given struct {
	Files    map[string]string          `json:"files,omitempty"`
	JSON     map[string]json.RawMessage `json:"json,omitempty"`
	Dirs     []string                   `json:"dirs,omitempty"`
	Modes    map[string]int             `json:"modes,omitempty"`
	Symlinks map[string]string          `json:"symlinks,omitempty"`
	Env      map[string]string          `json:"env,omitempty"`
	Stubs    map[string]*Stub           `json:"stubs,omitempty"`
	Git      *Git                       `json:"git,omitempty"`
	SQLite   map[string][]string        `json:"sqlite,omitempty"`
	// Mtimes overrides the frozen-clock mtime of given entries (RFC 3339), for staleness cases.
	Mtimes map[string]string `json:"mtimes,omitempty"`
	// Fetch scripts the closed network: URL to reply.
	Fetch map[string]FetchReply `json:"fetch,omitempty"`
}

// Stub scripts one fake external program on PATH. A nil stub removes the default one, so the
// program is absent.
type Stub struct {
	Stdout string     `json:"stdout,omitempty"`
	Stderr string     `json:"stderr,omitempty"`
	Exit   int        `json:"exit"`
	Cases  []StubCase `json:"cases,omitempty"`
}

// StubCase answers an invocation whose argv starts with Argv; the first matching case wins, and
// an invocation no case matches gets the stub's own answer.
type StubCase struct {
	Argv   []string `json:"argv"`
	Stdout string   `json:"stdout,omitempty"`
	Stderr string   `json:"stderr,omitempty"`
	Exit   int      `json:"exit"`
}

// FetchReply is a scripted answer to one URL the oracle fetches; unscripted URLs fail as offline.
type FetchReply struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Git makes a directory a repository (Dir, a case path, default "ws"): init, then (when Commit
// is set) one commit of every file under it, with the fixed author and dates of the recorder's
// env, then each linked worktree (a case path on a new branch).
type Git struct {
	Dir       string        `json:"dir,omitempty"`
	Commit    string        `json:"commit,omitempty"`
	Origin    string        `json:"origin,omitempty"`
	Worktrees []GitWorktree `json:"worktrees,omitempty"`
}

// GitWorktree is one `git worktree add -b Branch Path`.
type GitWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// Step is one invocation. Exactly one of Hook, CLI, Node, MCP or Write is set.
//   - Hook names a registered leg (hook-declarations.json) and runs its declared command.
//   - CLI runs the repository dispatcher bin/codexclaw.mjs (the `cxc` the README installs);
//     Payload selects the plugin's own bin/cxc.mjs instead.
//   - Node runs a file under the oracle root with arguments (component entrypoints).
//   - MCP starts the declared MCP server and writes each request as one line.
//   - Write is not an oracle invocation: the recorder writes those files (case paths, text with
//     placeholders) between invocations, standing in for an agent's edit. Its result is
//     recorded with action "write".
type Step struct {
	Hook    string            `json:"hook,omitempty"`
	CLI     []string          `json:"cli,omitempty"`
	Payload bool              `json:"payload,omitempty"`
	Node    []string          `json:"node,omitempty"`
	MCP     []json.RawMessage `json:"mcp,omitempty"`
	Write   map[string]string `json:"write,omitempty"`
	Stdin   json.RawMessage   `json:"stdin,omitempty"`
	// StdinPad appends that many spaces to stdin: an oversized payload that is still one JSON
	// document, without storing megabytes in the spec.
	StdinPad int               `json:"stdin_pad,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Unset    []string          `json:"unset,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
}

// Scenario is one spec entry: what to set up and what to run. Covers names the contract items
// (coverage.json) it records evidence for.
type Scenario struct {
	ID      string   `json:"id"`
	Covers  []string `json:"covers"`
	Note    string   `json:"note,omitempty"`
	Given   Given    `json:"given"`
	Steps   []Step   `json:"steps"`
	Observe []string `json:"observe,omitempty"`
}

// SpecFile is contract/schema/cxc/specs/<group>.json.
type SpecFile struct {
	Group     string     `json:"group"`
	Scenarios []Scenario `json:"scenarios"`
}

// StepResult is what one step did, normalised.
type StepResult struct {
	Action      string            `json:"action,omitempty"`
	Exit        int               `json:"exit"`
	Signal      string            `json:"signal,omitempty"`
	Timeout     bool              `json:"timeout,omitempty"`
	StdoutForm  string            `json:"stdout_form"`
	StdoutJSON  json.RawMessage   `json:"stdout_json,omitempty"`
	StdoutJSONL []json.RawMessage `json:"stdout_jsonl,omitempty"`
	Stdout      *string           `json:"stdout,omitempty"`
	Stderr      string            `json:"stderr"`
}

// Entry is one observed path after the run.
type Entry struct {
	Type   string            `json:"type"`
	Mode   string            `json:"mode"`
	Form   string            `json:"form,omitempty"`
	JSON   json.RawMessage   `json:"json,omitempty"`
	JSONL  []json.RawMessage `json:"jsonl,omitempty"`
	Text   *string           `json:"text,omitempty"`
	SHA256 string            `json:"sha256,omitempty"`
	Size   *int64            `json:"size,omitempty"`
	Target string            `json:"target,omitempty"`
}

// Call is one invocation of a stubbed external program.
type Call struct {
	Cmd  string   `json:"cmd"`
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}

// Expect is the recording.
type Expect struct {
	Exit  int              `json:"exit"`
	Steps []StepResult     `json:"steps"`
	Tree  map[string]Entry `json:"tree"`
	Calls []Call           `json:"calls"`
}

// RunBlock is the fixture's run block: the spec's steps verbatim under the corpus run kind.
type RunBlock struct {
	Kind    string   `json:"kind"`
	Steps   []Step   `json:"steps"`
	Observe []string `json:"observe,omitempty"`
}

// Fixture is contract/fixtures/cxc/<id>.json.
type Fixture struct {
	Oracle string   `json:"oracle"`
	Covers []string `json:"covers"`
	Note   string   `json:"note,omitempty"`
	Given  Given    `json:"given"`
	Run    RunBlock `json:"run"`
	Expect Expect   `json:"expect"`
}

// RunKind is the fixtures' run.kind. The Go contract runner has no runner for it: the cxc
// domain is gated in internal/contracttest until the Go port implements CXC.
const RunKind = "cxc"

// Roots are the case-root directories a scenario may address, in the order they are observed.
var Roots = []string{"ws", "codex", "cxc", "home", "tmp"}

// DefaultObserve is every root except the hook-observation diagnostics, which every hook
// invocation writes (scripts/hook-observation.mjs); a scenario that wants them names
// HookObservations in observe.
var DefaultObserve = []string{"ws", "codex", "cxc", "home", "tmp"}

// HookObservations is the diagnostic store excluded from DefaultObserve.
const HookObservations = "codex/codexclaw/hook-observations"

// LoadSpecs reads every spec file, sorted by name, and refuses duplicate scenario IDs.
func LoadSpecs(root string) ([]SpecFile, error) {
	paths, err := filepath.Glob(filepath.Join(root, SpecDir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var specs []SpecFile
	seen := map[string]string{}
	for _, path := range paths {
		var spec SpecFile
		if err := readStrict(path, &spec); err != nil {
			return nil, err
		}
		for _, s := range spec.Scenarios {
			if prior, ok := seen[s.ID]; ok {
				return nil, fmt.Errorf("%s: scenario %q is also in %s", path, s.ID, prior)
			}
			seen[s.ID] = path
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// LoadFixture reads one recorded fixture.
func LoadFixture(path string) (Fixture, error) {
	var f Fixture
	err := readStrict(path, &f)
	return f, err
}

func readStrict(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if decoder.More() {
		return fmt.Errorf("%s: trailing data after the JSON value", path)
	}
	return nil
}

// Marshal writes v as the corpus spells JSON: two-space indent, no HTML escaping, newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
