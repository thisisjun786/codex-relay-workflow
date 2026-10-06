package gui

import (
	"net/http"
)

// healthHandler answers GET /api/health with the running version, which is the one route this
// issue registers. Its file is also the pattern a later issue copies: one file, one init(),
// one Register call, and no edit to any shared table.
func healthHandler(env *Env, r *http.Request) (Response, error) {
	return Response{Status: http.StatusOK, Body: map[string]any{"ok": true, "version": env.Version}}, nil
}

func init() {
	Register(http.MethodGet, "/api/health", healthHandler)
}
