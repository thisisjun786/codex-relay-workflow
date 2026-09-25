package settings

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	Untransmittable     = "setting_untransmittable"
	NotPreserved        = "settings_not_preserved"
	Unobservable        = "setting_unobservable"
	UnsupportedApproval = "unsupported_approval_policy"
	UnsupportedSandbox  = "unsupported_sandbox_type"
)

var ErrInvalid = errors.New("invalid settings")

// invalid is a settings.py ValueError: its text is exactly Python's, and it matches ErrInvalid.
type invalid string

func (e invalid) Error() string        { return string(e) }
func (e invalid) Is(target error) bool { return target == ErrInvalid }

// UntransmittableError is settings.py UntransmittableSetting: the protocol cannot carry the
// request at all, decided before any RPC.
type UntransmittableError struct {
	Field     string
	Requested any
	Detail    string
}

func (e *UntransmittableError) Error() string { return Untransmittable + ": " + e.Detail }

// Code is the refusal code Python carries as UntransmittableSetting.code.
func (e *UntransmittableError) Code() string { return Untransmittable }

type Finding struct {
	Code     string `json:"code"`
	Field    string `json:"field"`
	Expected any    `json:"expected"`
	Returned any    `json:"returned"`
}
type Contract struct {
	CWD, Sandbox, Model, ReasoningEffort, ApprovalPolicy string
	Roots                                                []string
	ExpectedPolicy                                       map[string]any
}

var defaults = map[string]map[string]any{
	"readOnly": {"networkAccess": false}, "externalSandbox": {"networkAccess": "restricted"}, "dangerFullAccess": {},
	"workspaceWrite": {"writableRoots": []any{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false},
}
var modes = map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}

func Normalise(policy any) map[string]any {
	source, ok := policy.(map[string]any)
	if !ok {
		return nil
	}
	kind, ok := source["type"].(string)
	if !ok {
		return nil
	}
	out := map[string]any{}
	for k, v := range defaults[kind] {
		out[k] = v
	}
	for k, v := range source {
		if k != "type" {
			out[k] = v
		}
	}
	out["type"] = kind
	if roots, present := out["writableRoots"]; present {
		if _, ok := roots.([]any); !ok {
			return nil
		}
	}
	return out
}
func (c Contract) Validate() error {
	approval := c.ApprovalPolicy
	if approval != "" && approval != "never" && approval != "on-request" && approval != "untrusted" {
		return invalid("approval_policy must be one of ['never', 'on-request', 'untrusted']; a granular policy has no name a caller can declare")
	}
	if c.Sandbox != "" && modes[c.Sandbox] == "" {
		return invalid("Unsupported sandbox " + Repr(c.Sandbox))
	}
	if c.ExpectedPolicy != nil {
		if err := c.validatePolicy(); err != nil {
			return err
		}
	}
	for _, f := range [...][2]string{{"cwd", c.CWD}, {"model", c.Model}, {"reasoning_effort", c.ReasoningEffort}} {
		if f[1] != "" && strings.TrimSpace(f[1]) == "" {
			return invalid(f[0] + " must be a non-empty string when supplied")
		}
	}
	for _, root := range c.Roots {
		if !filepath.IsAbs(root) {
			return invalid("runtime_workspace_roots must be absolute paths")
		}
	}
	return nil
}

// transmittable lists, per sandbox type, the policy fields the config object can carry.
var transmittable = map[string]map[string]string{"workspaceWrite": {"writableRoots": "writable_roots", "networkAccess": "network_access", "excludeTmpdirEnvVar": "exclude_tmpdir_env_var", "excludeSlashTmp": "exclude_slash_tmp"}}

func (c Contract) validatePolicy() error {
	p := Normalise(c.ExpectedPolicy)
	if p == nil {
		return invalid("expected_sandbox_policy must be an object carrying a type")
	}
	kind := p["type"].(string)
	if kind != "readOnly" && kind != "workspaceWrite" && kind != "dangerFullAccess" {
		return &UntransmittableError{"sandbox", p, fmt.Sprintf("%q has no sandbox mode this bridge can send", kind)}
	}
	if c.Sandbox != "" && modes[c.Sandbox] != kind {
		return invalid("sandbox and expected_sandbox_policy.type must agree")
	}
	fields := make([]string, 0, len(p))
	for k := range p {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	for _, k := range fields {
		if _, mapped := transmittable[kind][k]; k == "type" || mapped {
			continue
		}
		if !reflect.DeepEqual(p[k], defaults[kind][k]) {
			return &UntransmittableError{"sandbox." + k, p[k], fmt.Sprintf("%s %s cannot be carried by thread/start or thread/resume", kind, k)}
		}
	}
	return nil
}
func (c Contract) approval() string {
	if c.ApprovalPolicy == "" {
		return "never"
	}
	return c.ApprovalPolicy
}
func (c Contract) Requested() map[string]any {
	asked := map[string]any{}
	if c.CWD != "" {
		asked["cwd"] = c.CWD
	}
	if c.Model != "" {
		asked["model"] = c.Model
	}
	if c.ReasoningEffort != "" {
		asked["reasoningEffort"] = c.ReasoningEffort
	}
	if c.Roots != nil {
		asked["runtimeWorkspaceRoots"] = roots(c.Roots)
	}
	if c.ExpectedPolicy != nil {
		asked["sandbox"] = Normalise(c.ExpectedPolicy)
	} else if c.Sandbox != "" {
		asked["sandbox"] = map[string]any{"type": modes[c.Sandbox]}
	}
	return asked
}

// Config carries what the typed parameters cannot: the effort, and every transmittable field of
// the normalised policy, defaults included, so a host configured the other way is overridden.
func (c Contract) Config() map[string]any {
	config := map[string]any{}
	if c.ReasoningEffort != "" {
		config["model_reasoning_effort"] = c.ReasoningEffort
	}
	if policy := Normalise(c.ExpectedPolicy); policy != nil {
		fields := map[string]any{}
		for k, name := range transmittable[policy["type"].(string)] {
			if v, ok := policy[k]; ok {
				fields[name] = v
			}
		}
		if len(fields) > 0 {
			config["sandbox_workspace_write"] = fields
		}
	}
	return config
}

// roots renders the roots as the JSON array a host echoes, so the two compare equal.
func roots(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
func (c Contract) StartParams() map[string]any {
	p := map[string]any{}
	mode := c.Sandbox
	if c.ExpectedPolicy != nil {
		for k, v := range modes {
			if v == c.ExpectedPolicy["type"] {
				mode = k
			}
		}
	}
	if mode != "" {
		p["sandbox"] = mode
	}
	if c.Model != "" {
		p["model"] = c.Model
	}
	if c.Roots != nil {
		p["runtimeWorkspaceRoots"] = roots(c.Roots)
	}
	if config := c.Config(); len(config) > 0 {
		p["config"] = config
	}
	return p
}
func (c Contract) ResumeParams(id string) map[string]any {
	p := map[string]any{"threadId": id, "excludeTurns": true}
	if len(c.Requested()) == 0 {
		return p
	}
	for k, v := range c.StartParams() {
		p[k] = v
	}
	if c.CWD != "" {
		p["cwd"] = c.CWD
	}
	return p
}
