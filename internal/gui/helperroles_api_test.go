package gui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// helperRolesHost isolates one test: HOME, CODEX_HOME, CRW_HOME and XDG_STATE_HOME all point into
// a temporary directory, so the handler reads and writes a throwaway subagents.json and never the
// operator's store. It returns the CRW_HOME the store lives under.
func helperRolesHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	crwHome := filepath.Join(root, "crw")
	t.Setenv("CRW_HOME", crwHome)
	return crwHome
}

// helperRolesServer builds a Server over the package-wide registry, so the real registered routes
// are exercised rather than a hand-built table.
func helperRolesServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "test-version"})
	if err != nil {
		t.Fatalf("New over the package registry: %v", err)
	}
	return server
}

// helperRolesGet drives one read of the route and returns the decoded body.
func helperRolesGet(t *testing.T, server *Server, target string) map[string]any {
	t.Helper()
	recorder := request(server, http.MethodGet, target, guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s body %q: %v", target, recorder.Body.String(), err)
	}
	return body
}

// helperRolesPost drives one write of the route and returns the status and the decoded body.
func helperRolesPost(t *testing.T, server *Server, payload string) (int, map[string]any) {
	t.Helper()
	recorder := request(server, http.MethodPost, "/api/helper-roles", guardHost, payload, writeHeaders())
	var body map[string]any
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("POST body %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, body
}

// helperRoleNames is the four roles the store holds, in the order the store encodes them.
var helperRoleNames = []string{"explorer", "reviewer", "executor", "architect"}

// roleEntry reads one role's object out of a decoded settings answer.
func roleEntry(t *testing.T, body map[string]any, section, role string) map[string]any {
	t.Helper()
	group, ok := body[section].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in %v", section, body)
	}
	entry, ok := group[role].(map[string]any)
	if !ok {
		t.Fatalf("%s.%s is missing in %v", section, role, group)
	}
	return entry
}

// storeFile is the one file the route may write.
func storeFile(crwHome string) string { return filepath.Join(crwHome, "subagents.json") }

// storeListing names every file under the CRW home, so a test can prove no GUI-only settings file
// appeared beside the store.
func storeListing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// decodeStore reads the store file back as raw JSON.
func decodeStore(t *testing.T, crwHome string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(storeFile(crwHome))
	if err != nil {
		t.Fatalf("the store was not written: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("store %q: %v", raw, err)
	}
	return stored
}

// storedRole reads one role out of the store file.
func storedRole(t *testing.T, crwHome, role string) map[string]any {
	t.Helper()
	stored := decodeStore(t, crwHome)
	roles, ok := stored["roles"].(map[string]any)
	if !ok {
		t.Fatalf("the store holds no roles object: %v", stored)
	}
	entry, ok := roles[role].(map[string]any)
	if !ok {
		t.Fatalf("the store holds no %s role: %v", role, roles)
	}
	return entry
}

// R1: a read of an empty store answers the four roles at global scope, every one of them inheriting
// from the session; a write changes the store and a following read agrees.
func TestHelperRolesRoundTripWritesTheStore(t *testing.T) {
	crwHome := helperRolesHost(t)
	server := helperRolesServer(t)

	body := helperRolesGet(t, server, "/api/helper-roles")
	if body["scope"] != "global" {
		t.Fatalf("scope = %v, want global", body["scope"])
	}
	sources, _ := body["sources"].(map[string]any)
	overrides, _ := body["overrides"].(map[string]any)
	for _, role := range helperRoleNames {
		config := roleEntry(t, body, "roles", role)
		if config["mode"] != "default" || config["model"] != nil || config["effort"] != nil || config["promptOverride"] != nil || config["fallback"] != nil {
			t.Fatalf("%s on an empty store = %v, want the default role", role, config)
		}
		if sources[role] != "session" {
			t.Fatalf("sources.%s = %v, want session", role, sources[role])
		}
		if overrides[role] != false {
			t.Fatalf("overrides.%s = %v, want false", role, overrides[role])
		}
	}
	if _, err := os.Stat(storeFile(crwHome)); !os.IsNotExist(err) {
		t.Fatalf("a read created the store: %v", err)
	}

	payload := `{"role":"explorer","mode":"model","model":"gpt-5.5","effort":"high","promptOverride":"be brief","fallback":{"model":"gpt-5.4","effort":"low"}}`
	if code, _ := helperRolesPost(t, server, payload); code != http.StatusOK {
		t.Fatalf("the write answered %d", code)
	}
	if _, err := os.Stat(storeFile(crwHome)); err != nil {
		t.Fatalf("the write did not create the store: %v", err)
	}
	after := helperRolesGet(t, server, "/api/helper-roles")
	explorer := roleEntry(t, after, "roles", "explorer")
	if explorer["mode"] != "model" || explorer["model"] != "gpt-5.5" || explorer["effort"] != "high" || explorer["promptOverride"] != "be brief" {
		t.Fatalf("explorer after the write = %v", explorer)
	}
	fallback, _ := explorer["fallback"].(map[string]any)
	if fallback["model"] != "gpt-5.4" || fallback["effort"] != "low" {
		t.Fatalf("fallback after the write = %v", fallback)
	}
	if afterSources, _ := after["sources"].(map[string]any); afterSources["explorer"] != "global" {
		t.Fatalf("explorer source after the write = %v, want global", afterSources["explorer"])
	}
	if afterOverrides, _ := after["overrides"].(map[string]any); afterOverrides["explorer"] != true {
		t.Fatalf("explorer override after the write = %v, want true", afterOverrides["explorer"])
	}
	// A member the write did not name keeps its value: the store merges rather than replaces.
	if reviewer := roleEntry(t, after, "roles", "reviewer"); reviewer["mode"] != "default" {
		t.Fatalf("reviewer was disturbed by the explorer write: %v", reviewer)
	}
	// No GUI-only settings file appears beside the store.
	if got := storeListing(t, crwHome); len(got) != 1 || got[0] != "subagents.json" {
		t.Fatalf("CRW_HOME holds %v, want only subagents.json", got)
	}
}

// R2: the store is global only, so a project-shaped request is refused with the role package's own
// message, on the query and in the body alike. A present-but-empty scope is refused too, and the
// explicit global scope is accepted.
func TestHelperRolesRefusesProjectScope(t *testing.T) {
	helperRolesHost(t)
	server := helperRolesServer(t)
	const want = `invalid scope "project"`

	for _, target := range []string{"/api/helper-roles?scope=project", "/api/helper-roles?scope=", "/api/helper-roles?scope=Global"} {
		recorder := request(server, http.MethodGet, target, guardHost, "", nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s answered %d, want 400 (%s)", target, recorder.Code, recorder.Body.String())
		}
		if target == "/api/helper-roles?scope=project" {
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("refusal body %q: %v", recorder.Body.String(), err)
			}
			if body["error"] != want {
				t.Fatalf("refusal error = %v, want %q", body["error"], want)
			}
		}
	}
	if body := helperRolesGet(t, server, "/api/helper-roles?scope=global"); body["scope"] != "global" {
		t.Fatalf("an explicit global scope answered %v", body["scope"])
	}

	recorder := request(server, http.MethodPost, "/api/helper-roles", guardHost, `{"role":"explorer","scope":"project","effort":"high"}`, writeHeaders())
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a project-scoped write answered %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	var refusal map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("refusal body %q: %v", recorder.Body.String(), err)
	}
	if refusal["error"] != want {
		t.Fatalf("a project-scoped write refused with %v, want %q", refusal["error"], want)
	}
}

