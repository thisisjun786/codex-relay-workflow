package role

import (
	"bytes"
	"encoding/json"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// This is mcp.ts:35-155 from CXC v0.2.40, as a library. It implements no
// JSON-RPC transport and registers nothing. Definitions retain the oracle's
// project scope, while the existing settings API only supports global scope
// (decision 7). Callers should encode results with Stringify, without adding
// structuredContent or reformatting the text payload.

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPTools returns independent definitions in the oracle's get/set/list order.
func MCPTools() []MCPTool {
	return []MCPTool{
		{Name: "subagents_get", Description: "Read the per-role subagent config (explorer/reviewer/executor/architect): mode, model, effort, promptOverride, source and scope. Project defaults to global, then original session.", InputSchema: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string","enum":["project","global"]}},"additionalProperties":false}`)},
		{Name: "subagents_set", Description: "Update one role's subagent config. mode is 'default' (main model) or 'model' (requires a model id); effort is a reasoning-effort override (null inherits the parent session's effort).", InputSchema: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string","enum":["project","global"]},"inherit":{"type":"boolean","description":"Remove this entire role override and inherit the next scope; do not combine with role settings."},"role":{"type":"string","enum":["explorer","reviewer","executor","architect"]},"mode":{"type":"string","enum":["default","model"]},"model":{"type":["string","null"]},"effort":{"type":["string","null"],"enum":["low","medium","high","xhigh",null]},"fallback":{"type":["object","null"],"description":"Optional first fallback. Null clears; omitted nested effort inherits existing fallback effort or session effort.","properties":{"model":{"type":"string","minLength":1},"effort":{"type":["string","null"],"enum":["low","medium","high","xhigh",null]}},"additionalProperties":false},"promptOverride":{"type":["string","null"]}},"required":["role"],"additionalProperties":false}`)},
		{Name: "catalog_list", Description: "List selectable models: Codex-native entries first, then ocx-backed entries when ocx is active.", InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	}
}

type MCPToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPToolResult is the result member, without a JSON-RPC id or envelope.
// Success omits isError; errors use the same JSON-encoded text content.
type MCPToolResult struct {
	Content []MCPToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

func mcpToolResult(payload any) (MCPToolResult, error) {
	b, err := Stringify(payload, "")
	if err != nil {
		return MCPToolResult{}, err
	}
	return MCPToolResult{Content: []MCPToolContent{{Type: "text", Text: string(b)}}}, nil
}

func mcpToolError(message string) (MCPToolResult, error) {
	r, err := mcpToolResult(ErrorBody{Error: message})
	r.IsError = true
	return r, err
}

const mcpToolNativeMaxAge = 24 * time.Hour
const mcpToolProbeTimeout = 5 * time.Second

// CatalogIsAuthoritative is mcp.ts:84-93. fetchedAt is not an authority signal:
// fresh OCX is authoritative; every other source checks the native file's mtime.
func CatalogIsAuthoritative(c LiveCatalog, now time.Time, env host.LookupEnv) bool {
	if c.Status != "fresh" {
		return false
	}
	if c.Source == ModelOcx {
		return true
	}
	p := NativeCatalogPath(env)
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	age := now.Sub(info.ModTime())
	return age >= 0 && age <= mcpToolNativeMaxAge
}

type MCPSpawnArgs struct {
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type MCPDecoratedRole struct {
	RoleConfig
	SpawnArgs   MCPSpawnArgs `json:"spawnArgs"`
	StaleModel  *bool        `json:"staleModel"`
	StaleReason string       `json:"staleReason,omitempty"`
}

type MCPDecoratedSettings struct {
	Roles     RoleMap[MCPDecoratedRole] `json:"roles"`
	Scope     ConfigScope               `json:"scope"`
	Sources   RoleMap[ConfigSource]     `json:"sources"`
	Overrides RoleMap[bool]             `json:"overrides"`
}

// DecorateSubagentsGet preserves settings order and the null/true/false
// staleModel distinction. A nil catalog means the probe timed out.
func DecorateSubagentsGet(s Settings, c *LiveCatalog, now time.Time, env host.LookupEnv) MCPDecoratedSettings {
	authoritative := c != nil && CatalogIsAuthoritative(*c, now, env)
	roles := RoleMap[MCPDecoratedRole]{}
	for _, name := range Roles() {
		cfg := s.Roles[name]
		r := MCPDecoratedRole{RoleConfig: cfg}
		modelMode := cfg.Mode == ModeModel && cfg.Model != nil && *cfg.Model != ""
		if modelMode {
			r.SpawnArgs.Model = *cfg.Model
		}
		if cfg.Effort != nil {
			r.SpawnArgs.ReasoningEffort = string(*cfg.Effort)
		}
		if modelMode && authoritative {
			missing := true
			for _, entry := range c.Entries {
				if entry.ID == *cfg.Model {
					missing = false
					break
				}
			}
			r.StaleModel = &missing
		} else {
			switch {
			case cfg.Mode != ModeModel:
				r.StaleReason = "role uses the default model"
			case c == nil:
				r.StaleReason = "catalog read timed out after 5000 ms"
			case c.Status == "stale":
				r.StaleReason = "catalog is last-success cache"
			default:
				r.StaleReason = "catalog unavailable"
			}
		}
		roles[name] = r
	}
	return MCPDecoratedSettings{roles, s.Scope, s.Sources, s.Overrides}
}

// MCPToolHandler shares the pending catalog reads of a server. Its zero value
// uses the current process environment. Options.Environ selects an explicit
// environment for all settings and catalog operations. Reader can be shared
// across handlers; nil uses this handler's own reader. Do not copy a used handler.
// ReadCatalog replaces only the get probe, as in the oracle; list always uses
// the live reader. Discovery errors on get are folded into an unavailable
// catalog. A list error escapes to the caller (the oracle sends no reply).
type MCPToolHandler struct {
	Options       CatalogOptions
	Reader        *CatalogReader
	ReadCatalog   func(CatalogOptions) (LiveCatalog, error)
	mcpToolReader CatalogReader
}

// HandleToolCall consumes the parsed params JSON, without an id or method.
// Arguments missing/null become {}; schema metadata adds no extra validation.
// All writes go through UpdateSettings and its lock/atomic publication.
func (h *MCPToolHandler) HandleToolCall(params json.RawMessage) (MCPToolResult, error) {
	if params == nil {
		params = json.RawMessage(`{}`)
	}
	var parsed json.RawMessage
	if err := json.Unmarshal(params, &parsed); err != nil {
		return MCPToolResult{}, err
	}
	fields, _ := members(parsed)
	name, _ := stringOf(fields["name"])
	args := fields["arguments"]
	if args == nil || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		args = json.RawMessage(`{}`)
	}
	o := h.Options
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
	env := catalogEnv(o.Environ)
	r := h.Reader
	if r == nil {
		r = &h.mcpToolReader
	}
	switch name {
	case "subagents_set":
		s, err := UpdateSettings(env, args)
		if err != nil {
			return mcpToolError(err.Error())
		}
		return mcpToolResult(s)
	case "subagents_get":
		argFields, _ := members(args)
		s, err := GetSettings(env, argFields["scope"])
		if err != nil {
			return mcpToolError(err.Error())
		}
		o.ForceRefresh = true
		read := h.ReadCatalog
		if read == nil {
			read = r.ReadCatalog
		}
		c := mcpToolProbe(read, o)
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		return mcpToolResult(DecorateSubagentsGet(s, c, now(), env))
	case "catalog_list":
		o.ForceRefresh = false
		c, err := r.ReadCatalog(o)
		if err != nil {
			return MCPToolResult{}, err
		}
		return mcpToolResult(c)
	default:
		n, err := jsString(fields["name"])
		if err != nil {
			return MCPToolResult{}, err
		}
		return mcpToolError("unknown tool: " + n)
	}
}

func mcpToolProbe(read func(CatalogOptions) (LiveCatalog, error), o CatalogOptions) *LiveCatalog {
	ready := make(chan LiveCatalog, 1)
	go func() {
		c, err := read(o)
		if err != nil {
			c = LiveCatalog{Catalog: Catalog{State: CatalogUnavailable, Entries: []CatalogEntry{}}, Status: "unavailable", Source: ModelOcx}
		}
		ready <- c
	}()
	timer := time.NewTimer(mcpToolProbeTimeout)
	defer timer.Stop()
	select {
	case c := <-ready:
		return &c
	case <-timer.C:
		return nil
	}
	// The buffered channel lets a late read finish. As in Promise.race, it is
	// not cancelled and can still publish a successful catalog cache.
}
