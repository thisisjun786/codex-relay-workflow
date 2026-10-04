package role

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// These answers were recorded from mcp.ts in CXC v0.2.40, with global settings
// and isolated homes. Go tests never invoke Node or the MCP transport.
func TestMCPToolOracle(t *testing.T) {
	var fixture struct {
		Tools json.RawMessage `json:"tools"`
		Cases []struct {
			ID        string          `json:"id"`
			Catalog   json.RawMessage `json:"catalog"`
			NativeAge *int64          `json:"nativeAge"`
			Answers   []struct {
				Params json.RawMessage `json:"params"`
				Result *MCPToolResult  `json:"result"`
				Error  *string         `json:"error"`
				Store  *string         `json:"store"`
			} `json:"answers"`
		} `json:"cases"`
	}
	check(t, json.Unmarshal([]byte(readText(t, "testdata/mcptools/oracle.json")), &fixture))
	if len(fixture.Cases) != 13 {
		t.Fatalf("oracle cases: %d", len(fixture.Cases))
	}
	if got, want := string(must(Stringify(MCPTools(), ""))), string(must(Stringify(fixture.Tools, ""))); got != want {
		t.Fatalf("tool definitions differ:\n%s\nwant\n%s", got, want)
	}
	// A returned schema must not share mutable memory with the next call.
	first := MCPTools()
	first[0].InputSchema[0] = '!'
	if !json.Valid(MCPTools()[0].InputSchema) {
		t.Fatal("shared schema memory")
	}
	for _, c := range fixture.Cases {
		t.Run(c.ID, func(t *testing.T) {
			o := liveOptions(t)
			o.Now = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }
			if c.NativeAge != nil {
				p := filepath.Join(t.TempDir(), "models.json")
				check(t, os.WriteFile(p, []byte(`{"models":[]}`), 0600))
				at := o.Now().Add(-time.Duration(*c.NativeAge) * time.Millisecond)
				check(t, os.Chtimes(p, at, at))
				o.Environ = append(o.Environ, "CODEX_MODELS_CACHE_PATH="+p)
			}
			h := MCPToolHandler{Options: o, ReadCatalog: func(opt CatalogOptions) (LiveCatalog, error) {
				if !opt.ForceRefresh {
					t.Error("get did not force refresh")
				}
				if string(c.Catalog) == `"reject"` {
					return LiveCatalog{}, errors.New("catalog failed")
				}
				var catalog LiveCatalog
				check(t, json.Unmarshal(c.Catalog, &catalog))
				return catalog, nil
			}}
			for i, a := range c.Answers {
				got, err := h.HandleToolCall(a.Params)
				if a.Error != nil {
					if err == nil || err.Error() != *a.Error {
						t.Fatalf("answer %d: error %v, want %s", i, err, *a.Error)
					}
				} else {
					check(t, err)
					want := *a.Result
					if c.ID == "argument_shapes" && !want.IsError {
						// Intentionally changed: decision 7's existing settings API
						// defaults to global; retain every other recorded byte.
						body := must(parseObject([]byte(want.Content[0].Text)))
						body.set("scope", ScopeGlobal)
						want.Content[0].Text = string(must(Stringify(body, "")))
					}
					if g, w := string(must(Stringify(got, ""))), string(must(Stringify(want, ""))); g != w {
						t.Fatalf("answer %d (%s):\n%s\nwant\n%s", i, a.Params, g, w)
					}
				}
				store := must(StorePath(catalogEnv(o.Environ)))
				b, e := os.ReadFile(store)
				if (e == nil) != (a.Store != nil) || a.Store != nil && string(b) != *a.Store {
					t.Fatalf("answer %d store: %s, error %v", i, b, e)
				}
				if _, e := os.Stat(store + ".lock"); e == nil {
					t.Fatal("settings lock left behind")
				}
			}
		})
	}
}

func mcpToolCall(t *testing.T, h *MCPToolHandler, params string) MCPToolResult {
	t.Helper()
	r, err := h.HandleToolCall(json.RawMessage(params))
	check(t, err)
	return r
}

