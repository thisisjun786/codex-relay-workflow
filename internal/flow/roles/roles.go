// Package roles builds the prompt of an engine thread: the CXC role definition (internal/role/agents, bytes
// unchanged) followed by the overlay of this package (<role>.md), which says what differs when the role runs as a
// CRW flow engine call: the thread's own cwd, the commit rule, the output schema, the skills an executor reads and
// the native spawn limit that applies in place of the definition's LEAF sentence.
package roles

import (
	"embed"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/thisisjun786/codex-relay-workflow/internal/role/agents"
)

//go:embed *.md
var overlays embed.FS

// Names are the six roles of an engine call, the critic (CRW-1434) last. reviewer-final is a retired role key (CRW-1450): no
// engine call runs as it.
var Names = [...]string{"architect", "executor", "executor-t1", "reviewer", "explorer", "critic"}

// definition is the role definition file a role's thread is built on: the second tier of the executor uses the definition of
// its family, and the critic, which has no definition file of its own, the reviewer's.
var definition = map[string]string{
	"architect": "architect", "executor": "executor", "executor-t1": "executor",
	"reviewer": "reviewer", "explorer": "explorer",
	"critic": "reviewer",
}

// Options select the variant of a call.
type Options struct {
	// QA is the QA capture call of a final gate (SP-QA-1): an executor call that drives a surface in a scratch
	// worktree and starts no helper.
	QA bool
}

// NativeHelpers is how many native helpers a call of the role may spawn, as its overlay states: none for an explorer call, a
// critic call and the QA capture call, one explorer helper for every other call. The flow binding gives a call's line this
// allowance, so the spawn hook enforces what the overlay tells the thread.
func NativeHelpers(role string, opts Options) int {
	if opts.QA || role == "explorer" || role == "critic" {
		return 0
	}
	return 1
}

// Separator joins the role definition and the overlay.
const Separator = "\n\n"

// LeafSentence is the opening of the sentence of a role definition that forbids native spawns; an overlay names it
// and says what applies instead.
const LeafSentence = "You are a LEAF agent: do NOT spawn sub-agents"

// Base is the developer instructions of the role definition the role's thread is built on, as the file states them.
func Base(role string) (string, error) {
	file, ok := definition[role]
	if !ok {
		return "", fmt.Errorf("roles: %q is not a role of an engine call; the roles are %s", role, strings.Join(Names[:], ", "))
	}
	raw, err := agents.FS.ReadFile(file + ".toml")
	if err != nil {
		return "", err
	}
	var parsed struct {
		DeveloperInstructions string `toml:"developer_instructions"`
	}
	if _, err := toml.Decode(string(raw), &parsed); err != nil {
		return "", fmt.Errorf("roles: %s.toml: %w", file, err)
	}
	if parsed.DeveloperInstructions == "" {
		return "", fmt.Errorf("roles: %s.toml has no developer_instructions", file)
	}
	return parsed.DeveloperInstructions, nil
}

// Overlay is the overlay text of the role (the QA variant for a QA call).
func Overlay(role string, opts Options) (string, error) {
	if _, ok := definition[role]; !ok {
		return "", fmt.Errorf("roles: %q is not a role of an engine call; the roles are %s", role, strings.Join(Names[:], ", "))
	}
	name := role
	if opts.QA {
		if role != "executor" {
			return "", fmt.Errorf("roles: the QA capture call is an executor call, not %q", role)
		}
		name = "executor-qa"
	}
	raw, err := overlays.ReadFile(name + ".md")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Prompt is the role definition followed by the overlay: the instructions of the engine thread.
func Prompt(role string, opts Options) (string, error) {
	base, err := Base(role)
	if err != nil {
		return "", err
	}
	overlay, err := Overlay(role, opts)
	if err != nil {
		return "", err
	}
	return base + Separator + overlay, nil
}
