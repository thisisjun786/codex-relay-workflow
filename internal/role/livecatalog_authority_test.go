package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CRW-1132: the authority of a native catalog is judged on the file the discovery read, not on the file
// the configuration names when the judgement is made. A configuration that switches from a.json to
// b.json while the OCX runs must not approve a.json's entries by b.json's age, nor report b.json's
// models missing from a.json's list.
func TestMCPToolAuthorityFollowsTheDiscoveredNativeFile(t *testing.T) {
	for _, c := range []struct {
		name         string
		aAge, bAge   time.Duration
		model        string
		wantAuth     bool // the verdict on a.json, the file the discovery read
		wantStaleOut bool // the staleModel value when authoritative: the model is missing from a.json
	}{
		{"old-a-recent-b-model-a", 48 * time.Hour, 0, "model-a", false, false},
		{"old-a-recent-b-model-b", 48 * time.Hour, 0, "model-b", false, false},
		{"recent-a-old-b-model-a", 0, 48 * time.Hour, "model-a", true, false},
		{"recent-a-old-b-model-b", 0, 48 * time.Hour, "model-b", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := liveOptions(t)
			env := catalogEnv(o.Environ)
			codex, _ := env("CODEX_HOME")
			check(t, os.MkdirAll(codex, 0700))
			for name, age := range map[string]time.Duration{"a.json": c.aAge, "b.json": c.bAge} {
				p := filepath.Join(codex, name)
				id := "model-" + name[:1]
				check(t, os.WriteFile(p, []byte(`{"models":["`+id+`"]}`), 0600))
				at := o.Now().Add(-age)
				check(t, os.Chtimes(p, at, at))
			}
			config := filepath.Join(codex, "config.toml")
			check(t, os.WriteFile(config, []byte(`model_catalog_json = 'a.json'`), 0600))
			must(UpdateSettings(env, json.RawMessage(`{"role":"reviewer","mode":"model","model":"`+c.model+`"}`)))
			o.RunOcx = func([]string) (string, error) {
				check(t, os.WriteFile(config, []byte(`model_catalog_json = 'b.json'`), 0600))
				return "", os.ErrNotExist
			}
			h := MCPToolHandler{Options: o}
			r := mcpToolCall(t, &h, `{"name":"subagents_get"}`)
			var got MCPDecoratedSettings
			check(t, json.Unmarshal([]byte(r.Content[0].Text), &got))
			cfg := got.Roles[Reviewer]
			if !c.wantAuth {
				if cfg.StaleModel != nil {
					t.Fatalf("a.json is older than 24 hours and cannot prove routing, got staleModel=%v", *cfg.StaleModel)
				}
				return
			}
			if cfg.StaleModel == nil || *cfg.StaleModel != c.wantStaleOut {
				t.Fatalf("a.json is recent: want staleModel=%v, got %+v (reason %q)", c.wantStaleOut, cfg.StaleModel, cfg.StaleReason)
			}
		})
	}
}
