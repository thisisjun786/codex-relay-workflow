package gui

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file is the helper-role settings route: GET /api/helper-roles reads the effective
// settings and POST /api/helper-roles changes one role, both over the internal/role store at
// CRW_HOME/subagents.json. It is the helper-role screen's API, and it is deliberately separate
// from the execution policy route in policy_api.go: that file serves the supervisor, parent and
// child pairs, this one serves the explorer, reviewer, executor and architect helper roles, and
// neither reuses the other's names or wording.
//
// The route is a thin passthrough. role.GetSettings and role.UpdateSettings already answer the
// status and the message the issue requires (a 400 carrying the store's own error text for every
// failure, including an unreadable store), so nothing here decodes the request into a second Go
// type and re-encodes an answer: a copy of those semantics would drift from the store's. For the
// same reason neither handler returns a non-nil error; that path is the server's 500.
//
// Two consequences worth naming rather than leaving to be discovered. First, role.Settings is
// served through encoding/json, which escapes <, >, & and U+2028/U+2029 as a backslash-u form.
// That is lossless for a JSON client (JSON.parse restores the exact string) and this response is
// only ever read by fetch().json(), never embedded in HTML or script; serving the store's own
// bytes would need a raw-body lane the server's dispatch does not have. Second, the store
// serializes its writers with a lock file and refuses the loser of a race, so two concurrent
// writes surface as a 400 with the lock's message rather than as a silent lost update; that is
// the store's behaviour and this route reports it as it is.

// helperRolesScope is the optional "scope" member of a read, as the raw JSON the store's scope
// check reads. An absent key is nil, which the store takes as the global default. A key that is
// present is carried even when its value is empty, so ?scope= is refused rather than silently
// answered with global data; only the store decides which values are acceptable.
func helperRolesScope(r *http.Request) (json.RawMessage, error) {
	values, present := r.URL.Query()["scope"]
	if !present {
		return nil, nil
	}
	value := ""
	if len(values) > 0 {
		value = values[0]
	}
	return json.Marshal(value)
}

// helperRolesGetHandler answers GET /api/helper-roles[?scope=global] with the effective settings:
// the four roles, the scope, and each role's source and override flag.
func helperRolesGetHandler(_ *Env, r *http.Request) (Response, error) {
	scope, err := helperRolesScope(r)
	if err != nil {
		return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
	}
	response := role.SettingsResponse(func() (role.Settings, error) {
		return role.GetSettings(envLookup, scope)
	})
	return Response{Status: response.Status, Body: response.Body}, nil
}

// helperRolesPostHandler answers POST /api/helper-roles with the settings after the write. The body
// is the store's own patch shape ({role, mode?, model?, effort?, promptOverride?, fallback?,
// inherit?}), and it is handed to the store as the bytes it arrived as: the guard has already read
// it up to the write limit and put it back on the request, so this reads the buffered copy.
func helperRolesPostHandler(_ *Env, r *http.Request) (Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
	}
	response := role.SettingsResponse(func() (role.Settings, error) {
		return role.UpdateSettings(envLookup, json.RawMessage(body))
	})
	return Response{Status: response.Status, Body: response.Body}, nil
}

func init() {
	Register(http.MethodGet, "/api/helper-roles", helperRolesGetHandler)
	Register(http.MethodPost, "/api/helper-roles", helperRolesPostHandler)
}
