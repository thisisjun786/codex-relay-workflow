package role

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// The settings contract of CXC v0.2.40 subagent-config/src/settings-api.ts over the global store. The oracle's calls take a working
// directory for its project layer (removed by decision 7, I1) and a scope that defaults to "project": here an absent scope is global
// and every other value is refused. A scope or a body is raw JSON, so a value of the wrong type is refused with the oracle's message.

// Response is settingsResponse's answer: 200 with the settings, or 400 with an ErrorBody. Encode it with Stringify.
type Response struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

// ErrorBody is the body of a 400.
type ErrorBody struct {
	Error string `json:"error"`
}

// SettingsResponse runs a settings operation and reports its failure as a 400, every failure, an unreadable store included (the oracle
// does the same).
func SettingsResponse(op func() (Settings, error)) Response {
	s, err := op()
	if err != nil {
		return Response{Status: 400, Body: ErrorBody{Error: err.Error()}}
	}
	return Response{Status: 200, Body: s}
}

// GetSettings is getSettings: the effective settings for a scope (absent or "global").
func GetSettings(env host.LookupEnv, scope json.RawMessage) (Settings, error) {
	if err := checkScope(scope); err != nil {
		return Settings{}, err
	}
	return ReadSettings(env)
}

// checkScope is configScope over a JSON value: absent is the default scope, a string is judged by ParseScope, anything else is
// printed as String() prints it.
func checkScope(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	raw = bytes.TrimSpace(raw) // JSON.parse allows white space around a value
	if s, ok := stringOf(raw); ok {
		_, err := ParseScope(&s)
		return err
	}
	text, err := jsString(raw)
	if err != nil {
		return err
	}
	return fmt.Errorf("invalid scope \"%s\"", text)
}

// UpdateSettings is updateSettings: body names a role and either inherit: true (reset the role) or the members to change, and the answer
// is the settings after the write. Unknown members are ignored.
func UpdateSettings(env host.LookupEnv, body json.RawMessage) (Settings, error) {
	body = bytes.TrimSpace(body)
	b, ok := members(body)
	if !ok || len(body) == 0 || body[0] != '{' {
		return Settings{}, errors.New("missing body")
	}
	name, isString := stringOf(b["role"])
	if !isString || !validRole(RoleName(name)) {
		text, err := jsString(b["role"])
		if err != nil {
			return Settings{}, err
		}
		return Settings{}, fmt.Errorf("unknown role \"%s\"", text)
	}
	role := RoleName(name)
	if err := checkScope(b["scope"]); err != nil {
		return Settings{}, err
	}
	inherit := string(b["inherit"]) == "true"
	if raw, present := b["inherit"]; present && !inherit && string(raw) != "false" {
		return Settings{}, errors.New("inherit must be a boolean")
	}
	var patch RolePatch
	present := 0
	for key, dst := range map[string]json.Unmarshaler{"mode": &patch.Mode, "model": &patch.Model, "effort": &patch.Effort, "promptOverride": &patch.PromptOverride, "fallback": &patch.Fallback} {
		if raw, ok := b[key]; ok {
			_ = dst.UnmarshalJSON(raw) // an Opt never fails; FallbackPatch's refusal is kept by the Opt that holds it
			present++
		}
	}
	var err error
	switch {
	case inherit && present > 0:
		return Settings{}, errors.New("inherit cannot be combined with role settings")
	case inherit:
		_, err = ResetRole(env, role)
	default:
		_, err = SetRole(env, role, patch)
	}
	if err != nil {
		return Settings{}, err
	}
	return ReadSettings(env)
}
