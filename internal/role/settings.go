package role

import (
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// Response is the stub of the red step.
type Response struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

type ErrorBody struct {
	Error string `json:"error"`
}

func GetSettings(env host.LookupEnv, scope json.RawMessage) (Settings, error) {
	return Settings{}, nil
}

func UpdateSettings(env host.LookupEnv, body json.RawMessage) (Settings, error) {
	return Settings{}, nil
}

func SettingsResponse(op func() (Settings, error)) Response { return Response{} }