// R3: an effort the store does not accept is refused by the store, with the store's own message.
// This test is about the store's refusal only; it is not evidence about what the screen offers.
func TestHelperRolesRefusesAnInvalidEffort(t *testing.T) {
	helperRolesHost(t)
	server := helperRolesServer(t)
	recorder := request(server, http.MethodPost, "/api/helper-roles", guardHost, `{"role":"explorer","mode":"model","model":"gpt-5.5","effort":"max"}`, writeHeaders())
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unaccepted effort answered %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal body %q: %v", recorder.Body.String(), err)
	}
	const want = `invalid effort "max" (must be one of low/medium/high/xhigh or null)`
	if body["error"] != want {
		t.Fatalf("refusal error = %v, want %q", body["error"], want)
	}
}

// R4: the write route is not reachable without the per-run token the guard requires.
func TestHelperRolesWriteNeedsTheToken(t *testing.T) {
	helperRolesHost(t)
	server := helperRolesServer(t)
	recorder := request(server, http.MethodPost, "/api/helper-roles", guardHost, `{"role":"explorer","effort":"high"}`, map[string]string{"Content-Type": "application/json"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a write without the token answered %d, want 403 (%s)", recorder.Code, recorder.Body.String())
	}
}

// R5 (Go half): the prompt override distinguishes null (inherit) from the empty string. The Node
// test pins the wire bodies; this pins what the store then holds.
func TestHelperRolesKeepsNullAndEmptyPromptApart(t *testing.T) {
	crwHome := helperRolesHost(t)
	server := helperRolesServer(t)

	if code, _ := helperRolesPost(t, server, `{"role":"reviewer","promptOverride":""}`); code != http.StatusOK {
		t.Fatalf("the empty-string write answered %d", code)
	}
	reviewer := storedRole(t, crwHome, "reviewer")
	if value, present := reviewer["promptOverride"]; !present || value != "" {
		t.Fatalf("the empty string was not stored as the empty string: %v", reviewer)
	}

	if code, _ := helperRolesPost(t, server, `{"role":"executor","promptOverride":null}`); code != http.StatusOK {
		t.Fatalf("the null write answered %d", code)
	}
	executor := storedRole(t, crwHome, "executor")
	value, present := executor["promptOverride"]
	if !present || value != nil {
		t.Fatalf("null was not stored as null: %v", executor)
	}
	if reviewer = storedRole(t, crwHome, "reviewer"); reviewer["promptOverride"] != "" {
		t.Fatalf("the null write disturbed the empty string: %v", reviewer)
	}
}

// R6: the route is registered for both methods, which is what makes the screen able to read and
// write at all.
func TestHelperRolesRouteIsRegistered(t *testing.T) {
	var get, post bool
	for _, route := range Routes() {
		if route.Path != "/api/helper-roles" {
			continue
		}
		switch route.Method {
		case http.MethodGet:
			get = true
		case http.MethodPost:
			post = true
		}
	}
	if !get || !post {
		t.Fatalf("GET registered = %v, POST registered = %v", get, post)
	}
}