func TestMCPToolCatalogList(t *testing.T) {
	o := liveOptions(t)
	calls := 0
	o.ForceRefresh = true // list must override this and retain cache reuse.
	o.RunOcx = func([]string) (string, error) {
		calls++
		return `[{"namespaced":"provider/present","reasoningEfforts":["low","high"]}]`, nil
	}
	h := MCPToolHandler{Options: o, ReadCatalog: func(CatalogOptions) (LiveCatalog, error) {
		t.Error("list used get-only injection")
		return LiveCatalog{}, errors.New("unexpected")
	}}
	store := must(StorePath(catalogEnv(o.Environ)))
	for range 2 {
		r := mcpToolCall(t, &h, `{"name":"catalog_list"}`)
		want := `{"state":"ocx-active","entries":[{"id":"provider/present","source":"ocx","label":"provider/present","reasoningEfforts":["low","high"]}],"status":"fresh","source":"ocx","fetchedAt":"2026-01-01T00:00:00.000Z"}`
		if r.IsError || r.Content[0].Type != "text" || r.Content[0].Text != want {
			t.Fatalf("catalog answer: %+v", r)
		}
	}
	if calls != 1 {
		t.Fatalf("list bypassed TTL: %d discovery calls", calls)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatal("list wrote settings", err)
	}
	if _, err := os.Stat(livePath(o)); err != nil {
		t.Fatal("list did not write catalog cache", err)
	}
	for _, kind := range []string{"unavailable", "native"} {
		t.Run(kind, func(t *testing.T) {
			o := liveOptions(t)
			o.RunOcx = func([]string) (string, error) {
				if kind == "native" {
					return "", os.ErrNotExist
				}
				return "", errors.New("exits 127")
			}
			o.ReadNative = func(_ host.LookupEnv) []CatalogEntry { return []CatalogEntry{} }
			h := MCPToolHandler{Options: o}
			r := mcpToolCall(t, &h, `{"name":"catalog_list"}`)
			var c LiveCatalog
			check(t, json.Unmarshal([]byte(r.Content[0].Text), &c))
			if r.IsError || (kind == "native" && (c.Status != "fresh" || c.Source != ModelNative)) || (kind == "unavailable" && (c.Status != "unavailable" || c.Message == "")) {
				t.Fatalf("discovery %s: %+v", kind, c)
			}
		})
	}
}

func TestMCPToolAuthority(t *testing.T) {
	o := liveOptions(t)
	env := catalogEnv(o.Environ)
	p := NativeCatalogPath(env)
	check(t, os.MkdirAll(filepath.Dir(p), 0700))
	c := LiveCatalog{Status: "fresh", Source: ModelNative}
	if CatalogIsAuthoritative(c, o.Now(), env) {
		t.Fatal("missing native path authoritative")
	}
	check(t, os.WriteFile(p, []byte(`{"models":[]}`), 0600))
	for _, row := range []struct {
		age  time.Duration
		want bool
	}{{-time.Millisecond, false}, {0, true}, {24 * time.Hour, true}, {24*time.Hour + time.Millisecond, false}} {
		at := o.Now().Add(-row.age)
		check(t, os.Chtimes(p, at, at))
		if got := CatalogIsAuthoritative(c, o.Now(), env); got != row.want {
			t.Errorf("age %s: %v", row.age, got)
		}
	}
	c.Source = ModelOcx
	if !CatalogIsAuthoritative(c, o.Now(), env) {
		t.Fatal("fresh ocx should be authoritative")
	}
	for _, status := range []string{"stale", "unavailable"} {
		c.Status = status
		if CatalogIsAuthoritative(c, o.Now(), env) {
			t.Fatal(status, "authoritative")
		}
	}
}

