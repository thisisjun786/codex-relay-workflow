//go:build dev

package cxcfuzz

import (
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// RootEnv is the environment one case runs under: the case's own root and the homes under it, so
// a target that reads HOME, CODEX_HOME, CRW_HOME or TMPDIR reaches the case's tree and never a
// real one. A target reads them from here; the harness itself sets nothing in its own process.
func RootEnv(root string) Env {
	return Env{
		Root:      root,
		Home:      filepath.Join(root, "home"),
		CodexHome: filepath.Join(root, "codex-home"),
		CrwHome:   filepath.Join(root, "crw-home"),
		TmpDir:    filepath.Join(root, "tmp"),
	}
}

// ReplaceRoot rewrites one case's root in an output as ${ROOT}, so the same relative behaviour in
// two different roots compares equal.
func ReplaceRoot(out, root string) string {
	if root == "" {
		return out
	}
	return strings.ReplaceAll(out, root, "${ROOT}")
}

// stripRoot applies ReplaceRoot to every string inside a value, before the two answers compare.
func stripRoot(value any, root string) any {
	switch v := value.(type) {
	case string:
		return ReplaceRoot(v, root)
	case pyjson.Object:
		out := make(pyjson.Object, 0, len(v))
		for _, item := range v {
			out = append(out, pyjson.Field{Key: item.Key, Value: stripRoot(item.Value, root)})
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, stripRoot(item, root))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = stripRoot(item, root)
		}
		return out
	default:
		return value
	}
}