func TestMCPToolTimeout(t *testing.T) {
	o := liveOptions(t)
	must(UpdateSettings(catalogEnv(o.Environ), json.RawMessage(`{"role":"reviewer","mode":"model","model":"provider/model"}`)))
	synctest.Test(t, func(t *testing.T) {
		defer func() { time.Sleep(2 * time.Second); synctest.Wait() }()
		late := false
		h := MCPToolHandler{Options: o, ReadCatalog: func(CatalogOptions) (LiveCatalog, error) {
			time.Sleep(6 * time.Second)
			late = true
			return LiveCatalog{}, nil
		}}
		started := time.Now()
		r := mcpToolCall(t, &h, `{"name":"subagents_get"}`)
		if elapsed := time.Since(started); elapsed != 5*time.Second {
			t.Fatalf("probe duration: %s", elapsed)
		}
		var got MCPDecoratedSettings
		check(t, json.Unmarshal([]byte(r.Content[0].Text), &got))
		cfg := got.Roles[Reviewer]
		if cfg.StaleModel != nil || cfg.StaleReason != "catalog read timed out after 5000 ms" || late {
			t.Fatalf("timeout: %+v, late=%v", cfg, late)
		}
		var fixture struct {
			Timeout MCPDecoratedSettings `json:"timeout"`
		}
		check(t, json.Unmarshal([]byte(readText(t, "testdata/mcptools/oracle.json")), &fixture))
		if g, w := string(must(Stringify(got, ""))), string(must(Stringify(fixture.Timeout, ""))); g != w {
			t.Fatalf("timeout oracle:\n%s\nwant\n%s", g, w)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !late {
			t.Fatal("late discovery did not complete")
		}
	})
}

func TestMCPToolRefusalsKeepStore(t *testing.T) {
	o := liveOptions(t)
	h := MCPToolHandler{Options: o, ReadCatalog: func(CatalogOptions) (LiveCatalog, error) {
		t.Error("discovery after refusal/set")
		return LiveCatalog{}, nil
	}}
	mcpToolCall(t, &h, `{"name":"subagents_set","arguments":{"role":"reviewer","mode":"model","model":"safe"}}`)
	p := must(StorePath(catalogEnv(o.Environ)))
	before := readText(t, p)
	for _, params := range []string{`{"name":"subagents_set","arguments":{"role":"reviewer","effort":"bogus"}}`, `{"name":"subagents_get","arguments":{"scope":"project"}}`} {
		if !mcpToolCall(t, &h, params).IsError || readText(t, p) != before {
			t.Fatal("refusal changed store")
		}
	}
	check(t, os.WriteFile(p, []byte("{"), 0600))
	r := mcpToolCall(t, &h, `{"name":"subagents_set","arguments":{"role":"reviewer","effort":"low"}}`)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "cannot update subagent config") || readText(t, p) != "{" {
		t.Fatal("malformed store not preserved", r)
	}
}

func TestMCPToolLiveGetJoinsLateDiscovery(t *testing.T) {
	o := liveOptions(t)
	must(UpdateSettings(catalogEnv(o.Environ), json.RawMessage(`{"role":"reviewer","mode":"model","model":"present","effort":"xhigh"}`)))
	synctest.Test(t, func(t *testing.T) {
		defer func() { time.Sleep(7 * time.Second); synctest.Wait() }()
		calls := 0
		o.RunOcx = func([]string) (string, error) {
			calls++
			time.Sleep(6 * time.Second)
			return `[{"namespaced":"present"}]`, nil
		}
		h := MCPToolHandler{Options: o}
		first := mcpToolCall(t, &h, `{"name":"subagents_get"}`)
		if !strings.Contains(first.Content[0].Text, "catalog read timed out after 5000 ms") {
			t.Fatal("first probe did not time out")
		}
		second := mcpToolCall(t, &h, `{"name":"subagents_get"}`)
		var s MCPDecoratedSettings
		check(t, json.Unmarshal([]byte(second.Content[0].Text), &s))
		r := s.Roles[Reviewer]
		if calls != 1 || r.StaleModel == nil || *r.StaleModel || r.SpawnArgs.ReasoningEffort != "xhigh" {
			t.Fatalf("joined probe: calls=%d role=%+v", calls, r)
		}
		// Even a successful cache does not suppress a get's forced refresh.
		mcpToolCall(t, &h, `{"name":"subagents_get"}`)
		if calls != 2 {
			t.Fatal("get reused fresh cache", calls)
		}
	})
}
